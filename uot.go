package forwardproxy

// The magic-address integration follows SagerNet/forwardproxy and
// aUsernameWoW/forwardproxy. Use sing's framing codec, but own socket lifecycle
// and policy: the generic NewServerConn adapter cannot enforce domain ACLs.

import (
	"context"
	"net"
	"strconv"
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

	var socket net.PacketConn
	var connected net.Conn
	var err error
	if request.IsConnect {
		// A fixed destination benefits from kernel source filtering and avoids
		// per-packet resolution, ACL checks and address serialization.
		connected, err = h.dialContextCheckACL(ctx, "udp", request.Destination.String())
		if err != nil || connected == nil {
			return
		}
		socket = &uotConnectedSocket{Conn: connected}
	} else {
		// One dual-stack socket preserves a stable local source port across
		// destinations (important for STUN/P2P), with constant FD/goroutine use.
		socket, err = net.ListenUDP("udp", nil)
		if err != nil {
			return
		}
	}
	defer socket.Close()
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		defer stream.Close()
		response := buf.NewSize(65535)
		defer response.Release()
		for {
			response.Reset()
			n, source, err := socket.ReadFrom(response.FreeBytes())
			if err != nil {
				return
			}
			response.Truncate(n)
			if err := packets.WritePacket(response, M.SocksaddrFromNet(source)); err != nil {
				return
			}
			touch()
		}
	}()
	defer func() {
		_ = stream.Close()
		_ = socket.Close()
		<-readDone
	}()

	cache := make(map[M.Socksaddr]uotCachedDestination)
	buffer := buf.NewSize(65535)
	defer buffer.Release()
	for {
		buffer.Reset()
		destination, err := packets.ReadPacket(buffer)
		if err != nil {
			return
		}
		touch()
		if connected != nil {
			if n, err := writeUOTConnected(connected, buffer.Bytes()); err != nil || n != buffer.Len() {
				return
			}
			continue
		}
		entry, cached := cache[destination]
		now := time.Now()
		if !cached || now.After(entry.expires) {
			entry = uotCachedDestination{expires: now.Add(uotCacheTTL)}
			IPs, err := h.lookupAllowedIPs(ctx, destination.AddrString(), strconv.Itoa(int(destination.Port)))
			if err == nil {
				entry.address = &net.UDPAddr{IP: IPs[0], Port: int(destination.Port)}
			}
			// Bound cached positive/negative decisions; a full cache never
			// interrupts a valid session or allocates more sockets/readers.
			if cached || len(cache) < uotCacheSize {
				cache[destination] = entry
			}
		}
		if entry.address == nil {
			continue
		} // Drop denied/unresolvable packets.
		if _, err := socket.WriteTo(buffer.Bytes(), entry.address); err != nil {
			if ctx.Err() != nil {
				return
			}
			// A bad destination must not break unrelated UDP destinations.
			continue
		}
	}
}

type uotCachedDestination struct {
	address *net.UDPAddr
	expires time.Time
}

type uotConnectedSocket struct{ net.Conn }

func (s *uotConnectedSocket) ReadFrom(p []byte) (int, net.Addr, error) {
	n, err := s.Read(p)
	return n, s.RemoteAddr(), err
}

func (s *uotConnectedSocket) WriteTo(p []byte, _ net.Addr) (int, error) {
	return writeUOTConnected(s.Conn, p)
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
