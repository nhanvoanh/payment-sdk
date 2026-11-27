package connectorrpc_test

import (
	"context"
	"net"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"

	"github.com/nhanvoanh/payment-sdk/connectorpb"
	"github.com/nhanvoanh/payment-sdk/connectorrpc"
	"github.com/nhanvoanh/payment-sdk/connectortest"
	"github.com/nhanvoanh/payment-sdk/oteltrace/oteltracetest"
	"github.com/nhanvoanh/payment-sdk/paykit"
)

// serve chạy svc (cùng health) sau TracingServerOption trên bufconn; stop đóng conn rồi dừng server và chờ mọi handler
// xong, để span server (stats.End chạy sau WriteStatus) đã kết thúc trước khi test đọc recorder.
func serve(t *testing.T, svc connectorpb.ConnectorServiceServer, dial ...grpc.DialOption) (*grpc.ClientConn, func()) {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer(connectorrpc.TracingServerOption(), grpc.WaitForHandlers(true))
	connectorpb.RegisterConnectorServiceServer(srv, svc)
	healthpb.RegisterHealthServer(srv, health.NewServer())
	go func() { _ = srv.Serve(lis) }()
	conn, err := grpc.NewClient("passthrough:///bufnet", append([]grpc.DialOption{
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials())}, dial...)...)
	require.NoError(t, err)
	var once sync.Once
	stop := func() { once.Do(func() { _ = conn.Close(); srv.Stop() }) }
	t.Cleanup(stop)
	return conn, stop
}

func fakeSvc() connectorpb.ConnectorServiceServer {
	return connectorrpc.NewServer(connectortest.New("zalopay", paykit.MethodQR))
}

func tracedConn(t *testing.T) (*grpc.ClientConn, func()) {
	t.Helper()
	return serve(t, fakeSvc(), connectorrpc.TracingDialOption("zalopay"))
}

func createReq() *connectorpb.CreatePaymentRequest {
	return &connectorpb.CreatePaymentRequest{TransactionCode: "T1", Money: &connectorpb.Money{Amount: 1, Currency: "VND"},
		Customer: &connectorpb.Customer{Name: "SENTINEL"}}
}

func parentCtx() (context.Context, trace.Span) {
	return otel.Tracer("test").Start(context.Background(), "parent")
}

func TestTracing_ClientAndServerSpansLinked(t *testing.T) {
	rec := oteltracetest.Install(t)
	conn, stop := tracedConn(t)
	ctx, parent := parentCtx()
	_, err := connectorpb.NewConnectorServiceClient(conn).CreatePayment(ctx, createReq())
	require.NoError(t, err)
	parent.End()
	stop()

	var client, server sdktrace.ReadOnlySpan
	for _, s := range rec.Ended() {
		switch s.SpanKind() {
		case trace.SpanKindClient:
			client = s
		case trace.SpanKindServer:
			server = s
		}
	}
	require.NotNil(t, client)
	require.NotNil(t, server)
	assert.Equal(t, client.SpanContext().TraceID(), server.SpanContext().TraceID())
	assert.Equal(t, parent.SpanContext().TraceID(), client.SpanContext().TraceID())
	assert.Equal(t, client.SpanContext().SpanID(), server.Parent().SpanID())
	assert.Contains(t, client.Attributes(), attribute.String("payment.connector", "zalopay"))
	assert.Len(t, rec.Ended(), 3) // parent + client + server
}

func TestTracing_NoParentNoSpans(t *testing.T) {
	rec := oteltracetest.Install(t)
	conn, stop := tracedConn(t)
	_, err := connectorpb.NewConnectorServiceClient(conn).CreatePayment(context.Background(), createReq())
	require.NoError(t, err)
	stop()
	assert.Empty(t, rec.Ended())
}

func TestTracing_DescribeNeverTraced(t *testing.T) {
	rec := oteltracetest.Install(t)
	conn, stop := tracedConn(t)
	ctx, parent := parentCtx()
	_, err := connectorpb.NewConnectorServiceClient(conn).Describe(ctx, &connectorpb.DescribeRequest{})
	require.NoError(t, err)
	_, err = healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{})
	require.NoError(t, err)
	parent.End()
	stop()
	assert.Len(t, rec.Ended(), 1) // only the parent
}

func TestTracing_ServerIgnoresMetadataWithoutTraceparent(t *testing.T) {
	rec := oteltracetest.Install(t)
	conn, stop := tracedConn(t)
	ctx := metadata.AppendToOutgoingContext(context.Background(), "x-foo", "bar", "tracestate", "a=b")
	_, err := connectorpb.NewConnectorServiceClient(conn).CreatePayment(ctx, createReq())
	require.NoError(t, err)
	stop()
	assert.Empty(t, rec.Ended())
}

func TestTracing_ServerIgnoresInvalidTraceparent(t *testing.T) {
	rec := oteltracetest.Install(t)
	conn, stop := tracedConn(t)
	ctx := metadata.AppendToOutgoingContext(context.Background(), "traceparent", "garbage")
	_, err := connectorpb.NewConnectorServiceClient(conn).CreatePayment(ctx, createReq())
	require.NoError(t, err)
	stop()
	assert.Empty(t, rec.Ended())
}

func TestTracing_NoMessagesRecorded(t *testing.T) {
	rec := oteltracetest.Install(t)
	conn, stop := tracedConn(t)
	ctx, parent := parentCtx()
	req := createReq()
	req.Customer.Name = "SENTINEL"
	_, err := connectorpb.NewConnectorServiceClient(conn).Charge(ctx, &connectorpb.ChargeRequest{
		TransactionCode: "T1", Money: &connectorpb.Money{Amount: 1, Currency: "VND"}, Method: "qr",
		Customer: &connectorpb.Customer{Name: "SENTINEL"}, PaymentCode: "SENTINEL"})
	_ = err // lỗi từ fake không quan trọng ở đây
	_, _ = connectorpb.NewConnectorServiceClient(conn).CreatePayment(ctx, req)
	parent.End()
	stop()
	spans := rec.Ended()
	require.GreaterOrEqual(t, len(spans), 3)
	assert.Empty(t, oteltracetest.Leaks(spans, "SENTINEL"))
}

func TestTracing_ServerNeverTracesDescribeOrHealthEvenWithTraceparent(t *testing.T) {
	rec := oteltracetest.Install(t)
	conn, stop := serve(t, fakeSvc()) // không có dial option: traceparent do test tự gửi
	_, parent := parentCtx()
	tp := "00-" + parent.SpanContext().TraceID().String() + "-" + parent.SpanContext().SpanID().String() + "-01"
	ctx := metadata.AppendToOutgoingContext(context.Background(), "traceparent", tp)
	_, err := connectorpb.NewConnectorServiceClient(conn).Describe(ctx, &connectorpb.DescribeRequest{})
	require.NoError(t, err)
	_, err = healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{})
	require.NoError(t, err)
	stop()
	assert.Empty(t, rec.Ended())
}

// describeNested là handler Describe (untraced phía server) mở span rồi gọi tiếp qua conn có TracingDialOption.
type describeNested struct {
	connectorpb.ConnectorServiceServer
	next *grpc.ClientConn
}

func (d describeNested) Describe(ctx context.Context, _ *connectorpb.DescribeRequest) (*connectorpb.DescribeResponse, error) {
	ctx, sp := otel.Tracer("test").Start(ctx, "nested-parent")
	defer sp.End()
	_, err := connectorpb.NewConnectorServiceClient(d.next).CreatePayment(ctx, createReq())
	return &connectorpb.DescribeResponse{}, err
}

func TestTracing_SkipMarkerNotInheritedByNestedRPC(t *testing.T) {
	rec := oteltracetest.Install(t)
	inner, stopInner := tracedConn(t)
	outer, stopOuter := serve(t, describeNested{fakeSvc(), inner})
	_, err := connectorpb.NewConnectorServiceClient(outer).Describe(context.Background(), &connectorpb.DescribeRequest{})
	require.NoError(t, err)
	stopOuter()
	stopInner()
	var clients int
	for _, s := range rec.Ended() {
		if s.SpanKind() == trace.SpanKindClient {
			clients++
		}
	}
	assert.Equal(t, 1, clients, "nested client span must end")
	assert.Len(t, rec.Started(), len(rec.Ended()), "no span may be left unended")
}
