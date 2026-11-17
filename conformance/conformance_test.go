package conformance_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"bitbucket.org/hasaki-tech/payment-connector-sdk/conformance"
	"bitbucket.org/hasaki-tech/payment-connector-sdk/connector"
	"bitbucket.org/hasaki-tech/payment-connector-sdk/connectortest"
	"bitbucket.org/hasaki-tech/payment-connector-sdk/paykit"
)

// fakePartner là đối tác giả trong tiến trình; connector tốt dựng trên connectortest.Fake hỏi nó mỗi lần "gọi mạng".
type fakePartner struct {
	mu      sync.Mutex
	mode    conformance.Mode
	pays    map[string]paid
	release chan struct{} // đóng khi test xong, để goroutine của connector treo được giải phóng
}

type paid struct {
	status connector.Status
	money  paykit.Money
}

var secret = []byte("acme-secret")

const sigHeader = "X-Acme-Signature"

func (p *fakePartner) SetMode(m conformance.Mode) {
	p.mu.Lock()
	p.mode = m
	p.mu.Unlock()
}

func (p *fakePartner) SetPaymentStatus(tx string, st connector.Status, m paykit.Money) {
	p.mu.Lock()
	p.pays[tx] = paid{st, m}
	p.mu.Unlock()
}

type hookBody struct {
	Tx, Ref, Status, Currency string
	Amount                    int64
}

func sign(body []byte) string {
	m := hmac.New(sha256.New, secret)
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

func (p *fakePartner) SignedWebhook(tx, ref string, st connector.Status, m paykit.Money) connector.RawWebhook {
	body, _ := json.Marshal(hookBody{Tx: tx, Ref: ref, Status: string(st), Currency: m.Currency, Amount: m.Amount})
	return connector.RawWebhook{Headers: map[string][]string{sigHeader: {sign(body)}}, Body: body, RemoteIP: "203.0.113.7"}
}

func (p *fakePartner) currentMode() conformance.Mode {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.mode
}

func (p *fakePartner) lookup(tx string) (paid, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	v, ok := p.pays[tx]
	return v, ok
}

// call mô phỏng một lần gọi mạng của connector viết đúng.
func (p *fakePartner) call(ctx context.Context) error {
	switch p.currentMode() {
	case conformance.ModeDown:
		return fmt.Errorf("%w: không mở được kết nối", connector.ErrProviderUnavailable)
	case conformance.ModeHang:
		p.hang(ctx)
		return fmt.Errorf("%w: hết hạn chờ: %v", connector.ErrUnknownOutcome, ctx.Err())
	case conformance.ModeReject:
		return &connector.RejectedError{Code: "R01", Message: "từ chối"}
	case conformance.ModeGarbage:
		return fmt.Errorf("%w: phản hồi đọc không được", connector.ErrUnknownOutcome)
	}
	if ctx.Err() != nil {
		return fmt.Errorf("%w: %v", connector.ErrUnknownOutcome, ctx.Err())
	}
	return nil
}

func (p *fakePartner) hang(ctx context.Context) {
	select {
	case <-ctx.Done():
	case <-p.release:
	}
}

// newGood dựng connector viết đúng trên Fake cùng đối tác giả của nó.
func newGood(tb testing.TB) (*connectortest.Fake, *fakePartner) {
	p := &fakePartner{pays: map[string]paid{}, release: make(chan struct{})}
	tb.Cleanup(func() { close(p.release) })
	f := connectortest.New("acme", paykit.MethodVisa, paykit.MethodQR)
	f.OnCreate = func(ctx context.Context, req connector.CreatePaymentRequest) (connector.CreatePaymentResult, error) {
		if err := p.call(ctx); err != nil {
			return connector.CreatePaymentResult{}, err
		}
		return connector.CreatePaymentResult{
			ProviderRef: "acme_" + req.TransactionCode,
			Status:      connector.StatusPending,
			NextAction:  paykit.NextAction{Kind: paykit.NextActionRedirect, URL: "https://pay.acme.example/" + req.TransactionCode},
		}, nil
	}
	f.OnCharge = func(ctx context.Context, req connector.ChargeRequest) (connector.ChargeResult, error) {
		if err := p.call(ctx); err != nil {
			return connector.ChargeResult{}, err
		}
		return connector.ChargeResult{ProviderRef: "acme_" + req.TransactionCode, Status: connector.StatusSucceeded, AmountPaid: req.Money}, nil
	}
	f.OnQuery = func(ctx context.Context, req connector.QueryPaymentRequest) (connector.PaymentStatus, error) {
		if err := p.call(ctx); err != nil {
			return connector.PaymentStatus{}, err
		}
		v, ok := p.lookup(req.TransactionCode)
		if !ok {
			return connector.PaymentStatus{Found: false}, nil
		}
		return connector.PaymentStatus{Found: true, Status: v.status, AmountPaid: v.money, ProviderRef: req.ProviderRef}, nil
	}
	f.OnCancel = func(ctx context.Context, req connector.CancelRequest) (connector.CancelResult, error) {
		if err := p.call(ctx); err != nil {
			return connector.CancelResult{}, err
		}
		if v, ok := p.lookup(req.TransactionCode); ok && v.status == connector.StatusSucceeded {
			return connector.CancelResult{Status: connector.StatusSucceeded}, nil
		}
		return connector.CancelResult{Status: connector.StatusCanceled}, nil
	}
	f.OnRefund = func(ctx context.Context, req connector.RefundRequest) (connector.RefundResult, error) {
		if err := p.call(ctx); err != nil {
			return connector.RefundResult{}, err
		}
		return connector.RefundResult{ProviderRef: "acme_" + req.RefundCode, Status: connector.StatusPending}, nil
	}
	f.OnQueryRefund = func(ctx context.Context, _ connector.QueryRefundRequest) (connector.RefundStatus, error) {
		if err := p.call(ctx); err != nil {
			return connector.RefundStatus{}, err
		}
		return connector.RefundStatus{Found: false}, nil
	}
	f.OnParseWebhook = func(_ context.Context, raw connector.RawWebhook) (connector.ProviderEvent, error) {
		var got string
		for k, vs := range raw.Headers {
			if strings.EqualFold(k, sigHeader) && len(vs) > 0 {
				got = vs[0]
			}
		}
		if !hmac.Equal([]byte(got), []byte(sign(raw.Body))) {
			return connector.ProviderEvent{}, connector.ErrInvalidSignature
		}
		var b hookBody
		if err := json.Unmarshal(raw.Body, &b); err != nil {
			return connector.ProviderEvent{}, err
		}
		return connector.ProviderEvent{
			EventID: "evt_" + b.Tx + "_" + b.Status, Kind: connector.EventKindPayment,
			TransactionCode: b.Tx, ProviderRef: b.Ref, Status: connector.Status(b.Status),
			AmountPaid: paykit.Money{Amount: b.Amount, Currency: b.Currency}, Raw: raw.Body,
		}, nil
	}
	return f, p
}

var amount = paykit.Money{Amount: 150000, Currency: "VND"}

// harness dựng Harness mà mỗi cặp mới đi qua wrap; wrap nil là connector tốt nguyên bản.
func harness(wrap func(f *connectortest.Fake, p *fakePartner) connector.Connector) conformance.Harness {
	return conformance.Harness{
		New: func(tb testing.TB) (connector.Connector, conformance.Partner) {
			f, p := newGood(tb)
			if wrap == nil {
				return f, p
			}
			return wrap(f, p), p
		},
		Method:             paykit.MethodVisa,
		Amount:             amount,
		CreateCallsPartner: true,
	}
}

func rules(r conformance.Report) []string {
	var out []string
	for _, f := range r.Findings {
		if !slices.Contains(out, f.Rule) {
			out = append(out, f.Rule)
		}
	}
	return out
}

func dump(r conformance.Report) string {
	var b strings.Builder
	for _, f := range r.Findings {
		fmt.Fprintf(&b, "\n  %s: %s", f.Rule, f.Detail)
	}
	return b.String()
}

// expectOnly đòi Check bắn đúng luật want (bản thường), chỉ cho phép thêm bản /grpc của chính luật đó.
func expectOnly(t *testing.T, r conformance.Report, want string) {
	t.Helper()
	got := rules(r)
	if !slices.Contains(got, want) {
		t.Fatalf("Check không bắt được %s; findings:%s", want, dump(r))
	}
	for _, g := range got {
		if g != want && g != want+"/grpc" {
			t.Errorf("luật %s bắn nhầm (chỉ chờ %s):%s", g, want, dump(r))
		}
	}
	for _, f := range r.Findings {
		if f.Detail == "" {
			t.Errorf("finding %s không có Detail", f.Rule)
		}
	}
}

// expectBoth như expectOnly nhưng đòi cả bản /grpc (khi vi phạm đi qua dây một cách tất định).
func expectBoth(t *testing.T, r conformance.Report, want string) {
	t.Helper()
	expectOnly(t, r, want)
	if !slices.Contains(rules(r), want+"/grpc") {
		t.Errorf("Check không bắt được %s/grpc; findings:%s", want, dump(r))
	}
}

func TestCheck_GoodFakePassesEverything(t *testing.T) {
	t.Parallel()
	r := conformance.Check(t, harness(nil))
	if len(r.Findings) != 0 {
		t.Fatalf("connector tốt không được có finding:%s", dump(r))
	}
	if len(r.Skipped) != 0 {
		t.Fatalf("connector khai đủ thao tác không được bỏ luật nào: %v", r.Skipped)
	}
}

func TestCheck_Catches_TimeoutReportedAsUnavailable(t *testing.T) {
	t.Parallel()
	r := conformance.Check(t, harness(func(f *connectortest.Fake, p *fakePartner) connector.Connector {
		f.OnCreate = func(ctx context.Context, req connector.CreatePaymentRequest) (connector.CreatePaymentResult, error) {
			if p.currentMode() == conformance.ModeHang {
				p.hang(ctx)
				return connector.CreatePaymentResult{}, connector.ErrProviderUnavailable
			}
			if err := p.call(ctx); err != nil {
				return connector.CreatePaymentResult{}, err
			}
			return connector.CreatePaymentResult{ProviderRef: "acme_" + req.TransactionCode, Status: connector.StatusPending}, nil
		}
		return f
	}))
	expectOnly(t, r, "PAY-4")
}

func TestCheck_Catches_GarbageReportedAsRejected(t *testing.T) {
	t.Parallel()
	r := conformance.Check(t, harness(func(f *connectortest.Fake, p *fakePartner) connector.Connector {
		good := f.OnCreate
		f.OnCreate = func(ctx context.Context, req connector.CreatePaymentRequest) (connector.CreatePaymentResult, error) {
			if p.currentMode() == conformance.ModeGarbage {
				return connector.CreatePaymentResult{}, &connector.RejectedError{Code: "PARSE", Message: "không đọc được"}
			}
			return good(ctx, req)
		}
		return f
	}))
	expectBoth(t, r, "PAY-6")
}

func TestCheck_Catches_DownReportedAsNil(t *testing.T) {
	t.Parallel()
	r := conformance.Check(t, harness(func(f *connectortest.Fake, p *fakePartner) connector.Connector {
		good := f.OnCreate
		f.OnCreate = func(ctx context.Context, req connector.CreatePaymentRequest) (connector.CreatePaymentResult, error) {
			if p.currentMode() == conformance.ModeDown {
				return connector.CreatePaymentResult{Status: connector.StatusPending}, nil
			}
			return good(ctx, req)
		}
		return f
	}))
	expectBoth(t, r, "PAY-3")
}

func TestCheck_Catches_LongProviderRef(t *testing.T) {
	t.Parallel()
	r := conformance.Check(t, harness(func(f *connectortest.Fake, _ *fakePartner) connector.Connector {
		good := f.OnCreate
		f.OnCreate = func(ctx context.Context, req connector.CreatePaymentRequest) (connector.CreatePaymentResult, error) {
			res, err := good(ctx, req)
			res.ProviderRef = strings.Repeat("r", 300)
			return res, err
		}
		return f
	}))
	expectBoth(t, r, "PAY-2")
}

func TestCheck_Catches_UnstableEventID(t *testing.T) {
	t.Parallel()
	r := conformance.Check(t, harness(func(f *connectortest.Fake, _ *fakePartner) connector.Connector {
		good := f.OnParseWebhook
		var n atomic.Int64
		f.OnParseWebhook = func(ctx context.Context, raw connector.RawWebhook) (connector.ProviderEvent, error) {
			ev, err := good(ctx, raw)
			ev.EventID = fmt.Sprintf("%s_%d", ev.EventID, n.Add(1)) // gắn số đếm: mỗi lần gửi lại thành sự kiện mới
			return ev, err
		}
		return f
	}))
	expectBoth(t, r, "WHK-2")
}

func TestCheck_Catches_SameEventIDAcrossTransactions(t *testing.T) {
	t.Parallel()
	r := conformance.Check(t, harness(func(f *connectortest.Fake, _ *fakePartner) connector.Connector {
		good := f.OnParseWebhook
		f.OnParseWebhook = func(ctx context.Context, raw connector.RawWebhook) (connector.ProviderEvent, error) {
			ev, err := good(ctx, raw)
			ev.EventID = "payment_notify"
			return ev, err
		}
		return f
	}))
	expectBoth(t, r, "WHK-3")
}

func TestCheck_Catches_TamperedWebhookAccepted(t *testing.T) {
	t.Parallel()
	r := conformance.Check(t, harness(func(f *connectortest.Fake, _ *fakePartner) connector.Connector {
		f.OnParseWebhook = func(_ context.Context, raw connector.RawWebhook) (connector.ProviderEvent, error) {
			var b hookBody // không kiểm chữ ký
			if err := json.Unmarshal(raw.Body, &b); err != nil {
				return connector.ProviderEvent{}, err
			}
			return connector.ProviderEvent{EventID: "evt_" + b.Tx + "_" + b.Status, Kind: connector.EventKindPayment,
				TransactionCode: b.Tx, Status: connector.Status(b.Status)}, nil
		}
		return f
	}))
	expectBoth(t, r, "WHK-4")
}

func TestCheck_Catches_PanicOnEmptyWebhook(t *testing.T) {
	t.Parallel()
	r := conformance.Check(t, harness(func(f *connectortest.Fake, _ *fakePartner) connector.Connector {
		good := f.OnParseWebhook
		f.OnParseWebhook = func(ctx context.Context, raw connector.RawWebhook) (connector.ProviderEvent, error) {
			if len(raw.Body) == 0 {
				panic("index out of range")
			}
			return good(ctx, raw)
		}
		return f
	}))
	expectBoth(t, r, "WHK-5")
}

// impureAck trả status khác nhau mỗi lần gọi.
type impureAck struct {
	*connectortest.Fake
	n atomic.Int64
}

func (a *impureAck) AckWebhook(connector.WebhookOutcome) (int, []byte, string) {
	return 200 + int(a.n.Add(1)%2), []byte("OK"), "text/plain"
}

func TestCheck_Catches_ImpureAck(t *testing.T) {
	t.Parallel()
	r := conformance.Check(t, harness(func(f *connectortest.Fake, _ *fakePartner) connector.Connector {
		return &impureAck{Fake: f}
	}))
	expectOnly(t, r, "WHK-6")
}

func TestCheck_Catches_CancelClaimsCanceledWhenPaid(t *testing.T) {
	t.Parallel()
	r := conformance.Check(t, harness(func(f *connectortest.Fake, _ *fakePartner) connector.Connector {
		f.OnCancel = func(context.Context, connector.CancelRequest) (connector.CancelResult, error) {
			return connector.CancelResult{Status: connector.StatusCanceled}, nil
		}
		return f
	}))
	expectBoth(t, r, "CXL-1")
}

func TestCheck_Catches_BadName(t *testing.T) {
	t.Parallel()
	r := conformance.Check(t, harness(func(f *connectortest.Fake, _ *fakePartner) connector.Connector {
		f.NameValue = "Acme-Pay"
		return f
	}))
	expectBoth(t, r, "ID-1")
}

// unstableName đổi tên giữa các lần gọi: Describe qua gRPC phải hỏng mà không làm sập test.
type unstableName struct {
	*connectortest.Fake
	n atomic.Int64
}

func (u *unstableName) Name() string { return fmt.Sprintf("acme%d", u.n.Add(1)) }

func TestCheck_Catches_UnstableName(t *testing.T) {
	t.Parallel()
	r := conformance.Check(t, harness(func(f *connectortest.Fake, _ *fakePartner) connector.Connector {
		return &unstableName{Fake: f}
	}))
	expectBoth(t, r, "ID-1")
	for _, s := range r.Skipped {
		if !strings.Contains(s, "/grpc") {
			t.Errorf("chỉ bản /grpc được bỏ khi Describe hỏng: %s", s)
		}
	}
}

func TestCheck_Catches_DuplicateMethods(t *testing.T) {
	t.Parallel()
	r := conformance.Check(t, harness(func(f *connectortest.Fake, _ *fakePartner) connector.Connector {
		f.MethodsValue = []paykit.Method{paykit.MethodVisa, paykit.MethodVisa}
		return f
	}))
	expectBoth(t, r, "ID-2")
}

// refundClaimer khai OpRefund mà không cài Refunder.
type refundClaimer struct{ f *connectortest.Fake }

func (c refundClaimer) Name() string             { return c.f.Name() }
func (c refundClaimer) Methods() []paykit.Method { return c.f.Methods() }
func (c refundClaimer) CreatePayment(ctx context.Context, req connector.CreatePaymentRequest) (connector.CreatePaymentResult, error) {
	return c.f.CreatePayment(ctx, req)
}
func (c refundClaimer) Supports(op connector.Operation) bool { return op == connector.OpRefund }

func TestCheck_Catches_SupportsWithoutInterface(t *testing.T) {
	t.Parallel()
	r := conformance.Check(t, harness(func(f *connectortest.Fake, _ *fakePartner) connector.Connector {
		return refundClaimer{f}
	}))
	expectBoth(t, r, "ID-3")
}

func TestCheck_Catches_IgnoresContext(t *testing.T) {
	t.Parallel()
	r := conformance.Check(t, harness(func(f *connectortest.Fake, _ *fakePartner) connector.Connector {
		good := f.OnCreate
		f.OnCreate = func(ctx context.Context, req connector.CreatePaymentRequest) (connector.CreatePaymentResult, error) {
			// Chỉ lấy hạn chót của ctx, bỏ qua tín hiệu huỷ: lỗi hay gặp khi tự dựng http.Client theo hạn.
			inner, cancel := context.WithCancel(context.Background())
			defer cancel()
			if dl, ok := ctx.Deadline(); ok {
				inner, cancel = context.WithDeadline(context.Background(), dl)
				defer cancel()
			}
			return good(inner, req)
		}
		return f
	}))
	expectOnly(t, r, "CTX-1")
}

func TestCheck_Catches_HangIgnoringDeadline(t *testing.T) {
	t.Parallel()
	r := conformance.Check(t, harness(func(f *connectortest.Fake, p *fakePartner) connector.Connector {
		good := f.OnQuery
		f.OnQuery = func(ctx context.Context, req connector.QueryPaymentRequest) (connector.PaymentStatus, error) {
			if p.currentMode() == conformance.ModeHang {
				p.hang(context.Background()) // treo tới khi test xong
			}
			return good(ctx, req)
		}
		return f
	}))
	got := rules(r)
	if !slices.Contains(got, "QRY-3") {
		t.Fatalf("Check không bắt được QRY-3:%s", dump(r))
	}
	for _, g := range got {
		if g != "QRY-3" && g != "CTX-1" { // treo bất kể ctx thì cũng vi phạm CTX-1
			t.Errorf("luật %s bắn nhầm:%s", g, dump(r))
		}
	}
}

func TestCheck_Catches_RefundTimeoutAsRejected(t *testing.T) {
	t.Parallel()
	r := conformance.Check(t, harness(func(f *connectortest.Fake, p *fakePartner) connector.Connector {
		good := f.OnRefund
		f.OnRefund = func(ctx context.Context, req connector.RefundRequest) (connector.RefundResult, error) {
			if p.currentMode() == conformance.ModeHang {
				p.hang(ctx)
				return connector.RefundResult{}, &connector.RejectedError{Code: "TIMEOUT"}
			}
			return good(ctx, req)
		}
		return f
	}))
	expectOnly(t, r, "RFD-1")
}

func TestCheck_Catches_QueryUnknownAsError(t *testing.T) {
	t.Parallel()
	r := conformance.Check(t, harness(func(f *connectortest.Fake, p *fakePartner) connector.Connector {
		good := f.OnQuery
		f.OnQuery = func(ctx context.Context, req connector.QueryPaymentRequest) (connector.PaymentStatus, error) {
			if _, ok := p.lookup(req.TransactionCode); !ok {
				return connector.PaymentStatus{}, errors.New("acme: transaction not found")
			}
			return good(ctx, req)
		}
		return f
	}))
	expectBoth(t, r, "QRY-1")
}

func TestCheck_PanicEverywhereDoesNotCrash(t *testing.T) {
	t.Parallel()
	r := conformance.Check(t, harness(func(f *connectortest.Fake, _ *fakePartner) connector.Connector {
		f.OnCreate = func(context.Context, connector.CreatePaymentRequest) (connector.CreatePaymentResult, error) {
			panic("boom")
		}
		return f
	}))
	got := rules(r)
	for _, want := range []string{"PAY-1", "PAY-1/grpc", "PAY-7", "CTX-1"} {
		if !slices.Contains(got, want) {
			t.Errorf("panic trong Create phải thành finding %s; findings:%s", want, dump(r))
		}
	}
}

func TestCheck_SkipsUndeclaredOperations(t *testing.T) {
	t.Parallel()
	h := harness(func(f *connectortest.Fake, _ *fakePartner) connector.Connector {
		for op := range f.Capabilities {
			f.Capabilities[op] = false
		}
		return f
	})
	h.CreateCallsPartner = false
	r := conformance.Check(t, h)
	if len(r.Findings) != 0 {
		t.Fatalf("thao tác không khai không được sinh finding:%s", dump(r))
	}
	want := []string{"PAY-3", "PAY-4", "PAY-5", "PAY-6", "QRY-1", "QRY-2", "QRY-3", "CXL-1",
		"WHK-1", "WHK-2", "WHK-3", "WHK-4", "WHK-5", "WHK-6", "RFD-1", "RFD-2"}
	for _, code := range want {
		for _, c := range []string{code, code + "/grpc"} {
			if !slices.ContainsFunc(r.Skipped, func(s string) bool { return strings.HasPrefix(s, c+": ") && len(s) > len(c)+2 }) {
				t.Errorf("thiếu %q trong Skipped (kèm lý do): %v", c, r.Skipped)
			}
		}
	}
	for _, s := range r.Skipped {
		for _, never := range []string{"ID-1:", "ID-2:", "ID-3:", "PAY-1:", "PAY-2:", "PAY-7:", "CTX-1:"} {
			if strings.HasPrefix(s, never) {
				t.Errorf("luật %s luôn áp dụng, không được bỏ: %s", never, s)
			}
		}
	}
}

func TestRun_GoodFake(t *testing.T) {
	conformance.Run(t, harness(nil))
}

func TestCheck_Catches_RelativeRedirectURL(t *testing.T) {
	t.Parallel()
	r := conformance.Check(t, harness(func(f *connectortest.Fake, _ *fakePartner) connector.Connector {
		good := f.OnCreate
		f.OnCreate = func(ctx context.Context, req connector.CreatePaymentRequest) (connector.CreatePaymentResult, error) {
			res, err := good(ctx, req)
			res.NextAction.URL = "/pay/" + req.TransactionCode
			return res, err
		}
		return f
	}))
	expectBoth(t, r, "PAY-1")
}

func TestCheck_Catches_RejectReportedAsPlainError(t *testing.T) {
	t.Parallel()
	r := conformance.Check(t, harness(func(f *connectortest.Fake, p *fakePartner) connector.Connector {
		good := f.OnCreate
		f.OnCreate = func(ctx context.Context, req connector.CreatePaymentRequest) (connector.CreatePaymentResult, error) {
			if p.currentMode() == conformance.ModeReject {
				return connector.CreatePaymentResult{}, errors.New("acme: card declined")
			}
			return good(ctx, req)
		}
		return f
	}))
	expectBoth(t, r, "PAY-5")
}

func TestCheck_Catches_PanicOnRepeatedCreate(t *testing.T) {
	t.Parallel()
	r := conformance.Check(t, harness(func(f *connectortest.Fake, _ *fakePartner) connector.Connector {
		good := f.OnCreate
		var mu sync.Mutex
		seen := map[string]bool{}
		f.OnCreate = func(ctx context.Context, req connector.CreatePaymentRequest) (connector.CreatePaymentResult, error) {
			mu.Lock()
			dup := seen[req.IdempotencyKey]
			seen[req.IdempotencyKey] = true
			mu.Unlock()
			if dup {
				panic("duplicate key")
			}
			return good(ctx, req)
		}
		return f
	}))
	expectBoth(t, r, "PAY-7")
}

func TestCheck_Catches_QueryWrongAmount(t *testing.T) {
	t.Parallel()
	r := conformance.Check(t, harness(func(f *connectortest.Fake, _ *fakePartner) connector.Connector {
		good := f.OnQuery
		f.OnQuery = func(ctx context.Context, req connector.QueryPaymentRequest) (connector.PaymentStatus, error) {
			st, err := good(ctx, req)
			st.AmountPaid.Amount *= 100 // nhầm đơn vị tiền
			return st, err
		}
		return f
	}))
	expectBoth(t, r, "QRY-2")
}

func TestCheck_Catches_WebhookWrongTransaction(t *testing.T) {
	t.Parallel()
	r := conformance.Check(t, harness(func(f *connectortest.Fake, _ *fakePartner) connector.Connector {
		good := f.OnParseWebhook
		f.OnParseWebhook = func(ctx context.Context, raw connector.RawWebhook) (connector.ProviderEvent, error) {
			ev, err := good(ctx, raw)
			ev.TransactionCode = strings.ToLower(ev.TransactionCode)
			return ev, err
		}
		return f
	}))
	expectBoth(t, r, "WHK-1")
}

func TestCheck_Catches_QueryRefundUnknownAsError(t *testing.T) {
	t.Parallel()
	r := conformance.Check(t, harness(func(f *connectortest.Fake, _ *fakePartner) connector.Connector {
		f.OnQueryRefund = func(context.Context, connector.QueryRefundRequest) (connector.RefundStatus, error) {
			return connector.RefundStatus{}, errors.New("acme: refund not found")
		}
		return f
	}))
	expectBoth(t, r, "RFD-2")
}
