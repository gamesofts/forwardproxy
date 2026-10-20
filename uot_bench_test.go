package forwardproxy

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	M "github.com/sagernet/sing/common/metadata"
	"github.com/sagernet/sing/common/uot"
)

// The library baseline is the UoT adapter used by the compared public forks.
// It has no ACLs, timeout, or request-cancellation integration of its own.
func BenchmarkUOTRelay(b *testing.B) {
	for _, implementation := range []string{"sing-adapter", "forwardproxy"} {
		for _, connect := range []bool{false, true} {
			for _, size := range []int{64, 1200, 65000} {
				b.Run(fmt.Sprintf("%s/connect=%t/bytes=%d", implementation, connect, size), func(b *testing.B) {
					if implementation == "sing-adapter" && size > 16384 {
						b.Skip("sing v0.7.5 NewServerConn uses a 16 KiB packet buffer")
					}
					destination := M.SocksaddrFromNet(uotEchoServer(b, "127.0.0.1:0"))
					var conn net.Conn
					if implementation == "sing-adapter" {
						udp, err := net.ListenUDP("udp", nil)
						if err != nil {
							b.Fatal(err)
						}
						conn = uot.NewServerConn(udp, uot.Version)
					} else {
						conn = uotTestHandler().dialUOT(context.Background(), uot.Version)
					}
					defer conn.Close()
					_ = conn.SetDeadline(time.Now().Add(time.Minute))
					packets, err := (&uot.Client{Version: 2}).DialConn(conn, connect, destination)
					if err != nil {
						b.Fatal(err)
					}
					payload, response := make([]byte, size), make([]byte, 65535)
					b.SetBytes(int64(size))
					b.ReportAllocs()
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						if _, err := packets.WriteTo(payload, destination); err != nil {
							b.Fatal(err)
						}
						if n, _, err := packets.ReadFrom(response); err != nil || n != size {
							b.Fatalf("UDP round trip: %d %v", n, err)
						}
					}
				})
			}
		}
	}
}
