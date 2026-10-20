package forwardproxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	M "github.com/sagernet/sing/common/metadata"
	"github.com/sagernet/sing/common/uot"
)

func uotTestHandler() *Handler {
	return &Handler{
		HideIP:          true,
		AuthCredentials: [][]byte{EncodeAuthCredentials("test", "pass")},
		aclRules:        []aclRule{&aclAllRule{allow: true}},
		dialContext:     (&net.Dialer{Timeout: time.Second}).DialContext,
	}
}

func uotEchoServer(t testing.TB, address string) net.Addr {
	t.Helper()
	conn, err := net.ListenPacket("udp", address)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	go func() {
		buffer := make([]byte, 65535)
		for {
			n, addr, err := conn.ReadFrom(buffer)
			if err != nil {
				return
			}
			_, _ = conn.WriteTo(buffer[:n], addr)
		}
	}()
	return conn.LocalAddr()
}

// Use actual HTTP/2 CONNECT and Naive's padding, with sing's independent
// client implementation. More than eight packets exercise the padding cutoff.
func TestUOTHTTP2(t *testing.T) {
	destination := M.SocksaddrFromNet(uotEchoServer(t, "127.0.0.1:0"))
	other := M.SocksaddrFromNet(uotEchoServer(t, "127.0.0.1:0"))
	for _, version := range []uint8{1, 2} {
		for _, connect := range []bool{false, true} {
			if version == 1 && connect {
				continue
			}
			for _, padded := range []bool{false, true} {
				t.Run(fmt.Sprintf("v%d/connect=%t/padding=%t", version, connect, padded), func(t *testing.T) {
					h := uotTestHandler()
					server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						r = r.WithContext(context.WithValue(r.Context(), caddy.ReplacerCtxKey, caddy.NewReplacer()))
						if r.ProtoMajor != 2 {
							t.Error("expected HTTP/2")
						}
						_ = h.ServeHTTP(w, r, nil)
					}))
					server.EnableHTTP2 = true
					server.StartTLS()
					defer server.Close()
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					input, output := io.Pipe()
					defer input.Close()
					defer output.Close()
					magic := uot.RequestDestination(version).String()
					req, err := http.NewRequestWithContext(ctx, http.MethodConnect, server.URL, input)
					if err != nil {
						t.Fatal(err)
					}
					req.Host = magic
					req.Header.Set("Proxy-Authorization", credentialsCorrect)
					if padded {
						req.Header.Set("Padding", "test-padding")
					}
					resp, err := server.Client().Do(req)
					if err != nil {
						t.Fatal(err)
					}
					defer resp.Body.Close()
					if resp.StatusCode != 200 || resp.Header.Get("Padding") == "" {
						t.Fatalf("CONNECT: %s, headers %v", resp.Status, resp.Header)
					}
					client, bridge := net.Pipe()
					defer client.Close()
					defer bridge.Close()
					_ = client.SetDeadline(time.Now().Add(5 * time.Second))
					add, remove := NoPadding, NoPadding
					if padded {
						add, remove = AddPadding, RemovePadding
					}
					go func() {
						_, _ = flushingIoCopy(output, bridge, make([]byte, 65536), add)
						_ = output.Close()
					}()
					go func() {
						_, _ = flushingIoCopy(bridge, resp.Body, make([]byte, 65536), remove)
						_ = bridge.Close()
					}()
					packets, err := (&uot.Client{Version: version}).DialConn(client, connect, destination)
					if err != nil {
						t.Fatal(err)
					}
					for i := 0; i < 12; i++ {
						target := destination
						if !connect && i%2 != 0 {
							target = other
						}
						payload := bytes.Repeat([]byte{byte(i)}, i*101)
						if i == 11 {
							payload = bytes.Repeat([]byte{byte(i)}, 65000)
						}
						if _, err := packets.WriteTo(payload, target); err != nil {
							t.Fatal(err)
						}
						buffer := make([]byte, 65535)
						n, _, err := packets.ReadFrom(buffer)
						if err != nil {
							t.Fatal(err)
						}
						if !bytes.Equal(buffer[:n], payload) {
							t.Fatalf("packet %d changed", i)
						}
					}
				})
			}
		}
	}
}

func TestUOTDestinationAddresses(t *testing.T) {
	for _, host := range []string{"127.0.0.1", "::1", "localhost"} {
		t.Run(host, func(t *testing.T) {
			listenHost := host
			if host == "localhost" {
				listenHost = ""
			}
			// IPv6 is optional on Windows and some CI hosts.
			probe, err := net.ListenPacket("udp", net.JoinHostPort(listenHost, "0"))
			if err != nil {
				t.Skipf("UDP address unavailable: %v", err)
			}
			_ = probe.Close()
			addr := uotEchoServer(t, net.JoinHostPort(listenHost, "0"))
			_, port, _ := net.SplitHostPort(addr.String())
			destination := M.ParseSocksaddr(net.JoinHostPort(host, port))
			conn := uotTestHandler().dialUOT(context.Background(), uot.Version)
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
			packets, err := (&uot.Client{Version: 2}).DialConn(conn, false, destination)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := packets.WriteTo([]byte("address test"), destination); err != nil {
				t.Fatal(err)
			}
			buffer := make([]byte, 128)
			n, _, err := packets.ReadFrom(buffer)
			if err != nil {
				t.Fatal(err)
			}
			if string(buffer[:n]) != "address test" {
				t.Fatal("UDP payload changed")
			}
		})
	}
}

func TestUOTUpstream(t *testing.T) {
	h := uotTestHandler()
	h.upstream = &url.URL{Scheme: "https", Host: "upstream.example"}
	conn, peer := net.Pipe()
	defer conn.Close()
	defer peer.Close()
	h.dialContext = func(_ context.Context, network, address string) (net.Conn, error) {
		if network != "tcp" || address != uot.MagicAddress+":0" {
			t.Fatalf("changed upstream destination: %s %s", network, address)
		}
		return conn, nil
	}
	got, err := h.dialContextCheckACL(context.Background(), "tcp", uot.MagicAddress+":0")
	if err != nil || got != conn {
		t.Fatalf("UoT was not delegated to upstream: %v", err)
	}
}

func TestUOTMultiTargetPolicyAndSourcePort(t *testing.T) {
	first := M.SocksaddrFromNet(uotEchoServer(t, "127.0.0.1:0"))
	second := M.SocksaddrFromNet(uotEchoServer(t, "127.0.0.1:0"))
	h := uotTestHandler()
	rule, _ := newACLRule("localhost", false)
	h.aclRules = append([]aclRule{rule}, h.aclRules...)
	h.AllowedPorts = []int{int(first.Port), int(second.Port)}
	conn := h.dialUOT(context.Background(), uot.Version)
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	packets, err := (&uot.Client{Version: 2}).DialConn(conn, false, first)
	if err != nil {
		t.Fatal(err)
	}
	// A forbidden domain resolving to the same allowed IP must still be denied.
	denied := M.ParseSocksaddrHostPort("localhost", first.Port)
	if _, err := packets.WriteTo([]byte("domain denied"), denied); err != nil {
		t.Fatal(err)
	}
	// Exercise more destinations than the cache can hold; denied packets must
	// not desynchronize framing, grow the cache, or terminate the tunnel.
	for port := 1; port <= uotCacheSize+10; port++ {
		_, err := packets.WriteTo([]byte("port denied"), M.ParseSocksaddrHostPort("127.0.0.1", uint16(port)))
		if err != nil {
			t.Fatal(err)
		}
	}
	// An oversized UDP payload has valid UoT framing but cannot be sent on IP.
	if _, err := packets.WriteTo(make([]byte, 65535), first); err != nil {
		t.Fatal(err)
	}
	for _, target := range []M.Socksaddr{first, second, first} {
		if _, err := packets.WriteTo([]byte("allowed"), target); err != nil {
			t.Fatal(err)
		}
		buffer := make([]byte, 128)
		n, source, err := packets.ReadFrom(buffer)
		if err != nil {
			t.Fatal(err)
		}
		if string(buffer[:n]) != "allowed" || M.SocksaddrFromNet(source).Port != target.Port {
			t.Fatalf("denied frame leaked or stream desynchronized: %q from %v", buffer[:n], source)
		}
	}

	// Observe the proxy's actual source port at two independent UDP targets.
	var sourcePort string
	unrestricted := uotTestHandler().dialUOT(context.Background(), uot.Version)
	defer unrestricted.Close()
	_ = unrestricted.SetDeadline(time.Now().Add(2 * time.Second))
	multi, err := (&uot.Client{Version: 2}).DialConn(unrestricted, false, first)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		peer, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer peer.Close()
		_ = peer.SetReadDeadline(time.Now().Add(time.Second))
		if _, err := multi.WriteTo([]byte("source port"), peer.LocalAddr()); err != nil {
			t.Fatal(err)
		}
		_, from, err := peer.ReadFrom(make([]byte, 128))
		if err != nil {
			t.Fatal(err)
		}
		_, port, _ := net.SplitHostPort(from.String())
		if i == 0 {
			sourcePort = port
		} else if sourcePort != port {
			t.Fatalf("source port changed: %s -> %s", sourcePort, port)
		}
	}
}

func TestUOTActiveCancellationAndUploadEOF(t *testing.T) {
	for _, cancelRequest := range []bool{false, true} {
		t.Run(strconv.FormatBool(cancelRequest), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			client, server := net.Pipe()
			defer client.Close()
			done := make(chan struct{})
			go func() { defer close(done); uotTestHandler().serveUOT(ctx, server, uot.Version) }()
			_ = client.SetDeadline(time.Now().Add(time.Second))
			destination := M.SocksaddrFromNet(uotEchoServer(t, "127.0.0.1:0"))
			packets, err := (&uot.Client{Version: 2}).DialConn(client, false, destination)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = packets.WriteTo([]byte("active"), destination)
			if _, _, err := packets.ReadFrom(make([]byte, 128)); err != nil {
				t.Fatal(err)
			}
			if cancelRequest {
				cancel()
			} else {
				_ = (&uotTunnel{Conn: client}).CloseWrite()
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("active session did not release its socket and reader")
			}
		})
	}
}

func TestUOTIdleTimeout(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	touch, stop := uotIdleWatch(context.Background(), server, 200*time.Millisecond)
	defer stop()
	time.Sleep(100 * time.Millisecond)
	touch()
	_ = client.SetReadDeadline(time.Now().Add(120 * time.Millisecond))
	if _, err := client.Read(make([]byte, 1)); err == nil {
		t.Fatal("idle stream returned data")
	} else if timeout, ok := err.(net.Error); !ok || !timeout.Timeout() {
		t.Fatalf("active traffic did not extend idle timer: %v", err)
	}
	_ = client.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := client.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("idle stream did not close: %v", err)
	}
}

func TestUOTAccessControl(t *testing.T) {
	destination := M.SocksaddrFromNet(uotEchoServer(t, "127.0.0.1:0"))
	for _, policy := range []string{"ip", "domain", "port"} {
		t.Run(policy, func(t *testing.T) {
			h := uotTestHandler()
			target := destination
			switch policy {
			case "ip":
				rule, _ := newACLRule("127.0.0.0/8", false)
				h.aclRules = append([]aclRule{rule}, h.aclRules...)
			case "domain":
				rule, _ := newACLRule("localhost", false)
				h.aclRules = append([]aclRule{rule}, h.aclRules...)
				target = M.ParseSocksaddrHostPort("localhost", destination.Port)
			case "port":
				h.AllowedPorts = []int{int(destination.Port) + 1}
			}
			dialed := false
			h.dialContext = func(context.Context, string, string) (net.Conn, error) {
				dialed = true
				return nil, fmt.Errorf("must not dial denied destination")
			}
			conn, err := h.dialContextCheckACL(context.Background(), "tcp", uot.MagicAddress+":0")
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(time.Second))
			packets, err := (&uot.Client{Version: 2}).DialConn(conn, true, target)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = packets.Write([]byte("denied"))
			_, err = packets.Read(make([]byte, 128))
			if err == nil {
				t.Fatal("denied UDP destination returned data")
			}
			if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
				t.Fatal("denied session did not close")
			}
			// stream close synchronizes the server's completed ACL check.
			if dialed {
				t.Fatal("ACL was bypassed")
			}
		})
	}
}

func TestUOTCancellationAndMalformedRequest(t *testing.T) {
	for _, malformed := range []bool{false, true} {
		t.Run(strconv.FormatBool(malformed), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			conn := uotTestHandler().dialUOT(ctx, uot.Version)
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(time.Second))
			if malformed {
				_, _ = conn.Write([]byte{1, 255})
			} else {
				cancel()
			}
			_, err := conn.Read(make([]byte, 1))
			if err == nil {
				t.Fatal("session remained open")
			}
			if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
				t.Fatal("session leaked")
			}
		})
	}
}

func TestUOTAuthenticationAndProbeResistance(t *testing.T) {
	for _, probe := range []bool{false, true} {
		h := uotTestHandler()
		if probe {
			h.ProbeResistance = &ProbeResistance{}
		}
		req := &http.Request{Method: http.MethodConnect, Host: uot.MagicAddress + ":0", URL: &url.URL{Host: uot.MagicAddress + ":0"}, Header: make(http.Header), ProtoMajor: 2}
		req = req.WithContext(context.WithValue(context.Background(), caddy.ReplacerCtxKey, caddy.NewReplacer()))
		w := httptest.NewRecorder()
		passed := false
		err := h.ServeHTTP(w, req, caddyhttp.HandlerFunc(func(http.ResponseWriter, *http.Request) error { passed = true; return nil }))
		if probe {
			if err != nil || !passed {
				t.Fatalf("probe was not passed through: %v", err)
			}
		} else if handlerErr, ok := err.(caddyhttp.HandlerError); !ok || handlerErr.StatusCode != http.StatusProxyAuthRequired {
			t.Fatalf("missing authentication challenge: %v", err)
		}
	}
}
