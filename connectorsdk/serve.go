// Package connectorsdk là bộ khởi chạy tiến trình connector: nhận một connector.Connector,
// phục vụ nó qua gRPC với mTLS bắt buộc, health, log không chứa thân và tắt êm.
//
// Keepalive (hợp đồng với core, không cấu hình được): server chấp nhận client ping cách nhau từ 10 giây,
// kể cả khi không có lời gọi nào đang chạy (core ping mỗi 15 giây, chờ trả lời 5 giây); server tự ping sau
// 30 giây rảnh và đóng kết nối không trả lời trong 10 giây.
package connectorsdk

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"runtime/debug"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"bitbucket.org/hasaki-tech/payment-connector-sdk/connector"
	"bitbucket.org/hasaki-tech/payment-connector-sdk/connectorpb"
	"bitbucket.org/hasaki-tech/payment-connector-sdk/connectorrpc"
)

const (
	healthPrefix         = "/grpc.health.v1.Health/"
	defaultListenAddr    = ":9090"
	defaultMaxStreams    = 1000
	minMaxStreams        = 100
	maxMaxStreams        = 100000
	defaultShutdownGrace = 20 * time.Second
)

// Options cấu hình Serve. Không có TLS thì phải bật Insecure một cách tường minh.
type Options struct {
	ListenAddr           string        // mặc định ":9090"
	TLS                  *tls.Config   // từ connectorrpc.ServerTLS; bắt buộc trừ khi Insecure
	Insecure             bool          // chỉ cho phát triển cục bộ: không mã hoá, không xác thực client
	AllowedClients       []string      // tên DNS SAN của chứng chỉ client được phép; bắt buộc khi có TLS
	MaxConcurrentStreams uint32        // 0 là mặc định 1000; khác 0 thì phải trong [100, 100000]
	ShutdownGrace        time.Duration // thời gian chờ lời gọi đang chạy khi tắt; mặc định 20s
	Logger               *slog.Logger  // mặc định slog.Default()

	keepalive *keepaliveConfig // nil: defaultKeepalive(); chỉ test đặt để rút ngắn thời gian
}

// keepaliveConfig là keepalive phía server. Core ping mỗi 15 giây khi rảnh (kể cả không có lời gọi nào)
// để phát hiện kết nối chết sau NAT hay cân bằng tải; mặc định của gRPC đóng client ping dày hơn 5 phút
// bằng GOAWAY too_many_pings. Cho phép ping cách nhau từ 10 giây, kể cả khi không có luồng; server tự
// ping sau 30 giây rảnh và đóng kết nối không trả lời trong 10 giây.
type keepaliveConfig struct {
	enforce keepalive.EnforcementPolicy
	params  keepalive.ServerParameters
}

func defaultKeepalive() keepaliveConfig {
	return keepaliveConfig{
		enforce: keepalive.EnforcementPolicy{MinTime: 10 * time.Second, PermitWithoutStream: true},
		params:  keepalive.ServerParameters{Time: 30 * time.Second, Timeout: 10 * time.Second},
	}
}

// Serve phục vụ c cho tới khi ctx huỷ, rồi tắt êm trong ShutdownGrace và ép dừng nếu quá hạn.
// Cấu hình sai (không TLS mà không Insecure, TLS yếu, danh sách client rỗng) lỗi ngay, trước khi nghe.
func Serve(ctx context.Context, c connector.Connector, o Options) error {
	if err := o.validate(); err != nil {
		return err
	}
	addr := o.ListenAddr
	if addr == "" {
		addr = defaultListenAddr
	}
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("connectorsdk: nghe %s: %w", addr, err)
	}
	return ServeListener(ctx, c, o, lis)
}

func (o Options) validate() error {
	switch {
	case o.TLS == nil && !o.Insecure:
		return errors.New("connectorsdk: thiếu TLS; chỉ chạy không mã hoá khi đặt Insecure một cách tường minh")
	case o.TLS != nil && o.Insecure:
		return errors.New("connectorsdk: TLS và Insecure không đi cùng nhau")
	case o.TLS != nil && o.TLS.MinVersion < tls.VersionTLS13:
		return errors.New("connectorsdk: TLS phải từ 1.3 trở lên")
	case o.TLS != nil && o.TLS.ClientAuth != tls.RequireAndVerifyClientCert:
		return errors.New("connectorsdk: TLS phải bắt buộc chứng chỉ client (RequireAndVerifyClientCert)")
	case o.TLS != nil && o.TLS.ClientCAs == nil:
		return errors.New("connectorsdk: TLS thiếu ClientCAs để kiểm chứng chỉ client")
	case o.TLS != nil && o.TLS.GetConfigForClient != nil:
		return errors.New("connectorsdk: TLS có GetConfigForClient có thể đè cấu hình kiểm client; không được dùng")
	case o.TLS != nil && len(o.AllowedClients) == 0:
		return errors.New("connectorsdk: AllowedClients rỗng; liệt kê tên DNS của client được phép")
	case o.MaxConcurrentStreams != 0 && !validStreams(o.MaxConcurrentStreams):
		return fmt.Errorf("connectorsdk: MaxConcurrentStreams=%d ngoài khoảng [%d, %d]", o.MaxConcurrentStreams, minMaxStreams, maxMaxStreams)
	}
	return nil
}

func validStreams(n uint32) bool { return n >= minMaxStreams && n <= maxMaxStreams }

// ServeListener là Serve trên một listener có sẵn (ListenAddr bị bỏ qua), để test dùng cổng :0.
// Listener được đóng khi hàm trả về.
func ServeListener(ctx context.Context, c connector.Connector, o Options, lis net.Listener) error {
	if err := o.validate(); err != nil {
		_ = lis.Close()
		return err
	}
	log := o.Logger
	if log == nil {
		log = slog.Default()
	}
	streams := o.MaxConcurrentStreams
	if streams == 0 {
		streams = defaultMaxStreams
	}
	grace := o.ShutdownGrace
	if grace <= 0 {
		grace = defaultShutdownGrace
	}

	allowed := make(map[string]bool, len(o.AllowedClients))
	for _, n := range o.AllowedClients {
		allowed[strings.ToLower(n)] = true
	}
	ka := defaultKeepalive()
	if o.keepalive != nil {
		ka = *o.keepalive
	}
	opts := []grpc.ServerOption{
		grpc.MaxConcurrentStreams(streams),
		grpc.KeepaliveEnforcementPolicy(ka.enforce),
		grpc.KeepaliveParams(ka.params),
		// Thứ tự: log ngoài cùng để thấy cả lời gọi bị từ chối và panic đã thành Internal. Kiểm tên chạy sau
		// chứng chỉ và trước handler: core gọi nhầm tiến trình thì lời gọi bị từ chối khi chưa chạm connector.
		grpc.ChainUnaryInterceptor(logInterceptor(log), authInterceptor(log, o.TLS != nil, allowed),
			connectorrpc.IdentityInterceptor(c.Name()), recoverInterceptor(log)),
	}
	if o.TLS != nil {
		opts = append(opts, grpc.Creds(credentials.NewTLS(o.TLS)))
	}
	srv := grpc.NewServer(opts...)
	connectorpb.RegisterConnectorServiceServer(srv, connectorrpc.NewServer(c))
	hs := health.NewServer()
	grpc_health_v1.RegisterHealthServer(srv, hs)
	hs.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)

	if o.Insecure {
		log.Warn("connector chạy KHÔNG mã hoá và không xác thực client (Insecure); chỉ dùng khi phát triển")
	}

	served := make(chan struct{}) // đóng khi srv.Serve trả về, để goroutine tắt không chờ ctx mãi
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		select {
		case <-ctx.Done():
		case <-served:
			return
		}
		hs.Shutdown()
		graceful := make(chan struct{})
		go func() { srv.GracefulStop(); close(graceful) }()
		t := time.NewTimer(grace)
		defer t.Stop()
		// Luồng Watch của health cũng là lời gọi đang chạy: client không đóng nó thì chờ hết grace.
		select {
		case <-graceful:
		case <-t.C:
			srv.Stop()
			<-graceful
		}
	}()

	err := srv.Serve(lis)
	close(served)
	<-stopped
	if ctx.Err() != nil {
		return nil
	}
	if err == nil {
		err = errors.New("connectorsdk: server dừng ngoài ý muốn")
	}
	srv.Stop()
	return err
}

func recoverInterceptor(log *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (resp any, err error) {
		defer func() {
			if r := recover(); r != nil {
				// Không ghi giá trị panic: có thể chứa thân request hay credential.
				log.Error("connector panic", "rpc", info.FullMethod, "panic_type", fmt.Sprintf("%T", r), "stack", string(debug.Stack()))
				resp, err = nil, status.Error(codes.Internal, "lỗi nội bộ của connector")
			}
		}()
		return h(ctx, req)
	}
}

// authInterceptor chỉ cho client có một tên DNS SAN nằm trong allowed. Chạy sau bước TLS đã kiểm chuỗi
// chứng chỉ; Insecure thì bỏ qua (không có danh tính). Health được miễn để bộ giám sát kiểm tra mà không cần là client.
func authInterceptor(log *slog.Logger, tlsOn bool, allowed map[string]bool) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
		if tlsOn && !strings.HasPrefix(info.FullMethod, healthPrefix) && !clientAllowed(ctx, allowed) {
			logDenied(ctx, log, info.FullMethod)
			return nil, status.Error(codes.PermissionDenied, "client không nằm trong danh sách được phép")
		}
		return h(ctx, req)
	}
}

// logDenied ghi DNS SAN và serial của chứng chỉ bị từ chối để vận hành biết ai gọi; không ghi khoá hay cả chứng chỉ.
func logDenied(ctx context.Context, log *slog.Logger, method string) {
	var dns []string
	var serial string
	if p, ok := peer.FromContext(ctx); ok {
		if ti, ok := p.AuthInfo.(credentials.TLSInfo); ok && len(ti.State.PeerCertificates) > 0 {
			leaf := ti.State.PeerCertificates[0]
			dns, serial = leaf.DNSNames, leaf.SerialNumber.Text(16)
		}
	}
	log.Warn("từ chối client không nằm trong danh sách được phép", "rpc", method, "dns_sans", dns, "serial", serial)
}

func clientAllowed(ctx context.Context, allowed map[string]bool) bool {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return false
	}
	ti, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok || len(ti.State.PeerCertificates) == 0 {
		return false
	}
	for _, n := range ti.State.PeerCertificates[0].DNSNames {
		if allowed[strings.ToLower(n)] {
			return true
		}
	}
	return false
}

// logInterceptor ghi tên RPC, thời lượng và loại lỗi. Cố ý không đụng tới thân request/response (có PII).
func logInterceptor(log *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
		start := time.Now()
		resp, err := h(ctx, req)
		log.Info("rpc", "rpc", info.FullMethod, "duration", time.Since(start), "error.kind", errorKind(resp, err))
		return resp, err
	}
}

// errorKind là loại lỗi nghiệp vụ trong thân phản hồi, hoặc mã gRPC nếu lời gọi lỗi vận chuyển; rỗng nếu thành công.
func errorKind(resp any, err error) string {
	if err != nil {
		return "grpc:" + status.Code(err).String()
	}
	if e, ok := resp.(interface {
		GetError() *connectorpb.ConnectorError
	}); ok && e.GetError() != nil {
		return e.GetError().GetKind().String()
	}
	return ""
}
