package connectorrpc_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"bitbucket.org/hasaki-tech/payment-connector-sdk/connector"
	"bitbucket.org/hasaki-tech/payment-connector-sdk/connectorpb"
	"bitbucket.org/hasaki-tech/payment-connector-sdk/connectorrpc"
	"bitbucket.org/hasaki-tech/payment-connector-sdk/connectorrpc/grpctest"
	"bitbucket.org/hasaki-tech/payment-connector-sdk/paykit"
)

const testReadyWait = 200 * time.Millisecond

func create(ctx context.Context, c *connectorrpc.Client) error {
	_, err := c.CreatePayment(ctx, connector.CreatePaymentRequest{TransactionCode: "T"})
	return err
}

func TestClient_NotReadySendsNothing(t *testing.T) {
	f := newFake()
	srv := grpctest.New(t, connectorrpc.NewServer(f))
	c := connectorrpc.NewClient("acme", srv.Conn, connectorrpc.ClientOptions{ReadyWait: testReadyWait})
	if _, err := c.Describe(context.Background()); err != nil {
		t.Fatal(err)
	}
	srv.Kill(t)

	start := time.Now()
	err := create(context.Background(), c)
	if !errors.Is(err, connector.ErrProviderUnavailable) {
		t.Fatalf("muốn ErrProviderUnavailable, được %v", err)
	}
	if errors.Is(err, connector.ErrUnknownOutcome) {
		t.Errorf("không được vừa unavailable vừa unknown: %v", err)
	}
	if d := time.Since(start); d > testReadyWait+500*time.Millisecond {
		t.Errorf("chờ quá lâu: %v", d)
	}
	if n := f.Calls("create"); n != 0 {
		t.Errorf("không được gửi, nhưng connector nhận %d lần", n)
	}
}

func TestClient_ParseNotReadyIsUnknown(t *testing.T) {
	srv := grpctest.New(t, connectorrpc.NewServer(newFake()))
	c := connectorrpc.NewClient("acme", srv.Conn, connectorrpc.ClientOptions{ReadyWait: testReadyWait})
	if _, err := c.Describe(context.Background()); err != nil {
		t.Fatal(err)
	}
	srv.Kill(t)
	_, err := c.ParseWebhook(context.Background(), connector.RawWebhook{})
	if !errors.Is(err, connector.ErrUnknownOutcome) || errors.Is(err, connector.ErrProviderUnavailable) {
		t.Fatalf("parse webhook mất kết nối phải là unknown: %v", err)
	}
}

func TestClient_ServerDiesMidCall(t *testing.T) {
	f := newFake()
	entered := make(chan struct{})
	f.OnCreate = func(ctx context.Context, _ connector.CreatePaymentRequest) (connector.CreatePaymentResult, error) {
		close(entered)
		<-ctx.Done()
		return connector.CreatePaymentResult{}, ctx.Err()
	}
	srv := grpctest.New(t, connectorrpc.NewServer(f))
	c := connectorrpc.NewClient("acme", srv.Conn, connectorrpc.ClientOptions{})
	if _, err := c.Describe(context.Background()); err != nil {
		t.Fatal(err)
	}
	errc := make(chan error, 1)
	go func() { errc <- create(context.Background(), c) }()
	<-entered // lời gọi đã tới connector rồi mới dừng server
	srv.Server.Stop()

	err := <-errc
	if !errors.Is(err, connector.ErrUnknownOutcome) {
		t.Fatalf("muốn ErrUnknownOutcome, được %v", err)
	}
	if errors.Is(err, connector.ErrProviderUnavailable) {
		t.Fatalf("đã gửi rồi, không được trả unavailable: %v", err)
	}
	if !strings.Contains(err.Error(), "Unavailable") {
		t.Errorf("thông điệp phải giữ mã gRPC: %v", err)
	}
}

func TestClient_DeadlineIsUnknown(t *testing.T) {
	f := newFake()
	entered := make(chan struct{})
	f.OnCreate = func(ctx context.Context, _ connector.CreatePaymentRequest) (connector.CreatePaymentResult, error) {
		close(entered)
		<-ctx.Done()
		return connector.CreatePaymentResult{}, ctx.Err()
	}
	c := grpctest.Loopback(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	err := create(ctx, c)
	<-entered
	if !errors.Is(err, connector.ErrUnknownOutcome) || errors.Is(err, connector.ErrProviderUnavailable) {
		t.Fatalf("hết hạn phải là unknown: %v", err)
	}
}

func TestClient_CallTimeoutAppliesOnlyWithoutDeadline(t *testing.T) {
	f := newFake()
	f.OnCreate = func(ctx context.Context, _ connector.CreatePaymentRequest) (connector.CreatePaymentResult, error) {
		<-ctx.Done()
		return connector.CreatePaymentResult{}, ctx.Err()
	}
	srv := grpctest.New(t, connectorrpc.NewServer(f))
	c := connectorrpc.NewClient("acme", srv.Conn, connectorrpc.ClientOptions{CallTimeout: 150 * time.Millisecond})
	if _, err := c.Describe(context.Background()); err != nil {
		t.Fatal(err)
	}
	err := create(context.Background(), c)
	if !errors.Is(err, connector.ErrUnknownOutcome) || !strings.Contains(err.Error(), "DeadlineExceeded") {
		t.Fatalf("CallTimeout phải áp khi ctx không có hạn: %v", err)
	}
}

func TestClient_PanicIsUnknown(t *testing.T) {
	f := newFake()
	entered := make(chan struct{})
	f.OnCreate = func(context.Context, connector.CreatePaymentRequest) (connector.CreatePaymentResult, error) {
		close(entered)
		panic("boom")
	}
	recoverer := func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, h grpc.UnaryHandler) (resp any, err error) {
		defer func() {
			if r := recover(); r != nil {
				err = status.Error(codes.Internal, "panic")
			}
		}()
		return h(ctx, req)
	}
	srv := grpctest.New(t, connectorrpc.NewServer(f), grpc.ChainUnaryInterceptor(recoverer))
	c := connectorrpc.NewClient("acme", srv.Conn, connectorrpc.ClientOptions{})
	if _, err := c.Describe(context.Background()); err != nil {
		t.Fatal(err)
	}
	err := create(context.Background(), c)
	<-entered
	if !errors.Is(err, connector.ErrUnknownOutcome) || errors.Is(err, connector.ErrProviderUnavailable) {
		t.Fatalf("panic phải là unknown: %v", err)
	}
}

// fakeDescribe thay Describe của server thật để thử các mô tả sai.
type fakeDescribe struct {
	connectorpb.ConnectorServiceServer
	resp *connectorpb.DescribeResponse
}

func (d fakeDescribe) Describe(context.Context, *connectorpb.DescribeRequest) (*connectorpb.DescribeResponse, error) {
	return d.resp, nil
}

func fullAcks() map[string]*connectorpb.WebhookAck {
	m := map[string]*connectorpb.WebhookAck{}
	for _, o := range connectorrpc.WebhookOutcomes {
		m[string(o)] = &connectorpb.WebhookAck{Status: 200, Body: []byte("OK"), ContentType: "text/plain"}
	}
	return m
}

func TestDescribe_Rejections(t *testing.T) {
	good := func() *connectorpb.DescribeResponse {
		return &connectorpb.DescribeResponse{ProtocolVersion: connectorrpc.ProtocolVersion, Name: "acme",
			Methods: []string{"qr"}, Operations: []string{"webhook", "refund"}, WebhookAcks: fullAcks()}
	}
	cases := []struct {
		name    string
		mut     func(*connectorpb.DescribeResponse)
		wantMsg []string
	}{
		{"name_mismatch", func(r *connectorpb.DescribeResponse) { r.Name = "other" }, []string{"acme", "other"}},
		{"protocol_mismatch", func(r *connectorpb.DescribeResponse) { r.ProtocolVersion = 2 }, []string{"1", "2"}},
		{"missing_ack", func(r *connectorpb.DescribeResponse) { delete(r.WebhookAcks, "ignored") }, []string{"ignored"}},
		{"nil_ack", func(r *connectorpb.DescribeResponse) { r.WebhookAcks["applied"] = nil }, []string{"applied"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := good()
			tc.mut(resp)
			srv := grpctest.New(t, fakeDescribe{connectorrpc.NewServer(newFake()), resp})
			c := connectorrpc.NewClient("acme", srv.Conn, connectorrpc.ClientOptions{})
			_, err := c.Describe(context.Background())
			if err == nil {
				t.Fatal("muốn lỗi")
			}
			for _, w := range tc.wantMsg {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("lỗi %q thiếu %q", err, w)
				}
			}
			if c.Described() {
				t.Error("Described phải false")
			}
			for _, op := range []connector.Operation{connector.OpQueryPayment, connector.OpWebhook, connector.OpRedirect,
				connector.OpCharge, connector.OpCancel, connector.OpRefund, connector.OpQueryRefund} {
				if c.Supports(op) {
					t.Errorf("Supports(%s) phải false", op)
				}
			}
			if len(c.Methods()) != 0 {
				t.Errorf("Methods phải rỗng: %v", c.Methods())
			}
		})
	}
}

func TestDescribe_NoWebhookNeedsNoAcks(t *testing.T) {
	resp := &connectorpb.DescribeResponse{ProtocolVersion: connectorrpc.ProtocolVersion, Name: "acme", Operations: []string{"refund"}}
	srv := grpctest.New(t, fakeDescribe{connectorrpc.NewServer(newFake()), resp})
	c := connectorrpc.NewClient("acme", srv.Conn, connectorrpc.ClientOptions{})
	if _, err := c.Describe(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !c.Supports(connector.OpRefund) || c.Supports(connector.OpWebhook) {
		t.Error("Supports sai")
	}
}

func TestDescribe_Result(t *testing.T) {
	c := grpctest.Loopback(t, newFake())
	if !c.Described() || c.Name() != "acme" {
		t.Fatal("chưa mô tả")
	}
	d, err := c.Describe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if d.Name != "acme" || len(d.Methods) != 2 || d.Methods[0] != paykit.MethodQR || !d.Operations[connector.OpWebhook] {
		t.Errorf("%+v", d)
	}
}

func TestAck_FromDescribeNoRPC(t *testing.T) {
	f := newFake()
	srv := grpctest.New(t, connectorrpc.NewServer(f))
	c := connectorrpc.NewClient("acme", srv.Conn, connectorrpc.ClientOptions{})
	if _, err := c.Describe(context.Background()); err != nil {
		t.Fatal(err)
	}
	srv.Server.Stop()
	st, body, ct := c.AckWebhook(connector.OutcomeApplied)
	if st != 200 || string(body) != "OK" || ct != "text/plain" {
		t.Errorf("ack: %d %q %q", st, body, ct)
	}
	if st, body, ct := c.AckWebhook("lạ"); st != 500 || body != nil || ct != "" {
		t.Errorf("outcome lạ: %d %q %q", st, body, ct)
	}
}

func TestClient_NotDescribedAckIs500(t *testing.T) {
	srv := grpctest.New(t, connectorrpc.NewServer(newFake()))
	c := connectorrpc.NewClient("acme", srv.Conn, connectorrpc.ClientOptions{})
	if st, _, _ := c.AckWebhook(connector.OutcomeApplied); st != 500 {
		t.Errorf("status %d", st)
	}
}

func TestCapabilities_NarrowedByReporter(t *testing.T) {
	f := newFake()
	f.Capabilities[connector.OpRefund] = false
	c := grpctest.Loopback(t, f)
	if _, ok := connector.AsRefunder(c); ok {
		t.Error("AsRefunder phải sai")
	}
	if _, ok := connector.AsCharger(c); !ok {
		t.Error("AsCharger phải đúng")
	}
	var _ connector.CapabilityReporter = c
}

func TestClient_OversizeWebhookNotSent(t *testing.T) {
	f := newFake()
	c := grpctest.Loopback(t, f)
	big := connector.RawWebhook{Body: make([]byte, 1<<20+1)}
	if _, err := c.ParseWebhook(context.Background(), big); !errors.Is(err, connector.ErrInvalidSignature) {
		t.Errorf("webhook: %v", err)
	}
	if _, err := c.ParseRedirect(context.Background(), big); !errors.Is(err, connector.ErrInvalidSignature) {
		t.Errorf("redirect: %v", err)
	}
	if f.Calls("parse_webhook") != 0 {
		t.Error("không được gửi")
	}
}

func TestClient_EmptyResponseIsUnknown(t *testing.T) {
	// Server trả OK nhưng thân thiếu status: không được hiểu là thất bại hay thành công.
	f := newFake()
	f.OnCreate = func(context.Context, connector.CreatePaymentRequest) (connector.CreatePaymentResult, error) {
		return connector.CreatePaymentResult{}, nil // Status rỗng
	}
	c := grpctest.Loopback(t, f)
	if err := create(context.Background(), c); !errors.Is(err, connector.ErrUnknownOutcome) {
		t.Fatalf("%v", err)
	}
}
