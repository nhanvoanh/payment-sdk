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
	"google.golang.org/grpc/metadata"
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
	exited chan struct{} // đóng sau khi ServeListener trả về và kết quả đã nằm trong done
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
	go func() { r.done <- ServeListener(ctx, c, o, lis); close(r.exited) }()
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
func mtlsEnv(t *testing.T, f *connectortest.Fake, logs *syncBuf, allowed ...string) (*running, *tlstest.Bundle, *tls.Config) {
	t.Helper()
	b := tlstest.New(t)
	srvCfg, err := connectorrpc.ServerTLS(b.Server(srvName))
	require.NoError(t, err)
	o := Options{TLS: srvCfg, AllowedClients: allowed}
	if logs != nil {
		o.Logger = newLogger(logs)
	}
	r := start(t, f, o)
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
	noCAs := cfg.Clone()
	noCAs.ClientCAs = nil
	require.Error(t, Serve(context.Background(), newFake(), Options{TLS: noCAs, AllowedClients: []string{"x"}}))
	override := cfg.Clone()
	override.GetConfigForClient = func(*tls.ClientHelloInfo) (*tls.Config, error) { return nil, nil }
	require.Error(t, Serve(context.Background(), newFake(), Options{TLS: override, AllowedClients: []string{"x"}}))
}

func TestMTLS_HappyPath(t *testing.T) {
	f := newFake()
	r, _, cliCfg := mtlsEnv(t, f, nil, "cli.test")
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
	r, b, good := mtlsEnv(t, f, nil, "cli.test")
	other := b.OtherCA()

	noCert := good.Clone()
	noCert.GetClientCertificate = nil

	otherCfg, err := connectorrpc.ClientTLS(withCA(other.Client("cli.test"), b.Server("x").CAFile), srvName)
	require.NoError(t, err)

	wrongName, err := connectorrpc.ClientTLS(b.Client("cli.test"), "wrong.test")
	require.NoError(t, err)

	tls12 := good.Clone()
	tls12.MinVersion, tls12.MaxVersion = tls.VersionTLS12, tls.VersionTLS12

	for name, cfg := range map[string]*tls.Config{
		"no client cert": noCert, "other CA": otherCfg, "wrong server name": wrongName, "TLS 1.2": tls12,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := tlsClient(t, r.addr, cfg).CreatePayment(ctx5(t), createReq())
			require.Equal(t, codes.Unavailable, status.Code(err), "từ chối ở lớp bắt tay: %v", err)
			require.Equal(t, 0, f.Calls("create"))
		})
	}
}

func withCA(f connectorrpc.TLSFiles, ca string) connectorrpc.TLSFiles {
	f.CAFile = ca
	return f
}

func TestMTLS_NotInAllowListIsPermissionDenied(t *testing.T) {
	logs := &syncBuf{}
	r, b, _ := mtlsEnv(t, newFake(), logs, "cli.test")
	cfg, err := connectorrpc.ClientTLS(b.Client("intruder.test"), srvName)
	require.NoError(t, err)
	_, err = tlsClient(t, r.addr, cfg).Describe(ctx5(t), &connectorpb.DescribeRequest{})
	require.Equal(t, codes.PermissionDenied, status.Code(err))
	require.Contains(t, logs.String(), "intruder.test")
	require.Contains(t, logs.String(), "serial=")
	require.NotContains(t, logs.String(), "PRIVATE KEY")
}

func TestServe_ReturnsWhenListenerFails(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	require.NoError(t, lis.Close())
	done := make(chan error, 1)
	go func() {
		done <- ServeListener(context.Background(), newFake(), Options{Insecure: true, Logger: discard()}, lis)
	}()
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("ServeListener treo khi Serve lỗi mà ctx chưa huỷ")
	}
}

func TestServe_WarnsWhenInsecure(t *testing.T) {
	logs := &syncBuf{}
	r := start(t, newFake(), Options{Insecure: true, Logger: newLogger(logs)})
	_, err := connectorpb.NewConnectorServiceClient(plainClient(t, r.addr)).Describe(ctx5(t), &connectorpb.DescribeRequest{}, grpc.WaitForReady(true))
	require.NoError(t, err)
	require.Equal(t, 1, strings.Count(logs.String(), "level=WARN"))
	require.Contains(t, logs.String(), "Insecure")
}

func TestServe_HealthWithoutAllowList(t *testing.T) {
	r, b, _ := mtlsEnv(t, newFake(), nil, "cli.test")
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
	require.NotContains(t, logs.String(), "boom-secret", "không ghi giá trị panic")
	require.Contains(t, logs.String(), "panic_type=string")
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

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newLogger(w *syncBuf) *slog.Logger { return slog.New(slog.NewTextHandler(w, nil)) }

// Tên trong metadata khác tên connector: từ chối trước handler, có dấu để proxy biết chắc chưa xử lý. Health không bị kiểm.
func TestServe_RejectsWrongConnectorNameBeforeHandler(t *testing.T) {
	f := newFake()
	r, _, cliCfg := mtlsEnv(t, f, nil, "cli.test")
	conn := dial(t, r.addr, credentials.NewTLS(cliCfg))
	cl := connectorpb.NewConnectorServiceClient(conn)

	var trailer metadata.MD
	wrong := metadata.AppendToOutgoingContext(ctx5(t), connectorrpc.MetadataConnectorName, "beta")
	_, err := cl.CreatePayment(wrong, createReq(), grpc.Trailer(&trailer))
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	require.Equal(t, []string{connectorrpc.IdentityMismatch}, trailer.Get(connectorrpc.MetadataIdentity))
	require.Equal(t, 0, f.Calls("create"), "handler không được chạy")

	_, err = grpc_health_v1.NewHealthClient(conn).Check(wrong, &grpc_health_v1.HealthCheckRequest{})
	require.NoError(t, err, "health không bị kiểm tên")

	right := metadata.AppendToOutgoingContext(ctx5(t), connectorrpc.MetadataConnectorName, "acme")
	_, err = cl.CreatePayment(right, createReq())
	require.NoError(t, err)
	_, err = cl.CreatePayment(ctx5(t), createReq()) // client cũ không gửi metadata
	require.NoError(t, err)
	require.Equal(t, 2, f.Calls("create"))
}

// Khoảng [100, 100000] chỉ áp cho biến môi trường: giá trị đặt bằng code được nhận như trước khi có biến đó.
func TestServe_AcceptsAnyProgrammaticMaxConcurrentStreams(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cấu hình hợp lệ thì Serve trả về ngay thay vì chạy mãi
	for _, n := range []uint32{1, 99, 100001} {
		err := Serve(ctx, newFake(), Options{ListenAddr: "127.0.0.1:0", Insecure: true, MaxConcurrentStreams: n})
		require.NoError(t, err, n)
	}
}

// Quá MaxConcurrentStreams lời gọi đồng thời trên một kết nối thì lời gọi dư chờ ở client, không chạm handler.
func TestServe_MaxConcurrentStreamsLimitsInFlightCalls(t *testing.T) {
	const limit = 100
	f := newFake()
	var mu sync.Mutex
	inFlight := 0
	release := make(chan struct{})
	f.OnCreate = func(context.Context, connector.CreatePaymentRequest) (connector.CreatePaymentResult, error) {
		mu.Lock()
		inFlight++
		mu.Unlock()
		<-release
		return connector.CreatePaymentResult{Status: connector.StatusPending}, nil
	}
	count := func() int { mu.Lock(); defer mu.Unlock(); return inFlight }
	r := start(t, f, Options{Insecure: true, MaxConcurrentStreams: limit, ShutdownGrace: time.Second})
	cl := connectorpb.NewConnectorServiceClient(plainClient(t, r.addr))
	var wg sync.WaitGroup
	for range limit + 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = cl.CreatePayment(ctx5(t), createReq(), grpc.WaitForReady(true))
		}()
	}
	require.Eventually(t, func() bool { return count() == limit }, 5*time.Second, 5*time.Millisecond)
	time.Sleep(100 * time.Millisecond)
	require.Equal(t, limit, count(), "lời gọi vượt giới hạn không được tới handler")
	close(release)
	wg.Wait()
	require.Equal(t, limit+5, count())
}
