package forwardproxy

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

func TestDefaultSpecialUseIPRanges(t *testing.T) {
	h := Handler{}
	for _, cidr := range defaultDeniedIPRanges {
		rule, err := newACLRule(cidr, false)
		if err != nil {
			t.Fatalf("invalid default CIDR %q: %v", cidr, err)
		}
		h.aclRules = append(h.aclRules, rule)
	}
	h.aclRules = append(h.aclRules, &aclAllRule{allow: true})

	tests := []struct {
		address string
		allow   bool
	}{
		{"0.0.0.1", false},
		{"10.0.0.1", false},
		{"100.100.100.100", false},
		{"127.0.0.1", false},
		{"169.254.169.254", false},
		{"172.16.0.1", false},
		{"192.0.0.8", false},
		{"192.0.2.1", false},
		{"192.88.99.1", false},
		{"192.168.1.1", false},
		{"198.18.0.1", false},
		{"198.51.100.1", false},
		{"203.0.113.1", false},
		{"224.0.0.1", false},
		{"240.0.0.1", false},
		{"255.255.255.255", false},
		{"::", false},
		{"::1", false},
		{"64:ff9b::a9fe:a9fe", false},
		{"100::1", false},
		{"2001::1", false},
		{"2001:db8::1", false},
		{"2002:c0a8:0101::1", false},
		{"fc00::1", false},
		{"fd12::1", false},
		{"fe80::1", false},
		{"ff02::1", false},
		{"::ffff:169.254.169.254", false},
		{"1.1.1.1", true},
		{"8.8.8.8", true},
		{"2606:4700:4700::1111", true},
		{"2001:4860:4860::8888", true},
	}
	for _, tt := range tests {
		t.Run(tt.address, func(t *testing.T) {
			ip := net.ParseIP(tt.address)
			if ip == nil {
				t.Fatal("invalid test IP")
			}
			if got := h.hostIsAllowed(tt.address, ip); got != tt.allow {
				t.Fatalf("address %s allowed=%t; want %t", tt.address, got, tt.allow)
			}
		})
	}

	// Explicit user exceptions must continue to take precedence.
	rule, err := newACLRule("169.254.169.254", true)
	if err != nil {
		t.Fatal(err)
	}
	h.aclRules = append([]aclRule{rule}, h.aclRules...)
	if !h.hostIsAllowed("169.254.169.254", net.ParseIP("169.254.169.254")) {
		t.Fatal("explicit allow rule was overridden by defaults")
	}
}

func TestDefaultSpecialUseIPBlockedViaProxy(t *testing.T) {
	for _, address := range []string{"169.254.169.254", "100.64.0.1", "fc00::1"} {
		t.Run(address, func(t *testing.T) {
			resp, err := getViaProxy(net.JoinHostPort(address, "80"), "/", caddyForwardProxyNoBlacklistOverride.addr, "HTTP/1.1", "", true)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("request to %s returned %d; want 403", address, resp.StatusCode)
			}
		})
	}
}

// ControlContext checks the real destination IP after native DNS selection,
// so a domain cannot bypass IP/CIDR restrictions by resolving to a private IP.
func TestControlContextAppliesOrderedDomainAndIPACL(t *testing.T) {
	h := uotTestHandler()
	loopback, err := newACLRule("127.0.0.0/8", false)
	if err != nil {
		t.Fatal(err)
	}
	h.aclRules = append([]aclRule{loopback}, h.aclRules...)
	ctx := context.WithValue(context.Background(), aclHostKey{}, "example.org")

	if err := h.controlDestination(ctx, "tcp4", "127.0.0.1:443", nil); !errors.Is(err, errACLDenied) {
		t.Fatalf("resolved loopback address was not blocked: %v", err)
	}
	if err := h.controlDestination(ctx, "udp4", "8.8.8.8:53", nil); err != nil {
		t.Fatalf("public destination rejected: %v", err)
	}
	if err := h.controlDestination(context.Background(), "tcp4", "8.8.8.8:443", nil); !errors.Is(err, errACLDenied) {
		t.Fatalf("missing ACL context did not fail closed: %v", err)
	}

	// Explicit domain allow before IP deny overrides the CIDR restriction,
	// matching the original ordered ACL semantics.
	allowDomain, err := newACLRule("example.org", true)
	if err != nil {
		t.Fatal(err)
	}
	h.aclRules = append([]aclRule{allowDomain}, h.aclRules...)
	if err := h.controlDestination(ctx, "tcp4", "127.0.0.1:443", nil); err != nil {
		t.Fatalf("ordered domain allow did not take precedence: %v", err)
	}

	// Domain denial also applies even to a permitted public destination.
	denyDomain, err := newACLRule("example.org", false)
	if err != nil {
		t.Fatal(err)
	}
	h.aclRules = append([]aclRule{denyDomain}, h.aclRules...)
	if err := h.controlDestination(ctx, "tcp4", "8.8.8.8:443", nil); !errors.Is(err, errACLDenied) {
		t.Fatalf("explicit domain denial was bypassed: %v", err)
	}

	linkLocal, err := newACLRule("fe80::/10", false)
	if err != nil {
		t.Fatal(err)
	}
	h.aclRules = []aclRule{linkLocal, &aclAllRule{allow: true}}
	if err := h.controlDestination(context.WithValue(context.Background(), aclHostKey{}, "example.org"), "tcp6", "[fe80::1%eth0]:443", nil); !errors.Is(err, errACLDenied) {
		t.Fatalf("scoped IPv6 address bypassed CIDR restriction: %v", err)
	}
}

// A rejected DNS candidate must not prevent the standard Dialer from
// falling back to another family (IPv6 refused, then permitted IPv4).
func TestNativeDialerACLAndIPv4Fallback(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	h := uotTestHandler()
	denyIPv4, err := newACLRule("127.0.0.0/8", false)
	if err != nil {
		t.Fatal(err)
	}
	denyIPv6, err := newACLRule("::1/128", false)
	if err != nil {
		t.Fatal(err)
	}
	h.aclRules = []aclRule{denyIPv4, denyIPv6, &aclAllRule{allow: true}}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	address := net.JoinHostPort("localhost", port)
	conn, err := h.dialContextCheckACL(ctx, "tcp", address)
	if conn != nil {
		_ = conn.Close()
		t.Fatal("forbidden loopback connection unexpectedly succeeded")
	}
	var handlerErr caddyhttp.HandlerError
	if !errors.As(err, &handlerErr) || handlerErr.StatusCode != http.StatusForbidden {
		t.Fatalf("DNS-selected loopback addresses should return 403; got %v", err)
	}

	h.aclRules = []aclRule{denyIPv6, &aclAllRule{allow: true}}
	conn, err = h.dialContextCheckACL(ctx, "tcp", address)
	if err != nil {
		t.Fatalf("allowed IPv4 fallback failed: %v", err)
	}
	_ = conn.Close()

	h.AllowedPorts = []int{443}
	if conn, err := h.dialContextCheckACL(ctx, "tcp", address); conn != nil || err == nil {
		if conn != nil { _ = conn.Close() }
		t.Fatalf("port restriction ignored: %v", err)
	}
}

func TestHTTP1ConnectNaivePadding(t *testing.T) {
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		for {
			conn, err := echo.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()

	h := uotTestHandler()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(context.WithValue(r.Context(), caddy.ReplacerCtxKey, caddy.NewReplacer()))
		if err := h.ServeHTTP(w, r, nil); err != nil {
			t.Errorf("forward proxy: %v", err)
		}
	}))
	defer server.Close()

	for _, padded := range []bool{true, false} {
		t.Run(fmt.Sprintf("padded=%t", padded), func(t *testing.T) {
			conn, err := net.Dial("tcp", server.Listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
				t.Fatal(err)
			}

			payload := []byte("http1-naive-first-frame")
			frame := append([]byte(nil), payload...)
			headers := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\nProxy-Authorization: %s\r\n", echo.Addr(), echo.Addr(), credentialsCorrect)
			if padded {
				headers += "Padding: naive-test\r\n"
				frame = make([]byte, 3+len(payload)+3)
				binary.BigEndian.PutUint16(frame[:2], uint16(len(payload)))
				frame[2] = 3
				copy(frame[3:], payload)
				copy(frame[3+len(payload):], "pad")
			}
			headers += "\r\n"
			// The first tunnel frame is intentionally sent together with CONNECT:
			// net/http may have buffered it by the time Hijack is called.
			if _, err := conn.Write(append([]byte(headers), frame...)); err != nil {
				t.Fatal(err)
			}
			reader := bufio.NewReader(conn)
			resp, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("CONNECT returned %s", resp.Status)
			}
			if resp.Header.Get("Padding") == "" {
				t.Fatal("missing Naive response padding header")
			}

			var got []byte
			if padded {
				header := make([]byte, 3)
				if _, err := io.ReadFull(reader, header); err != nil {
					t.Fatal(err)
				}
				size := int(binary.BigEndian.Uint16(header[:2]))
				got = make([]byte, size)
				if _, err := io.ReadFull(reader, got); err != nil {
					t.Fatal(err)
				}
				if _, err := io.CopyN(io.Discard, reader, int64(header[2])); err != nil {
					t.Fatal(err)
				}
			} else {
				got = make([]byte, len(payload))
				if _, err := io.ReadFull(reader, got); err != nil {
					t.Fatal(err)
				}
			}
			if !bytes.Equal(got, payload) {
				t.Fatalf("echo mismatch: got %q, want %q", got, payload)
			}
		})
	}
}
