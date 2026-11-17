// Package connectorsdk là bộ khởi chạy tiến trình connector: nhận một connector.Connector,
// phục vụ nó qua gRPC với mTLS bắt buộc, health, log không chứa thân và tắt êm.
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
	defaultShutdownGrace = 20 * time.Second
)

// Options cấu hình Serve. Không có TLS thì phải bật Insecure một cách tường minh.
type Options struct {
	ListenAddr           string        // mặc định ":9090"
	TLS                  *tls.Config   // từ connectorrpc.ServerTLS; bắt buộc trừ khi Insecure
	Insecure             bool          // chỉ cho phát triển cục bộ: không mã hoá, không xác thực client
	AllowedClients       []string      // tên DNS SAN của chứng chỉ client được phép; bắt buộc khi có TLS
	MaxConcurrentStreams uint32        // mặc định 1000
	ShutdownGrace        time.Duration // thời gian chờ lời gọi đang chạy khi tắt; mặc định 20s
	Logger               *slog.Logger  // mặc định slog.Default()
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
	return serveOn(ctx, c, o, lis)
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
	case o.TLS != nil && len(o.AllowedClients) == 0:
		return errors.New("connectorsdk: AllowedClients rỗng; liệt kê tên DNS của client được phép")
	}
	return nil
}

// serveOn là thân của Serve trên một listener có sẵn (test dùng cổng :0).
func serveOn(ctx context.Context, c connector.Connector, o Options, lis net.Listener) error {
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
	opts := []grpc.ServerOption{
		grpc.MaxConcurrentStreams(streams),
		// Thứ tự: log ngoài cùng để thấy cả lời gọi bị từ chối và panic đã thành Internal.
		grpc.ChainUnaryInterceptor(logInterceptor(log), authInterceptor(o.TLS != nil, allowed), recoverInterceptor(log)),
	}
	if o.TLS != nil {
		opts = append(opts, grpc.Creds(credentials.NewTLS(o.TLS)))
	}
	srv := grpc.NewServer(opts...)
	connectorpb.RegisterConnectorServiceServer(srv, connectorrpc.NewServer(c))
	hs := health.NewServer()
	grpc_health_v1.RegisterHealthServer(srv, hs)
	hs.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)

	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		<-ctx.Done()
		hs.Shutdown()
		graceful := make(chan struct{})
		go func() { srv.GracefulStop(); close(graceful) }()
		t := time.NewTimer(grace)
		defer t.Stop()
		select {
		case <-graceful:
		case <-t.C:
			srv.Stop()
			<-graceful
		}
	}()

	err := srv.Serve(lis)
	// Serve trả về khi server bị dừng; nếu lý do là lỗi nghe thì ctx chưa huỷ, ép dừng để goroutine thoát.
	if ctx.Err() == nil {
		srv.Stop()
	}
	<-stopped
	if ctx.Err() != nil {
		return nil
	}
	return err
}

func recoverInterceptor(log *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (resp any, err error) {
		defer func() {
			if r := recover(); r != nil {
				log.Error("connector panic", "rpc", info.FullMethod, "panic", fmt.Sprint(r), "stack", string(debug.Stack()))
				resp, err = nil, status.Error(codes.Internal, "lỗi nội bộ của connector")
			}
		}()
		return h(ctx, req)
	}
}

// authInterceptor chỉ cho client có một tên DNS SAN nằm trong allowed. Chạy sau bước TLS đã kiểm chuỗi
// chứng chỉ; Insecure thì bỏ qua (không có danh tính). Health được miễn để bộ giám sát kiểm tra mà không cần là client.
func authInterceptor(tlsOn bool, allowed map[string]bool) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
		if tlsOn && !strings.HasPrefix(info.FullMethod, healthPrefix) && !clientAllowed(ctx, allowed) {
			return nil, status.Error(codes.PermissionDenied, "client không nằm trong danh sách được phép")
		}
		return h(ctx, req)
	}
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
