package connectorsdk

import (
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/http2"
	"google.golang.org/grpc/keepalive"
)

// Các con số là hợp đồng với core (core ping mỗi 15 giây khi rảnh): đổi ở đây thì phải đổi ở core.
func TestKeepalive_Defaults(t *testing.T) {
	k := defaultKeepalive()
	assert.Equal(t, keepalive.EnforcementPolicy{MinTime: 10 * time.Second, PermitWithoutStream: true}, k.enforce)
	assert.Equal(t, keepalive.ServerParameters{Time: 30 * time.Second, Timeout: 10 * time.Second}, k.params)
}

// pingServer nói HTTP/2 thô với server (không qua client gRPC, vốn không cho ping dày hơn 10 giây), gửi n
// PING cách nhau every khi không có luồng nào, rồi trả dữ liệu GOAWAY server gửi (rỗng nếu không có).
func pingServer(t *testing.T, addr string, every time.Duration, n int) string {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.Write([]byte(http2.ClientPreface))
	require.NoError(t, err)
	fr := http2.NewFramer(conn, conn)
	writes := make(chan func() error, 64)
	require.NoError(t, fr.WriteSettings())
	goaway := make(chan string, 1)
	go func() {
		for {
			f, err := fr.ReadFrame()
			if err != nil {
				close(goaway)
				return
			}
			switch f := f.(type) {
			case *http2.SettingsFrame:
				if !f.IsAck() {
					writes <- fr.WriteSettingsAck
				}
			case *http2.PingFrame:
				if !f.IsAck() {
					data := f.Data
					writes <- func() error { return fr.WritePing(true, data) }
				}
			case *http2.GoAwayFrame:
				goaway <- string(f.DebugData())
				return
			}
		}
	}()
	tick := time.NewTicker(every)
	defer tick.Stop()
	deadline := time.After(time.Duration(n)*every + 300*time.Millisecond)
	sent := 0
	for {
		select {
		case w := <-writes:
			if w() != nil {
				return ""
			}
		case <-tick.C:
			if sent < n {
				sent++
				if fr.WritePing(false, [8]byte{byte(sent)}) != nil {
					return <-goaway
				}
			}
		case g, ok := <-goaway:
			if ok {
				return g
			}
			return ""
		case <-deadline:
			return ""
		}
	}
}

// Thời gian rút ngắn 100 lần (10 giây → 100 ms): client ping mỗi 150 ms (tương ứng 15 giây của core) không bị
// đóng bằng too_many_pings; ping dày hơn MinTime, hoặc khi không cho ping lúc không có luồng, thì bị đóng
// (chứng minh test bắt được).
func TestKeepalive_ClientPingEvery15sAccepted(t *testing.T) {
	cases := []struct {
		name    string
		enforce keepalive.EnforcementPolicy
		every   time.Duration
		want    string
	}{
		{"ping 15s, MinTime 10s", keepalive.EnforcementPolicy{MinTime: 100 * time.Millisecond, PermitWithoutStream: true}, 150 * time.Millisecond, ""},
		{"ping dày hơn MinTime", keepalive.EnforcementPolicy{MinTime: 100 * time.Millisecond, PermitWithoutStream: true}, 10 * time.Millisecond, "too_many_pings"},
		{"không cho ping khi rảnh", keepalive.EnforcementPolicy{MinTime: 100 * time.Millisecond}, 150 * time.Millisecond, "too_many_pings"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			k := defaultKeepalive()
			k.enforce = c.enforce
			r := start(t, newFake(), Options{Insecure: true, keepalive: &k})
			assert.Equal(t, c.want, pingServer(t, r.addr, c.every, 6))
		})
	}
}
