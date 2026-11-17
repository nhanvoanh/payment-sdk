package connectorsdk

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"

	"bitbucket.org/hasaki-tech/payment-connector-sdk/connector"
	"bitbucket.org/hasaki-tech/payment-connector-sdk/connectorpb"
	"bitbucket.org/hasaki-tech/payment-connector-sdk/connectorrpc"
	"bitbucket.org/hasaki-tech/payment-connector-sdk/connectorrpc/tlstest"
	"bitbucket.org/hasaki-tech/payment-connector-sdk/connectortest"
	"bitbucket.org/hasaki-tech/payment-connector-sdk/paykit"
)

const srvName = "srv.test"

func newFake() *connectortest.Fake { return connectortest.New("acme", paykit.MethodQR) }

// syncBuf là bộ đệm an toàn cho handler log ghi từ nhiều goroutine.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// running là một server đang chạy trên 127.0.0.1:0.
type running struct {
	addr   string
	cancel context.CancelFunc
	done   chan error
	exited chan struct{} // đóng sau khi serveOn trả về và kết quả đã nằm trong done
}

func start(t *testing.T, c connector.Connector, o Options) *running {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	r := &running{addr: lis.Addr().String(), cancel: cancel, done: make(chan error, 1), exited: make(chan struct{})}
	if o.Logger == nil {
		o.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	go func() { r.done <- serveOn(ctx, c, o, lis); close(r.exited) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-r.exited:
		case <-time.After(10 * time.Second):
			t.Error("Serve không dừng sau khi huỷ ctx")
		}
	})
	return r
}

func dial(t *testing.T, addr string, creds credentials.TransportCredentials) *grpc.ClientConn {
	t.Helper()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(creds))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func tlsClient(t *testing.T, addr string, cfg *tls.Config) connectorpb.ConnectorServiceClient {
	t.Helper()
	return connectorpb.NewConnectorServiceClient(dial(t, addr, credentials.NewTLS(cfg)))
}

func plainClient(t *testing.T, addr string) *grpc.ClientConn {
	t.Helper()
	return dial(t, addr, insecure.NewCredentials())
}

func ctx5(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func createReq() *connectorpb.CreatePaymentRequest {
	return &connectorpb.CreatePaymentRequest{}
}

// mtlsEnv dựng server TLS cho phép "cli.test" và trả cấu hình client đúng.
func mtlsEnv(t *testing.T, f *connectortest.Fake, allowed ...string) (*running, *tlstest.Bundle, *tls.Config) {
	t.Helper()
	b := tlstest.New(t)
	srvCfg, err := connectorrpc.ServerTLS(b.Server(srvName))
	require.NoError(t, err)
	r := start(t, f, Options{TLS: srvCfg, AllowedClients: allowed})
	cli, err := connectorrpc.ClientTLS(b.Client("cli.test"), srvName)
	require.NoError(t, err)
	return r, b, cli
}

func TestServe_RefusesPlaintextByDefault(t *testing.T) {
	err := Serve(context.Background(), newFake(), Options{ListenAddr: "127.0.0.1:0"})
	require.ErrorContains(t, err, "Insecure")
}

func TestServe_EmptyAllowListWithTLSFails(t *testing.T) {
	b := tlstest.New(t)
	cfg, err := connectorrpc.ServerTLS(b.Server(srvName))
	require.NoError(t, err)
	err = Serve(context.Background(), newFake(), Options{ListenAddr: "127.0.0.1:0", TLS: cfg})
	require.ErrorContains(t, err, "AllowedClients")
}

func TestServe_RejectsWeakTLSConfig(t *testing.T) {
	b := tlstest.New(t)
	cfg, err := connectorrpc.ServerTLS(b.Server(srvName))
	require.NoError(t, err)
	weak := cfg.Clone()
	weak.ClientAuth = tls.NoClientCert
	require.Error(t, Serve(context.Background(), newFake(), Options{TLS: weak, AllowedClients: []string{"x"}}))
	old := cfg.Clone()
	old.MinVersion = tls.VersionTLS12
	require.Error(t, Serve(context.Background(), newFake(), Options{TLS: old, AllowedClients: []string{"x"}}))
	require.Error(t, Serve(context.Background(), newFake(), Options{TLS: cfg, Insecure: true, AllowedClients: []string{"x"}}))
}

func TestMTLS_HappyPath(t *testing.T) {
	f := newFake()
	r, _, cliCfg := mtlsEnv(t, f, "cli.test")
	cl := tlsClient(t, r.addr, cliCfg)
	d, err := cl.Describe(ctx5(t), &connectorpb.DescribeRequest{})
	require.NoError(t, err)
	require.Equal(t, "acme", d.GetName())
	resp, err := cl.CreatePayment(ctx5(t), createReq())
	require.NoError(t, err)
	require.Nil(t, resp.GetError())
	require.Equal(t, 1, f.Calls("create"))
}

func TestMTLS_Rejections(t *testing.T) {
	f := newFake()
	r, b, good := mtlsEnv(t, f, "cli.test")
	other := b.OtherCA()

	noCert := good.Clone()
	noCert.GetClientCertificate = nil

	otherCfg, err := connectorrpc.ClientTLS(withCA(other.Client("cli.test"), b.Server("x").CAFile), srvName)
	require.NoError(t, err)

	notAllowed, err := connectorrpc.ClientTLS(b.Client("intruder.test"), srvName)
	require.NoError(t, err)

	wrongName, err := connectorrpc.ClientTLS(b.Client("cli.test"), "wrong.test")
	require.NoError(t, err)

	tls12 := good.Clone()
	tls12.MinVersion, tls12.MaxVersion = tls.VersionTLS12, tls.VersionTLS12

	for name, cfg := range map[string]*tls.Config{
		"no client cert": noCert, "other CA": otherCfg, "not in allow list": notAllowed,
		"wrong server name": wrongName, "TLS 1.2": tls12,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := tlsClient(t, r.addr, cfg).CreatePayment(ctx5(t), createReq())
			require.Error(t, err)
			require.Equal(t, 0, f.Calls("create"))
		})
	}
}

func withCA(f connectorrpc.TLSFiles, ca string) connectorrpc.TLSFiles {
	f.CAFile = ca
	return f
}

func TestMTLS_NotInAllowListIsPermissionDenied(t *testing.T) {
	r, b, _ := mtlsEnv(t, newFake(), "cli.test")
	cfg, err := connectorrpc.ClientTLS(b.Client("intruder.test"), srvName)
	require.NoError(t, err)
	_, err = tlsClient(t, r.addr, cfg).Describe(ctx5(t), &connectorpb.DescribeRequest{})
	require.Equal(t, codes.PermissionDenied, status.Code(err))
}

func TestServe_HealthWithoutAllowList(t *testing.T) {
	r, b, _ := mtlsEnv(t, newFake(), "cli.test")
	cfg, err := connectorrpc.ClientTLS(b.Client("intruder.test"), srvName)
	require.NoError(t, err)
	h := grpc_health_v1.NewHealthClient(dial(t, r.addr, credentials.NewTLS(cfg)))
	resp, err := h.Check(ctx5(t), &grpc_health_v1.HealthCheckRequest{}, grpc.WaitForReady(true))
	require.NoError(t, err)
	require.Equal(t, grpc_health_v1.HealthCheckResponse_SERVING, resp.GetStatus())
}

func TestServe_PanicBecomesInternalAndServerSurvives(t *testing.T) {
	f := newFake()
	f.OnCreate = func(context.Context, connector.CreatePaymentRequest) (connector.CreatePaymentResult, error) {
		panic("boom-secret")
	}
	logs := &syncBuf{}
	r := start(t, f, Options{Insecure: true, Logger: newLogger(logs)})
	cl := connectorpb.NewConnectorServiceClient(plainClient(t, r.addr))

	_, err := cl.CreatePayment(ctx5(t), createReq(), grpc.WaitForReady(true))
	require.Equal(t, codes.Internal, status.Code(err))
	require.NotContains(t, err.Error(), "boom-secret", "không lộ nội dung panic cho client")
	_, err = cl.Describe(ctx5(t), &connectorpb.DescribeRequest{})
	require.NoError(t, err)
	require.Contains(t, logs.String(), "goroutine", "log có stack")
}

func TestServe_LogsNoBodies(t *testing.T) {
	const secret = "sekret.customer@example.invalid"
	f := newFake()
	logs := &syncBuf{}
	r := start(t, f, Options{Insecure: true, Logger: newLogger(logs)})
	cl := connectorpb.NewConnectorServiceClient(plainClient(t, r.addr))
	req := createReq()
	req.Customer = &connectorpb.Customer{Email: secret}
	_, err := cl.CreatePayment(ctx5(t), req, grpc.WaitForReady(true))
	require.NoError(t, err)

	out := logs.String()
	require.Contains(t, out, "CreatePayment")
	require.Contains(t, out, "duration")
	require.NotContains(t, out, secret)
	require.NotContains(t, strings.ToLower(out), "email")
}

func TestServe_LogsErrorKind(t *testing.T) {
	f := newFake()
	f.OnCreate = func(context.Context, connector.CreatePaymentRequest) (connector.CreatePaymentResult, error) {
		return connector.CreatePaymentResult{}, connector.ErrProviderUnavailable
	}
	logs := &syncBuf{}
	r := start(t, f, Options{Insecure: true, Logger: newLogger(logs)})
	cl := connectorpb.NewConnectorServiceClient(plainClient(t, r.addr))
	_, err := cl.CreatePayment(ctx5(t), createReq(), grpc.WaitForReady(true))
	require.NoError(t, err)
	require.Contains(t, logs.String(), "error.kind=ERROR_KIND_PROVIDER_UNAVAILABLE")
}

func TestServe_GracefulShutdownLetsInFlightFinish(t *testing.T) {
	f := newFake()
	entered, release := make(chan struct{}), make(chan struct{})
	f.OnCreate = func(context.Context, connector.CreatePaymentRequest) (connector.CreatePaymentResult, error) {
		close(entered)
		<-release
		return connector.CreatePaymentResult{ProviderRef: "p", Status: connector.StatusPending}, nil
	}
	r := start(t, f, Options{Insecure: true, ShutdownGrace: 30 * time.Second})
	conn := plainClient(t, r.addr)
	cl := connectorpb.NewConnectorServiceClient(conn)

	// Theo dõi health để biết chắc việc tắt đã bắt đầu (NOT_SERVING) trước khi nhả lời gọi.
	wctx, wcancel := context.WithCancel(ctx5(t))
	defer wcancel()
	w, err := grpc_health_v1.NewHealthClient(conn).Watch(wctx, &grpc_health_v1.HealthCheckRequest{}, grpc.WaitForReady(true))
	require.NoError(t, err)
	m, err := w.Recv()
	require.NoError(t, err)
	require.Equal(t, grpc_health_v1.HealthCheckResponse_SERVING, m.GetStatus())

	type result struct {
		resp *connectorpb.CreatePaymentResponse
		err  error
	}
	res := make(chan result, 1)
	go func() {
		resp, err := cl.CreatePayment(ctx5(t), createReq())
		res <- result{resp, err}
	}()
	<-entered

	r.cancel()
	m, err = w.Recv()
	require.NoError(t, err)
	require.Equal(t, grpc_health_v1.HealthCheckResponse_NOT_SERVING, m.GetStatus())
	wcancel() // luồng Watch cũng là lời gọi đang chạy, phải kết thúc thì GracefulStop mới xong

	select {
	case err := <-r.done:
		t.Fatalf("Serve trả về khi lời gọi còn đang chạy: %v", err)
	default:
	}
	close(release)
	got := <-res
	require.NoError(t, got.err)
	require.Equal(t, "p", got.resp.GetProviderRef())
	require.NoError(t, <-r.done)
}

func TestServe_ShutdownGraceForcesStop(t *testing.T) {
	f := newFake()
	entered := make(chan struct{})
	f.OnCreate = func(ctx context.Context, _ connector.CreatePaymentRequest) (connector.CreatePaymentResult, error) {
		close(entered)
		<-ctx.Done()
		return connector.CreatePaymentResult{}, ctx.Err()
	}
	r := start(t, f, Options{Insecure: true, ShutdownGrace: 50 * time.Millisecond})
	cl := connectorpb.NewConnectorServiceClient(plainClient(t, r.addr))
	errc := make(chan error, 1)
	go func() {
		_, err := cl.CreatePayment(ctx5(t), createReq(), grpc.WaitForReady(true))
		errc <- err
	}()
	<-entered
	r.cancel()
	require.NoError(t, <-r.done)
	require.Error(t, <-errc)
}

func newLogger(w *syncBuf) *slog.Logger { return slog.New(slog.NewTextHandler(w, nil)) }
