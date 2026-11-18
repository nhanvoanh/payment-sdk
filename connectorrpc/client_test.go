package connectorrpc_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"bitbucket.org/hasaki-tech/payment-connector-sdk/connector"
	"bitbucket.org/hasaki-tech/payment-connector-sdk/connectorpb"
	"bitbucket.org/hasaki-tech/payment-connector-sdk/connectorrpc"
	"bitbucket.org/hasaki-tech/payment-connector-sdk/connectorrpc/grpctest"
	"bitbucket.org/hasaki-tech/payment-connector-sdk/connectortest"
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

// switchDescribe trả mô tả có thể đổi khi đang chạy, như khi địa chỉ bị trỏ sang tiến trình khác.
type switchDescribe struct {
	connectorpb.ConnectorServiceServer
	name atomic.Value // string
}

func (d *switchDescribe) Describe(context.Context, *connectorpb.DescribeRequest) (*connectorpb.DescribeResponse, error) {
	return &connectorpb.DescribeResponse{ProtocolVersion: connectorrpc.ProtocolVersion, Name: d.name.Load().(string),
		Methods: []string{"qr"}, Operations: []string{"webhook", "redirect", "charge", "cancel", "refund", "query_payment", "query_refund"},
		WebhookAcks: fullAcks()}, nil
}

// businessCalls gọi mọi RPC nghiệp vụ một lần và trả lỗi của từng cái.
func businessCalls(c *connectorrpc.Client) map[string]error {
	ctx := context.Background()
	out := map[string]error{}
	_, out["create"] = c.CreatePayment(ctx, connector.CreatePaymentRequest{TransactionCode: "T"})
	_, out["charge"] = c.Charge(ctx, connector.ChargeRequest{TransactionCode: "T"})
	_, out["query"] = c.QueryPayment(ctx, connector.QueryPaymentRequest{TransactionCode: "T"})
	_, out["cancel"] = c.CancelPayment(ctx, connector.CancelRequest{TransactionCode: "T"})
	_, out["refund"] = c.CreateRefund(ctx, connector.RefundRequest{RefundCode: "R"})
	_, out["query_refund"] = c.QueryRefund(ctx, connector.QueryRefundRequest{RefundCode: "R"})
	_, out["parse_webhook"] = c.ParseWebhook(ctx, connector.RawWebhook{Body: []byte("{}")})
	_, out["parse_redirect"] = c.ParseRedirect(ctx, connector.RawWebhook{Body: []byte("{}")})
	return out
}

// moneyOps là các RPC tiền/truy vấn: khi chắc chắn chưa gửi phải là ErrProviderUnavailable; Parse* là ErrUnknownOutcome.
var moneyOps = map[string]bool{"create": true, "charge": true, "query": true, "cancel": true, "refund": true, "query_refund": true}

func assertNotSentErrors(t *testing.T, errs map[string]error) {
	t.Helper()
	for op, err := range errs {
		if moneyOps[op] {
			if !errors.Is(err, connector.ErrProviderUnavailable) || errors.Is(err, connector.ErrUnknownOutcome) {
				t.Errorf("%s: muốn ErrProviderUnavailable (chưa gửi), có %v", op, err)
			}
		} else if !errors.Is(err, connector.ErrUnknownOutcome) {
			t.Errorf("%s: muốn ErrUnknownOutcome (đối tác gửi lại), có %v", op, err)
		}
	}
}

// countingServer đếm RPC trừ Describe tới được server.
func countingServer(n *atomic.Int32) grpc.ServerOption {
	return grpc.ChainUnaryInterceptor(func(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
		if !strings.HasSuffix(info.FullMethod, "/Describe") {
			n.Add(1)
		}
		return h(ctx, req)
	})
}

func TestQuarantine_KeepsDescriptionAndBlocksCalls(t *testing.T) {
	sd := &switchDescribe{ConnectorServiceServer: connectorrpc.NewServer(newFake())}
	sd.name.Store("acme")
	var rpcs atomic.Int32
	srv := grpctest.New(t, sd, countingServer(&rpcs))
	c := connectorrpc.NewClient("acme", srv.Conn, connectorrpc.ClientOptions{})
	if _, err := c.Describe(context.Background()); err != nil {
		t.Fatal(err)
	}

	sd.name.Store("other")
	if _, err := c.Describe(context.Background()); !errors.Is(err, connectorrpc.ErrIdentityMismatch) {
		t.Fatalf("muốn ErrIdentityMismatch, có %v", err)
	}
	if !c.Quarantined() {
		t.Fatal("phải bị cách ly")
	}
	// Mô tả hợp lệ cuối được giữ: use case không được hiểu nhầm "đối tác không có năng lực".
	if !c.Described() || len(c.Methods()) != 1 || !c.Supports(connector.OpRefund) || !c.Supports(connector.OpQueryPayment) {
		t.Fatalf("cách ly phải giữ mô tả: described=%v methods=%v", c.Described(), c.Methods())
	}
	if st, _, _ := c.AckWebhook(connector.OutcomeApplied); st != 200 {
		t.Errorf("ack giữ theo mô tả cũ, có %d", st)
	}
	assertNotSentErrors(t, businessCalls(c))
	if n := rpcs.Load(); n != 0 {
		t.Fatalf("cách ly mà vẫn có %d RPC tới server", n)
	}

	sd.name.Store("acme")
	if _, err := c.Describe(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c.Quarantined() {
		t.Fatal("Describe đúng lại phải gỡ cách ly")
	}
	if err := create(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if rpcs.Load() != 1 {
		t.Fatalf("sau khi gỡ cách ly lệnh tạo phải tới server đúng một lần, có %d", rpcs.Load())
	}
}

func TestQuarantine_ProtocolMismatchBeforeAnyDescription(t *testing.T) {
	resp := &connectorpb.DescribeResponse{ProtocolVersion: connectorrpc.ProtocolVersion + 1, Name: "acme", Methods: []string{"qr"}}
	f := newFake()
	srv := grpctest.New(t, fakeDescribe{connectorrpc.NewServer(f), resp})
	c := connectorrpc.NewClient("acme", srv.Conn, connectorrpc.ClientOptions{})
	if _, err := c.Describe(context.Background()); !errors.Is(err, connectorrpc.ErrIdentityMismatch) {
		t.Fatalf("muốn ErrIdentityMismatch, có %v", err)
	}
	if !c.Quarantined() || c.Described() || len(c.Methods()) != 0 {
		t.Fatal("chưa từng có mô tả hợp lệ: cách ly và mô tả rỗng")
	}
	if err := create(context.Background(), c); !errors.Is(err, connector.ErrProviderUnavailable) {
		t.Fatalf("muốn ErrProviderUnavailable, có %v", err)
	}
	if f.Calls("create") != 0 {
		t.Fatal("cách ly mà lệnh tạo vẫn tới connector")
	}
}

// Đua: lời gọi đã qua lần kiểm cờ đầu và đang chờ kết nối thì bị cách ly; lần đọc lại ngay trước khi gửi chặn nó.
func TestQuarantine_SetWhileWaitingReadyBlocksSend(t *testing.T) {
	var rpcs atomic.Int32
	srv := grpctest.New(t, connectorrpc.NewServer(newFake()), countingServer(&rpcs))
	c := connectorrpc.NewClient("acme", srv.Conn, connectorrpc.ClientOptions{})
	if _, err := c.Describe(context.Background()); err != nil {
		t.Fatal(err)
	}
	connectorrpc.SetAfterReady(c, func() { connectorrpc.Quarantine(c) })
	if err := create(context.Background(), c); !errors.Is(err, connector.ErrProviderUnavailable) || errors.Is(err, connector.ErrUnknownOutcome) {
		t.Fatalf("muốn ErrProviderUnavailable, có %v", err)
	}
	if rpcs.Load() != 0 {
		t.Fatal("bị cách ly trước khi gửi mà vẫn gửi")
	}
}

// Lời gọi đang bay lúc phát hiện sai danh tính: tiến trình trả lời "thành công" nhưng không được tin.
func TestQuarantine_InFlightResponseIsUnknown(t *testing.T) {
	f := newFake()
	entered, release := make(chan struct{}), make(chan struct{})
	f.OnCreate = func(context.Context, connector.CreatePaymentRequest) (connector.CreatePaymentResult, error) {
		close(entered)
		<-release
		return connector.CreatePaymentResult{ProviderRef: "r", Status: connector.StatusPending,
			NextAction: paykit.NextAction{Kind: paykit.NextActionRedirect, URL: "https://pay.example/T"}}, nil
	}
	sd := &switchDescribe{ConnectorServiceServer: connectorrpc.NewServer(f)}
	sd.name.Store("acme")
	srv := grpctest.New(t, sd)
	c := connectorrpc.NewClient("acme", srv.Conn, connectorrpc.ClientOptions{})
	if _, err := c.Describe(context.Background()); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- create(context.Background(), c) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("lệnh tạo không tới server")
	}
	sd.name.Store("other")
	if _, err := c.Describe(context.Background()); !errors.Is(err, connectorrpc.ErrIdentityMismatch) {
		t.Fatalf("muốn ErrIdentityMismatch, có %v", err)
	}
	// Gỡ cách ly trước khi phản hồi về: cờ đã sạch nhưng thế hệ đã đổi, phản hồi vẫn không được tin.
	sd.name.Store("acme")
	if _, err := c.Describe(context.Background()); err != nil || c.Quarantined() {
		t.Fatalf("Describe đúng phải gỡ cách ly: %v", err)
	}
	close(release)
	select {
	case err := <-done:
		if !errors.Is(err, connector.ErrUnknownOutcome) {
			t.Fatalf("phản hồi về sau khi cách ly phải là chưa rõ, có %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("lệnh tạo không trả về")
	}
}

// Server biết tên mình: từ chối trước handler, proxy tự cách ly mà không cần Describe.
func TestIdentity_ServerRejectsBeforeHandler(t *testing.T) {
	f := connectortest.New("other", paykit.MethodQR)
	srv := grpctest.New(t, connectorrpc.NewServer(f), grpc.UnaryInterceptor(connectorrpc.IdentityInterceptor("other")))

	c := connectorrpc.NewClient("acme", srv.Conn, connectorrpc.ClientOptions{})
	err := create(context.Background(), c)
	if !errors.Is(err, connector.ErrProviderUnavailable) || errors.Is(err, connector.ErrUnknownOutcome) || !errors.Is(err, connectorrpc.ErrIdentityMismatch) {
		t.Fatalf("muốn ErrProviderUnavailable + ErrIdentityMismatch, có %v", err)
	}
	if !c.Quarantined() {
		t.Fatal("proxy phải tự cách ly khi server báo sai tên")
	}
	if f.Calls("create") != 0 {
		t.Fatal("handler không được chạy")
	}

	p := connectorrpc.NewClient("acme", srv.Conn, connectorrpc.ClientOptions{})
	if _, err := p.ParseWebhook(context.Background(), connector.RawWebhook{Body: []byte("{}")}); !errors.Is(err, connector.ErrUnknownOutcome) {
		t.Fatalf("Parse* bị từ chối vì sai tên: muốn ErrUnknownOutcome, có %v", err)
	}
	d := connectorrpc.NewClient("acme", srv.Conn, connectorrpc.ClientOptions{})
	if _, err := d.Describe(context.Background()); !errors.Is(err, connectorrpc.ErrIdentityMismatch) {
		t.Fatalf("Describe bị server từ chối: muốn ErrIdentityMismatch, có %v", err)
	}
	if len(f.CallLog()) != 0 {
		t.Fatalf("không handler nào được chạy: %v", f.CallLog())
	}

	// Đúng tên thì qua; client không gửi metadata (bản cũ) cũng qua.
	ok := connectorrpc.NewClient("other", srv.Conn, connectorrpc.ClientOptions{})
	if err := create(context.Background(), ok); err != nil {
		t.Fatal(err)
	}
	if _, err := connectorpb.NewConnectorServiceClient(srv.Conn).CreatePayment(context.Background(), &connectorpb.CreatePaymentRequest{}); err != nil {
		t.Fatalf("client không gửi metadata phải gọi được: %v", err)
	}
	if f.Calls("create") != 2 {
		t.Fatalf("muốn 2 lệnh tạo, có %d", f.Calls("create"))
	}
}

// FailedPrecondition không có dấu của server SDK vẫn là chưa rõ, và không cách ly.
func TestIdentity_FailedPreconditionWithoutMarkIsUnknown(t *testing.T) {
	srv := grpctest.New(t, connectorrpc.NewServer(newFake()), grpc.UnaryInterceptor(
		func(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
			if strings.HasSuffix(info.FullMethod, "/CreatePayment") {
				_ = grpc.SetTrailer(ctx, metadata.Pairs(connectorrpc.MetadataIdentity, "something-else"))
				return nil, status.Error(codes.FailedPrecondition, "lý do khác")
			}
			return h(ctx, req)
		}))
	c := connectorrpc.NewClient("acme", srv.Conn, connectorrpc.ClientOptions{})
	if err := create(context.Background(), c); !errors.Is(err, connector.ErrUnknownOutcome) {
		t.Fatalf("muốn ErrUnknownOutcome, có %v", err)
	}
	if c.Quarantined() {
		t.Fatal("không có dấu thì không cách ly")
	}
}

// Lỗi vận chuyển ở Describe (không có phản hồi) không cách ly: giữ mô tả cũ.
func TestDescribe_TransportErrorKeepsDescription(t *testing.T) {
	srv := grpctest.New(t, connectorrpc.NewServer(newFake()))
	c := connectorrpc.NewClient("acme", srv.Conn, connectorrpc.ClientOptions{ReadyWait: testReadyWait})
	if _, err := c.Describe(context.Background()); err != nil {
		t.Fatal(err)
	}
	srv.Kill(t)
	_, err := c.Describe(context.Background())
	if err == nil || errors.Is(err, connectorrpc.ErrIdentityMismatch) {
		t.Fatalf("muốn lỗi vận chuyển, có %v", err)
	}
	if !c.Described() || !c.Supports(connector.OpRefund) || len(c.Methods()) == 0 {
		t.Fatal("lỗi vận chuyển phải giữ mô tả cũ")
	}
}
