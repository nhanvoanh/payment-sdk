package connectorrpc_test

import (
	"bytes"
	"crypto/tls"
	"log/slog"
	"math/big"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"bitbucket.org/hasaki-tech/payment-connector-sdk/connectorrpc"
	"bitbucket.org/hasaki-tech/payment-connector-sdk/connectorrpc/tlstest"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

// serverSerial bắt tay qua net.Pipe và trả số serial của chứng chỉ server mà client nhận được.
func serverSerial(t *testing.T, srv, cli *tls.Config) *big.Int {
	t.Helper()
	c1, c2 := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = tls.Server(c1, srv).Handshake()
		_ = c1.Close() // đóng đầu ống, không gửi close_notify (net.Pipe đồng bộ sẽ chặn)
	}()
	cl := tls.Client(c2, cli)
	require.NoError(t, cl.Handshake())
	sn := cl.ConnectionState().PeerCertificates[0].SerialNumber
	_ = c2.Close()
	<-done
	return sn
}

func copyPair(t *testing.T, from, to connectorrpc.TLSFiles, mtime time.Time) {
	t.Helper()
	for _, p := range [][2]string{{from.CertFile, to.CertFile}, {from.KeyFile, to.KeyFile}} {
		b, err := os.ReadFile(p[0])
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(p[1], b, 0o600))
		require.NoError(t, os.Chtimes(p[1], mtime, mtime))
	}
}

func TestTLS_ReloadsRotatedCert(t *testing.T) {
	b := tlstest.New(t)
	live := b.Server("srv.test")
	next := b.Server("srv.test")
	third := b.Server("srv.test")
	clk := &clock{t: time.Unix(1_000_000, 0)}
	mtime := time.Now()
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))

	srv, err := connectorrpc.ServerTLSWithClock(live, 30*time.Second, clk.now, logger)
	require.NoError(t, err)
	cli, err := connectorrpc.ClientTLSWithClock(b.Client("cli.test"), "srv.test", 30*time.Second, clk.now, logger)
	require.NoError(t, err)

	first := serverSerial(t, srv, cli)

	// File đổi nhưng chưa tới mốc kiểm: vẫn cặp cũ.
	copyPair(t, next, live, mtime.Add(time.Hour))
	clk.t = clk.t.Add(10 * time.Second)
	require.Equal(t, first, serverSerial(t, srv, cli))

	// Tới mốc kiểm: kết nối mới dùng cặp mới.
	clk.t = clk.t.Add(21 * time.Second)
	second := serverSerial(t, srv, cli)
	require.NotEqual(t, first, second)

	// Ghi rác: giữ cặp cũ.
	require.NoError(t, os.WriteFile(live.CertFile, []byte("rác"), 0o600))
	require.NoError(t, os.Chtimes(live.CertFile, mtime.Add(2*time.Hour), mtime.Add(2*time.Hour)))
	clk.t = clk.t.Add(31 * time.Second)
	require.Equal(t, second, serverSerial(t, srv, cli))
	require.Equal(t, 1, strings.Count(logs.String(), "giữ cặp cũ"), "báo Warn khi đọc lại hỏng")
	require.NotContains(t, logs.String(), "rác", "không ghi nội dung file")

	// File chưa đổi: không báo lặp lại mỗi lần kiểm.
	clk.t = clk.t.Add(31 * time.Second)
	require.Equal(t, second, serverSerial(t, srv, cli))
	require.Equal(t, 1, strings.Count(logs.String(), "giữ cặp cũ"))

	// Sửa lại bằng cặp hợp lệ: lần kiểm kế tiếp nhận.
	copyPair(t, third, live, mtime.Add(3*time.Hour))
	clk.t = clk.t.Add(31 * time.Second)
	got := serverSerial(t, srv, cli)
	require.NotEqual(t, second, got)
	require.NotEqual(t, first, got)
}

func TestTLS_ConfigInvariants(t *testing.T) {
	b := tlstest.New(t)
	srv, err := connectorrpc.ServerTLS(b.Server("srv.test"))
	require.NoError(t, err)
	require.Equal(t, uint16(tls.VersionTLS13), srv.MinVersion)
	require.Equal(t, tls.RequireAndVerifyClientCert, srv.ClientAuth)

	cli, err := connectorrpc.ClientTLS(b.Client("cli.test"), "srv.test")
	require.NoError(t, err)
	require.Equal(t, uint16(tls.VersionTLS13), cli.MinVersion)
	require.Equal(t, "srv.test", cli.ServerName)

	_, err = connectorrpc.ClientTLS(b.Client("cli.test"), "")
	require.Error(t, err)
	_, err = connectorrpc.ServerTLS(connectorrpc.TLSFiles{CertFile: "x", KeyFile: "y", CAFile: "z"})
	require.Error(t, err)
	bad := b.Server("srv.test")
	bad.CAFile = bad.CertFile[:len(bad.CertFile)-len("cert.pem")] + "key.pem" // không phải CA
	_, err = connectorrpc.ServerTLS(bad)
	require.Error(t, err)
}
