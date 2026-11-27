package forwardproxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	caddy "github.com/caddyserver/caddy/v2"
	"golang.org/x/net/http2"
)

type blockingReadCloser struct {
	unblock chan struct{}
	exited  atomic.Bool
	once    sync.Once
	err     error
}

func newBlockingReadCloser(err error) *blockingReadCloser {
	return &blockingReadCloser{
		unblock: make(chan struct{}),
		err:     err,
	}
}

func (b *blockingReadCloser) Read(_ []byte) (int, error) {
	<-b.unblock
	// Brief yield so a dualStream that failed to wait for the uplink goroutine
	// would return before exited is marked.
	time.Sleep(20 * time.Millisecond)
	b.exited.Store(true)
	return 0, b.err
}

func (b *blockingReadCloser) Close() error {
	b.once.Do(func() {
		close(b.unblock)
	})
	return nil
}

type failingReader struct {
	err error
}

func (f failingReader) Read(_ []byte) (int, error) { return 0, f.err }
func (f failingReader) Close() error               { return nil }

type closeWritePipeConn struct {
	net.Conn
	closeWriteCalled atomic.Bool
}

func (c *closeWritePipeConn) CloseWrite() error {
	c.closeWriteCalled.Store(true)
	return nil
}

// Simulates an HTTP/2 RST_STREAM (canceling r.Context()) while the target TCP
// server is completely silent. dualStream must wake the downlink reader and
// return promptly rather than leaking a zombie stream and 64 KiB buffer.
func TestDualStreamCancelsDownlinkOnContextCancel(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	accepted := make(chan net.Conn, 1)
	go func() {
		conn, acceptErr := ln.Accept()
		if acceptErr == nil {
			accepted <- conn
		}
	}()

	target, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	serverSide := <-accepted
	defer serverSide.Close()

	ctx, cancel := context.WithCancel(context.Background())
	reader := newBlockingReadCloser(context.Canceled)
	var clientOut bytes.Buffer

	done := make(chan error, 1)
	go func() {
		done <- dualStream(ctx, target, reader, &clientOut, true)
	}()

	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("dualStream hung after context cancellation with silent target")
	}

	if !reader.exited.Load() {
		t.Fatal("dualStream returned before uplink goroutine exited")
	}
}

// When the client upload fails abnormally (e.g. truncated Naive padding or
// broken stream) without canceling ctx, dualStream must still close target so
// the downlink reader does not hang indefinitely.
func TestDualStreamWakesDownlinkOnAbnormalUplinkError(t *testing.T) {
	target, peer := net.Pipe()
	defer target.Close()
	defer peer.Close()

	var clientOut bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- dualStream(context.Background(), target, failingReader{err: errors.New("stream reset")}, &clientOut, false)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("dualStream hung after abnormal uplink read error")
	}
}

// Clean client half-close (io.EOF) must call CloseWrite on target and still
// allow the target to finish sending its remaining response to the client.
func TestDualStreamPreservesCleanHalfClose(t *testing.T) {
	target, peer := net.Pipe()
	defer target.Close()
	defer peer.Close()

	wrappedTarget := &closeWritePipeConn{Conn: target}
	wantResp := []byte("response-after-client-eof")

	peerErr := make(chan error, 1)
	go func() {
		req := make([]byte, 4)
		if _, err := io.ReadFull(peer, req); err != nil {
			peerErr <- err
			return
		}
		if string(req) != "ping" {
			peerErr <- errors.New("unexpected request payload")
			return
		}
		// Delay briefly so uplink reaches EOF and calls CloseWrite first.
		time.Sleep(30 * time.Millisecond)
		if _, err := peer.Write(wantResp); err != nil {
			peerErr <- err
			return
		}
		peerErr <- peer.Close()
	}()

	var clientOut bytes.Buffer
	err := dualStream(context.Background(), wrappedTarget, io.NopCloser(bytes.NewReader([]byte("ping"))), &clientOut, false)
	if err != nil {
		t.Fatalf("dualStream returned error on clean half-close: %v", err)
	}
	if err := <-peerErr; err != nil {
		t.Fatalf("peer error: %v", err)
	}
	if !wrappedTarget.closeWriteCalled.Load() {
		t.Fatal("expected CloseWrite to be called on clean uplink EOF")
	}
	if !bytes.Equal(clientOut.Bytes(), wantResp) {
		t.Fatalf("got %q; want %q", clientOut.Bytes(), wantResp)
	}
}

// When target closes cleanly first, dualStream must close clientReader and wait
// for the uplink goroutine to finish before returning.
func TestDualStreamWaitsForUplinkGoroutineBeforeReturn(t *testing.T) {
	target, peer := net.Pipe()
	defer target.Close()

	go func() {
		_, _ = peer.Write([]byte("done"))
		_ = peer.Close()
	}()

	reader := newBlockingReadCloser(io.ErrClosedPipe)
	var clientOut bytes.Buffer
	err := dualStream(context.Background(), target, reader, &clientOut, false)
	if err != nil {
		t.Fatalf("dualStream returned unexpected error: %v", err)
	}
	if !reader.exited.Load() {
		t.Fatal("dualStream returned before uplink goroutine finished")
	}
	if clientOut.String() != "done" {
		t.Fatalf("got %q; want %q", clientOut.String(), "done")
	}
}

// End-to-end HTTP/2 over TLS CONNECT verification:
//  1. Padded HTTP/2 CONNECT forwards 8 padded frames + raw data cleanly.
//  2. Canceling an HTTP/2 CONNECT stream (RST_STREAM) with a silent target
//     immediately terminates ServeHTTP and closes the backend TCP connection.
func TestHTTP2ConnectPaddingAndStreamReset(t *testing.T) {
	echoLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echoLn.Close()
	go func() {
		for {
			conn, acceptErr := echoLn.Accept()
			if acceptErr != nil {
				return
			}
			go func() {
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()

	silentLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer silentLn.Close()
	silentReady := make(chan struct{}, 1)
	silentClosed := make(chan struct{}, 1)
	go func() {
		conn, acceptErr := silentLn.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		var b [1]byte
		if _, readErr := io.ReadFull(conn, b[:]); readErr != nil {
			return
		}
		silentReady <- struct{}{}
		// Stay completely silent and block until the proxy tears down the TCP conn.
		_, _ = io.Copy(io.Discard, conn)
		silentClosed <- struct{}{}
	}()

	dialer := &net.Dialer{Timeout: 5 * time.Second}
	h := &Handler{
		aclRules:    []aclRule{&aclAllRule{allow: true}},
		dialContext: dialer.DialContext,
	}

	serveDone := make(chan struct{}, 2)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { serveDone <- struct{}{} }()
		r = r.WithContext(context.WithValue(r.Context(), caddy.ReplacerCtxKey, caddy.NewReplacer()))
		_ = h.ServeHTTP(w, r, nil)
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()

	h2Transport := &http2.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true, // #nosec G402 -- local httptest TLS cert
		},
	}
	defer h2Transport.CloseIdleConnections()

	// Part 1: Padded HTTP/2 CONNECT across 8 padded frames + raw tail.
	t.Run("padded connect 8 frames plus raw", func(t *testing.T) {
		pr, pw := io.Pipe()
		req := &http.Request{
			Method: http.MethodConnect,
			URL:    &url.URL{Scheme: "https", Host: echoLn.Addr().String()},
			Host:   echoLn.Addr().String(),
			Header: http.Header{"Padding": []string{"naive-h2-test"}},
			Body:   pr,
		}
		// Dial the httptest server address instead of the CONNECT target.
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

		var want bytes.Buffer
		go func() {
			defer pw.Close()
			for i := 0; i < NumFirstPaddings; i++ {
				payload := []byte("frame-payload")
				pad := make([]byte, i+1)
				frame := appendPaddingFrame(nil, payload, pad)
				_, _ = pw.Write(frame)
			}
			_, _ = pw.Write([]byte("raw-tail-bytes"))
		}()
		for i := 0; i < NumFirstPaddings; i++ {
			want.WriteString("frame-payload")
		}
		want.WriteString("raw-tail-bytes")

		var got bytes.Buffer
		// Server adds Naive padding to the first NumFirstPaddings downlink chunks.
		if _, err := flushingIoCopy(&got, resp.Body, make([]byte, 64*1024), RemovePadding); err != nil {
			t.Fatalf("failed reading padded H2 response: %v", err)
		}
		if !bytes.Equal(got.Bytes(), want.Bytes()) {
			t.Fatalf("echo mismatch: got %q, want %q", got.String(), want.String())
		}
		select {
		case <-serveDone:
		case <-time.After(2 * time.Second):
			t.Fatal("ServeHTTP did not exit after clean H2 CONNECT close")
		}
	})

	// Part 2: HTTP/2 RST_STREAM cancels silent target and finishes ServeHTTP.
	t.Run("rst_stream wakes silent target", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

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

		pr, pw := io.Pipe()
		defer pw.Close()
		req := (&http.Request{
			Method: http.MethodConnect,
			URL:    &url.URL{Scheme: "https", Host: silentLn.Addr().String()},
			Host:   silentLn.Addr().String(),
			Header: http.Header{"Padding": []string{"naive-h2-rst"}},
			Body:   pr,
		}).WithContext(ctx)

		resp, err := cc.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		// Send one valid padded frame and wait until the silent target receives it,
		// proving the tunnel is active inside dualStream before RST_STREAM.
		var hdr [3]byte
		binary.BigEndian.PutUint16(hdr[:2], 1)
		hdr[2] = 0
		if _, err := pw.Write(append(hdr[:], 'x')); err != nil {
			t.Fatal(err)
		}
		select {
		case <-silentReady:
		case <-time.After(2 * time.Second):
			t.Fatal("silent target did not receive initial byte")
		}
		select {
		case <-serveDone:
			t.Fatal("ServeHTTP exited unexpectedly before RST_STREAM")
		default:
		}

		// Closing resp.Body with canceled ctx sends HTTP/2 RST_STREAM while
		// keeping the outer H2 TLS connection open.
		cancel()
		_ = resp.Body.Close()

		select {
		case <-serveDone:
		case <-time.After(2 * time.Second):
			t.Fatal("ServeHTTP hung after HTTP/2 RST_STREAM against silent target")
		}
		select {
		case <-silentClosed:
		case <-time.After(2 * time.Second):
			t.Fatal("silent target TCP connection was not closed after RST_STREAM")
		}
	})
}
