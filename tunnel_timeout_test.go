package forwardproxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	caddy "github.com/caddyserver/caddy/v2"
	"golang.org/x/net/http2"
)

type fakeIdleReadCloser struct {
	io.ReadCloser
	mu          sync.Mutex
	timeout     time.Duration
	overridden  bool
	restored    bool
	minRateSeen int64
}

func (f *fakeIdleReadCloser) Override(timeout time.Duration, minRate int64) func() {
	f.mu.Lock()
	prev := f.timeout
	f.timeout = timeout
	f.minRateSeen = minRate
	f.overridden = true
	f.mu.Unlock()
	return func() {
		f.mu.Lock()
		f.timeout = prev
		f.restored = true
		f.mu.Unlock()
	}
}

func (f *fakeIdleReadCloser) Read(p []byte) (int, error) {
	f.mu.Lock()
	d := f.timeout
	f.mu.Unlock()

	type readResult struct {
		n   int
		err error
	}
	ch := make(chan readResult, 1)
	go func() {
		n, err := f.ReadCloser.Read(p)
		ch <- readResult{n, err}
	}()
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case res := <-ch:
		return res.n, res.err
	case <-timer.C:
		_ = f.ReadCloser.Close()
		return 0, os.ErrDeadlineExceeded
	}
}

type fakeLengthReader struct {
	Source io.ReadCloser
	Length int
}

func (r *fakeLengthReader) Read(p []byte) (int, error) {
	n, err := r.Source.Read(p)
	r.Length += n
	return n, err
}

func (r *fakeLengthReader) Close() error {
	return r.Source.Close()
}

type fakeIdleResponseWriter struct {
	http.ResponseWriter
	mu         sync.Mutex
	timeout    time.Duration
	overridden bool
	restored   bool
}

func (w *fakeIdleResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (w *fakeIdleResponseWriter) Override(timeout time.Duration, _ int64, _ int) func() {
	w.mu.Lock()
	prev := w.timeout
	w.timeout = timeout
	w.overridden = true
	w.mu.Unlock()
	return func() {
		w.mu.Lock()
		w.timeout = prev
		w.restored = true
		w.mu.Unlock()
	}
}

type fakeResponseRecorder struct {
	http.ResponseWriter
}

func (r *fakeResponseRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

func TestOverrideTunnelIdleTimeouts(t *testing.T) {
	t.Run("direct wrappers", func(t *testing.T) {
		body := &fakeIdleReadCloser{
			ReadCloser: io.NopCloser(bytes.NewReader(nil)),
			timeout:    time.Minute,
		}
		rw := &fakeIdleResponseWriter{
			ResponseWriter: httptest.NewRecorder(),
			timeout:        time.Minute,
		}
		req := httptest.NewRequest(http.MethodConnect, "https://example.com:443", body)

		restore := overrideTunnelIdleTimeouts(req, rw, connectIdleTimeout)
		if !body.overridden || body.timeout != connectIdleTimeout {
			t.Fatalf("read override = (%v, %v); want (true, %v)", body.overridden, body.timeout, connectIdleTimeout)
		}
		if !rw.overridden || rw.timeout != connectIdleTimeout {
			t.Fatalf("write override = (%v, %v); want (true, %v)", rw.overridden, rw.timeout, connectIdleTimeout)
		}

		restore()
		if !body.restored || body.timeout != time.Minute {
			t.Fatalf("read restore = (%v, %v); want (true, 1m)", body.restored, body.timeout)
		}
		if !rw.restored || rw.timeout != time.Minute {
			t.Fatalf("write restore = (%v, %v); want (true, 1m)", rw.restored, rw.timeout)
		}
	})

	t.Run("access-log wrapped lengthReader and ResponseRecorder", func(t *testing.T) {
		innerBody := &fakeIdleReadCloser{
			ReadCloser: io.NopCloser(bytes.NewReader(nil)),
			timeout:    time.Minute,
		}
		innerRW := &fakeIdleResponseWriter{
			ResponseWriter: httptest.NewRecorder(),
			timeout:        time.Minute,
		}
		req := httptest.NewRequest(http.MethodConnect, "https://example.com:443", &fakeLengthReader{Source: innerBody})
		wrappedRW := &fakeResponseRecorder{ResponseWriter: innerRW}

		restore := overrideTunnelIdleTimeouts(req, wrappedRW, connectIdleTimeout)
		if !innerBody.overridden || innerBody.timeout != connectIdleTimeout {
			t.Fatalf("wrapped read override = (%v, %v); want (true, %v)", innerBody.overridden, innerBody.timeout, connectIdleTimeout)
		}
		if !innerRW.overridden || innerRW.timeout != connectIdleTimeout {
			t.Fatalf("wrapped write override = (%v, %v); want (true, %v)", innerRW.overridden, innerRW.timeout, connectIdleTimeout)
		}
		restore()
		if !innerBody.restored || !innerRW.restored {
			t.Fatalf("expected wrapped read/write overrides to restore: read=%v write=%v", innerBody.restored, innerRW.restored)
		}
	})

	t.Run("plain request and writer are no-op", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodConnect, "https://example.com:443", io.NopCloser(bytes.NewReader(nil)))
		restore := overrideTunnelIdleTimeouts(req, httptest.NewRecorder(), connectIdleTimeout)
		restore()
	})
}

// Simulates Caddy v2.11+'s default read_body_idle / write_idle wrappers on an
// HTTP/2 CONNECT download where the client sends no uplink bytes after the
// initial request while the origin streams data longer than the idle timeout.
func TestHTTP2ConnectRelaxesIdleTimeouts(t *testing.T) {
	// Target origin waits 160ms (> 60ms simulated read_body_idle) before finishing
	// a unidirectional downlink download with zero uplink bytes after connect.
	originLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer originLn.Close()

	const wantBody = "chunk-1-chunk-2-done"
	go func() {
		conn, acceptErr := originLn.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		_, _ = conn.Write([]byte("chunk-1-"))
		time.Sleep(160 * time.Millisecond)
		_, _ = conn.Write([]byte("chunk-2-done"))
	}()

	h := &Handler{
		ACL: []ACLRule{{Allow: true, Subjects: []string{"127.0.0.1"}}},
	}
	if err := h.Provision(caddy.Context{}); err != nil {
		t.Fatal(err)
	}

	var (
		mu       sync.Mutex
		lastRead *fakeIdleReadCloser
		lastRW   *fakeIdleResponseWriter
	)
	serveDone := make(chan struct{}, 1)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { serveDone <- struct{}{} }()
		idleBody := &fakeIdleReadCloser{
			ReadCloser: r.Body,
			timeout:    60 * time.Millisecond,
		}
		idleRW := &fakeIdleResponseWriter{
			ResponseWriter: w,
			timeout:        60 * time.Millisecond,
		}
		mu.Lock()
		lastRead = idleBody
		lastRW = idleRW
		mu.Unlock()

		// Wrap both to mimic Caddy v2.11+ with access logs enabled.
		r = r.WithContext(context.WithValue(r.Context(), caddy.ReplacerCtxKey, caddy.NewReplacer()))
		r.Body = &fakeLengthReader{Source: idleBody}
		wrappedW := &fakeResponseRecorder{ResponseWriter: idleRW}
		if serveErr := h.ServeHTTP(wrappedW, r, nil); serveErr != nil {
			t.Errorf("ServeHTTP failed: %v", serveErr)
		}
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()

	h2Transport := &http2.Transport{
		TLSClientConfig: srv.Client().Transport.(*http.Transport).TLSClientConfig.Clone(),
	}
	defer h2Transport.CloseIdleConnections()

	pr, pw := io.Pipe()
	defer pw.Close()
	req := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Scheme: "https", Host: originLn.Addr().String()},
		Host:   originLn.Addr().String(),
		Header: make(http.Header),
		Body:   pr,
	}

	h2Conn, err := tls.Dial("tcp", srv.Listener.Addr().String(), &tls.Config{
		InsecureSkipVerify: true, // #nosec G402 -- local httptest TLS cert
		NextProtos:         []string{"h2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer h2Conn.Close()
	cc, err := h2Transport.NewClientConn(h2Conn)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := cc.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT status = %d; want 200", resp.StatusCode)
	}

	// Keep uplink completely silent while receiving the entire downlink stream.
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("download aborted prematurely: %v", err)
	}
	if string(got) != wantBody {
		t.Fatalf("download payload = %q; want %q", string(got), wantBody)
	}
	_ = pw.Close()

	select {
	case <-serveDone:
	case <-time.After(2 * time.Second):
		t.Fatal("ServeHTTP did not exit after download completed")
	}

	mu.Lock()
	defer mu.Unlock()
	if lastRead == nil || !lastRead.overridden || !lastRead.restored {
		t.Fatalf("expected IdleTimeoutReader override+restore; got %+v", lastRead)
	}
	if lastRW == nil || !lastRW.overridden || !lastRW.restored {
		t.Fatalf("expected IdleTimeoutWriter override+restore; got %+v", lastRW)
	}
}

type leakingDeadlineFlushWriter struct {
	http.ResponseWriter
	conn    net.Conn
	timeout time.Duration
}

func (w *leakingDeadlineFlushWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// Flush mimics Caddy v2.11+ IdleTimeoutWriter.FlushError() on HTTP/1.1: it
// arms a write deadline on the underlying net.Conn and does NOT clear it
// because ClearBetweenWrites is false for HTTP/1.1.
func (w *leakingDeadlineFlushWriter) Flush() {
	_ = w.conn.SetWriteDeadline(time.Now().Add(w.timeout))
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *leakingDeadlineFlushWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return http.NewResponseController(w.ResponseWriter).Hijack()
}

// Verifies that HTTP/1.1 CONNECT clears any write deadline armed on the raw
// TCP connection during the pre-hijack Flush (Caddy v2.11+ write_idle).
func TestHTTP1ServeHijackClearsLeftoverDeadlines(t *testing.T) {
	originLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer originLn.Close()

	go func() {
		conn, acceptErr := originLn.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		// Wait longer than the 50ms leaked write deadline before sending data.
		time.Sleep(120 * time.Millisecond)
		_, _ = conn.Write([]byte("after-leaked-deadline"))
	}()

	h := &Handler{
		ACL: []ACLRule{{Allow: true, Subjects: []string{"127.0.0.1"}}},
	}
	if err := h.Provision(caddy.Context{}); err != nil {
		t.Fatal(err)
	}

	type connCtxKey struct{}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawConn, _ := r.Context().Value(connCtxKey{}).(net.Conn)
		leakingW := &leakingDeadlineFlushWriter{
			ResponseWriter: w,
			conn:           rawConn,
			timeout:        50 * time.Millisecond,
		}
		r = r.WithContext(context.WithValue(r.Context(), caddy.ReplacerCtxKey, caddy.NewReplacer()))
		if serveErr := h.ServeHTTP(leakingW, r, nil); serveErr != nil && !errors.Is(serveErr, io.EOF) {
			t.Errorf("ServeHTTP failed: %v", serveErr)
		}
	}))
	srv.Config.ConnContext = func(ctx context.Context, c net.Conn) context.Context {
		return context.WithValue(ctx, connCtxKey{}, c)
	}
	srv.Start()
	defer srv.Close()

	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	_, err = conn.Write([]byte("CONNECT " + originLn.Addr().String() + " HTTP/1.1\r\nHost: " + originLn.Addr().String() + "\r\n\r\n"))
	if err != nil {
		t.Fatal(err)
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT status = %d; want 200", resp.StatusCode)
	}

	got, err := io.ReadAll(br)
	if err != nil {
		t.Fatalf("failed reading hijacked tunnel after leaked deadline: %v", err)
	}
	if string(got) != "after-leaked-deadline" {
		t.Fatalf("tunnel payload = %q; want %q", string(got), "after-leaked-deadline")
	}
}
