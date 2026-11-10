package forwardproxy

// The magic-address integration follows SagerNet/forwardproxy and
// aUsernameWoW/forwardproxy. Use sing's framing codec, but own socket lifecycle
// and policy: the generic NewServerConn adapter does not enforce ACLs.

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sagernet/sing/common/buf"
	M "github.com/sagernet/sing/common/metadata"
	"github.com/sagernet/sing/common/uot"
)

const (
	uotIdleTimeout = 2 * time.Minute
	uotCacheTTL    = time.Minute
	uotCacheSize   = 256
)

func uotVersion(host string) (int, bool) {
	switch host {
	case uot.MagicAddress:
		return uot.Version, true
	case uot.LegacyMagicAddress:
		return uot.LegacyVersion, true
	default:
		return 0, false
	}
}

// Preserve the existing CONNECT/padding path. UDP has no TCP-style EOF, so an
// upload EOF must close the pipe and wake the UDP reader too.
type uotTunnel struct{ net.Conn }

func (c *uotTunnel) CloseWrite() error { return c.Close() }

func (h Handler) dialUOT(ctx context.Context, version int) net.Conn {
	client, server := net.Pipe()
	go h.serveUOT(ctx, server, version)
	return &uotTunnel{Conn: client}
}

// Idle tracking uses a single timer rather than resetting pipe deadlines on
// every packet (which creates timers and allocations in the packet hot path).
func uotIdleWatch(ctx context.Context, stream net.Conn, timeout time.Duration) (touch func(), stop func()) {
	started := time.Now()
	var last atomic.Int64
	touch = func() { last.Store(time.Since(started).Nanoseconds()) }
	touch()
	timer := time.NewTimer(timeout)
	done, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		defer timer.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				_ = stream.Close()
				return
			case <-timer.C:
				remaining := timeout - (time.Since(started) - time.Duration(last.Load()))
				if remaining <= 0 {
					_ = stream.Close()
					return
				}
				timer.Reset(remaining)
			}
		}
	}()
	return touch, func() { close(done); <-stopped }
}

// serveUOT handles the UoT v1/v2 framing and dispatches the two socket modes.
// All DNS resolution is performed by Go's DialContext, after request-host ACLs.
func (h Handler) serveUOT(ctx context.Context, stream net.Conn, version int) {
	defer stream.Close()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	touch, stop := uotIdleWatch(ctx, stream, uotIdleTimeout)
	defer stop()

	var request uot.Request
	if version == uot.Version {
		parsed, err := uot.ReadRequest(stream)
		if err != nil {
			return
		}
		request = *parsed
	}
	packets := uot.NewConn(stream, request)
	if !request.IsConnect {
		h.serveUOTMulti(ctx, stream, packets, touch)
		return
	}

	// Fixed-destination UoT continues to use one connected UDP socket.
	connected, err := h.dialContextCheckACL(ctx, "udp", request.Destination.String())
	if err != nil || connected == nil {
		return
	}
	defer connected.Close()

	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		defer stream.Close()
		response := buf.NewSize(65535)
		defer response.Release()
		source := M.SocksaddrFromNet(connected.RemoteAddr())
		for {
			response.Reset()
			n, err := connected.Read(response.FreeBytes())
			if err != nil {
				return
			}
			response.Truncate(n)
			if err := packets.WritePacket(response, source); err != nil {
				return
			}
			touch()
		}
	}()
	defer func() {
		_ = stream.Close()
		_ = connected.Close()
		<-readDone
	}()

	buffer := buf.NewSize(65535)
	defer buffer.Release()
	for {
		buffer.Reset()
		if _, err := packets.ReadPacket(buffer); err != nil {
			return
		}
		touch()
		if n, err := writeUOTConnected(connected, buffer.Bytes()); err != nil || n != buffer.Len() {
			return
		}
	}
}

type uotResponse struct {
	payload []byte
	source  M.Socksaddr
}

// serveUOTMulti uses Go's native UDP DialContext for each destination,
// avoiding explicit DNS lookups and preserving the pre-DNS domain ACL.
// Each target has its own connected socket (thus its own source port), with
// a bounded 256-entry cache and one reader goroutine per live connection.
func (h Handler) serveUOTMulti(ctx context.Context, stream net.Conn, packets *uot.Conn, touch func()) {
	ctx, cancel := context.WithCancel(ctx)
	replies := make(chan uotResponse, 32)
	writerDone := make(chan struct{})
	var readers sync.WaitGroup
	cache := make(map[M.Socksaddr]uotCachedDestination)

	// Only one goroutine writes UoT reply frames to the shared stream.
	go func() {
		defer close(writerDone)
		for {
			select {
			case <-ctx.Done():
				return
			case reply := <-replies:
				capacity := len(reply.payload)
				if capacity == 0 {
					capacity = 1
				}
				response := buf.NewSize(capacity)
				copy(response.FreeBytes(), reply.payload)
				response.Truncate(len(reply.payload))
				err := packets.WritePacket(response, reply.source)
				response.Release()
				if err != nil {
					_ = stream.Close()
					return
				}
				touch()
			}
		}
	}()

	// Close connections before waiting for their readers, then stop the
	// single response writer. Canceled and evicted readers cannot leak.
	defer func() {
		cancel()
		_ = stream.Close()
		for _, entry := range cache {
			if entry.conn != nil {
				_ = entry.conn.Close()
			}
		}
		readers.Wait()
		<-writerDone
	}()

	buffer := buf.NewSize(65535)
	defer buffer.Release()
	for {
		buffer.Reset()
		destination, err := packets.ReadPacket(buffer)
		if err != nil {
			return
		}
		touch()
		now := time.Now()
		entry, found := cache[destination]
		if !found || !now.Before(entry.expires) {
			// The ACL checks only the original destination; Go's dialer
			// handles DNS and chooses the UDP destination IP itself.
			conn, err := h.dialContextCheckACL(ctx, "udp", destination.String())
			entry = uotCachedDestination{expires: now.Add(uotCacheTTL)}
			if err == nil && conn != nil {
				entry.conn = conn
				readers.Add(1)
				go func(conn net.Conn) {
					defer readers.Done()
					source := M.SocksaddrFromNet(conn.RemoteAddr())
					udpBuffer := make([]byte, 65535)
					for {
						n, err := conn.Read(udpBuffer)
						if err != nil {
							return
						}
						data := append([]byte(nil), udpBuffer[:n]...)
						select {
						case replies <- uotResponse{payload: data, source: source}:
						case <-ctx.Done():
							return
						}
					}
				}(conn)
			}
			if oldConn := storeUOTDestination(cache, destination, entry); oldConn != nil {
				_ = oldConn.Close()
			}
		} else if entry.conn != nil {
			// Expiration is based on idleness, not age, so active UDP flows
			// keep their connected socket and stable per-destination port.
			entry.expires = now.Add(uotCacheTTL)
			cache[destination] = entry
		}
		if entry.conn == nil {
			continue // Denied or unresolved destination; keep other targets alive.
		}
		if n, err := writeUOTConnected(entry.conn, buffer.Bytes()); err != nil || n != buffer.Len() {
			if ctx.Err() != nil {
				return
			}
			// A failing destination must not close the entire UoT session.
			_ = entry.conn.Close()
			delete(cache, destination)
		}
	}
}

type uotCachedDestination struct {
	conn    net.Conn
	expires time.Time
}

// A bounded cache of connected UDP sockets. Prefer evicting expired entries;
// otherwise evict the least recently used entry to keep open FDs/goroutines
// below uotCacheSize. Return its socket for the caller to close.
func storeUOTDestination(cache map[M.Socksaddr]uotCachedDestination, destination M.Socksaddr, entry uotCachedDestination) net.Conn {
	if old, exists := cache[destination]; exists {
		cache[destination] = entry
		return old.conn
	}
	var evicted net.Conn
	if len(cache) >= uotCacheSize {
		var oldest M.Socksaddr
		var oldestExpiry time.Time
		for key, old := range cache {
			if oldestExpiry.IsZero() || old.expires.Before(oldestExpiry) {
				oldest = key
				oldestExpiry = old.expires
			}
		}
		evicted = cache[oldest].conn
		delete(cache, oldest)
	}
	cache[destination] = entry
	return evicted
}

func writeUOTConnected(conn net.Conn, payload []byte) (int, error) {
	// Windows' generic FD.Write loop skips empty slices. WriteMsgUDP must
	// issue a real send operation so an empty UDP datagram is not swallowed.
	if len(payload) == 0 {
		if udp, ok := conn.(*net.UDPConn); ok {
			n, _, err := udp.WriteMsgUDP(payload, nil, nil)
			return n, err
		}
	}
	return conn.Write(payload)
}
