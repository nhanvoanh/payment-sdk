package connectorrpc

import (
	"context"
	"strings"
	"sync"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/stats"

	"github.com/nhanvoanh/payment-sdk/oteltrace"
)

const (
	describeMethod   = "/hasaki.payment.connector.v1.ConnectorService/Describe"
	healthPrefix     = "/grpc.health.v1.Health/"
	connectorAttrKey = "payment.connector"
)

type untracedKey struct{}

// TracingDialOption gắn stats handler otelgrpc phía client, chỉ trace RPC nghiệp vụ khi ctx đã có span cha;
// mọi span mang payment.connector=<connector>. Handler otelgrpc được tạo lười ở kết nối đầu tiên (TagConn) nên dùng
// TracerProvider toàn cục hiện hành (oteltrace.Setup có thể chạy trước hoặc sau khi dial).
func TracingDialOption(connector string) grpc.DialOption {
	return grpc.WithStatsHandler(&filteredHandler{
		decide: func(ctx context.Context, method string) bool {
			return tracedMethod(method) && oteltrace.HasParent(ctx)
		},
		build: func() stats.Handler {
			return otelgrpc.NewClientHandler(
				otelgrpc.WithPropagators(propagation.TraceContext{}),
				otelgrpc.WithSpanAttributes(attribute.String(connectorAttrKey, connector)),
			)
		},
	})
}

// TracingServerOption gắn stats handler otelgrpc phía server, chỉ trace RPC nghiệp vụ mà metadata đến mang
// traceparent hợp lệ (không tin gì khác).
func TracingServerOption() grpc.ServerOption {
	return grpc.StatsHandler(&filteredHandler{
		decide: func(ctx context.Context, method string) bool {
			return tracedMethod(method) && incomingTraceparentValid(ctx)
		},
		build: func() stats.Handler {
			return otelgrpc.NewServerHandler(otelgrpc.WithPropagators(propagation.TraceContext{}))
		},
	})
}

// tracedMethod: Describe và health không bao giờ tạo span.
func tracedMethod(full string) bool {
	return full != describeMethod && !strings.HasPrefix(full, healthPrefix)
}

func incomingTraceparentValid(ctx context.Context) bool {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return false
	}
	sctx := propagation.TraceContext{}.Extract(context.Background(), mdCarrier(md))
	return oteltrace.HasParent(sctx)
}

type mdCarrier metadata.MD

func (c mdCarrier) Get(key string) string {
	if v := metadata.MD(c).Get(key); len(v) > 0 {
		return v[0]
	}
	return ""
}
func (c mdCarrier) Set(key, value string) { metadata.MD(c).Set(key, value) }
func (c mdCarrier) Keys() []string {
	keys := make([]string, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}
	return keys
}

// filteredHandler bọc handler otelgrpc: TagRPC quyết định trace hay không; RPC không trace được đánh dấu
// trong ctx và bị bỏ qua ở HandleRPC.
type filteredHandler struct {
	decide func(ctx context.Context, method string) bool
	build  func() stats.Handler
	once   sync.Once
	inner  stats.Handler
}

func (h *filteredHandler) handler() stats.Handler {
	h.once.Do(func() { h.inner = h.build() })
	return h.inner
}

func (h *filteredHandler) TagRPC(ctx context.Context, info *stats.RPCTagInfo) context.Context {
	if !h.decide(ctx, info.FullMethodName) {
		return context.WithValue(ctx, untracedKey{}, h)
	}
	return h.handler().TagRPC(ctx, info)
}

func (h *filteredHandler) HandleRPC(ctx context.Context, s stats.RPCStats) {
	// Marker gắn theo danh tính handler: ctx thừa kế từ RPC khác (handler server lồng client) không bị bỏ qua nhầm.
	if ctx.Value(untracedKey{}) == h {
		return
	}
	h.handler().HandleRPC(ctx, s)
}

func (h *filteredHandler) TagConn(ctx context.Context, info *stats.ConnTagInfo) context.Context {
	return h.handler().TagConn(ctx, info)
}

func (h *filteredHandler) HandleConn(ctx context.Context, s stats.ConnStats) {
	h.handler().HandleConn(ctx, s)
}
