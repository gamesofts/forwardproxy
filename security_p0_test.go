package forwardproxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
)

// Hostnames and IP literals both reach Go's native dialer without an IP ACL.
func TestNativeDialDomainAndLiteralIP(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	h := uotTestHandler()
	for _, host := range []string{"localhost", "127.0.0.1"} {
		t.Run(host, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			conn, err := h.dialTarget(ctx, "tcp", net.JoinHostPort(host, port))
			if err != nil {
				t.Fatal(err)
			}
			_ = conn.Close()
		})
	}
}

func TestDialTargetRejectsBlockedPortWithoutDNS(t *testing.T) {
	h := &Handler{
		AllowedPorts: []int{443},
		dialContext: func(context.Context, string, string) (net.Conn, error) {
			t.Fatal("blocked port reached the dialer")
			return nil, nil
		},
	}
	if _, err := h.dialTarget(context.Background(), "tcp", "example.invalid:80"); err == nil {
		t.Fatal("blocked port was accepted")
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
