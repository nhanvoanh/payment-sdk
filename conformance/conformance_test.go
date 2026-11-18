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
	expectOnlyGRPCSkips(t, r)
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
	expectBoth(t, r, "PAY-4")
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
	r := conformance.Check(t, withTamperer(harness(func(f *connectortest.Fake, _ *fakePartner) connector.Connector {
		f.OnParseWebhook = func(_ context.Context, raw connector.RawWebhook) (connector.ProviderEvent, error) {
			var b hookBody // không kiểm chữ ký
			if err := json.Unmarshal(raw.Body, &b); err != nil {
				return connector.ProviderEvent{}, err
			}
			return connector.ProviderEvent{EventID: "evt_" + b.Tx + "_" + b.Status, Kind: connector.EventKindPayment,
				TransactionCode: b.Tx, Status: connector.Status(b.Status)}, nil
		}
		return f
	})))
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
	r := conformance.Check(t, harness(func(f *connectortest.Fake, p *fakePartner) connector.Connector {
		good := f.OnCancel
		f.OnCancel = func(ctx context.Context, req connector.CancelRequest) (connector.CancelResult, error) {
			if p.currentMode() != conformance.ModeNormal {
				return good(ctx, req)
			}
			return connector.CancelResult{Status: connector.StatusCanceled}, nil // không hỏi đối tác
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
	expectBoth(t, r, "RFD-1")
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
	want := []string{"PAY-3", "PAY-4", "PAY-5", "PAY-6", "QRY-1", "QRY-2", "QRY-3", "QRY-4", "CHG-1", "CXL-1", "CXL-2", "CXL-3",
		"CXL-4", "WHK-1", "WHK-2", "WHK-3", "WHK-4", "WHK-5", "WHK-6", "RFD-1", "RFD-2", "RFD-3"}
	for _, code := range want {
		for _, c := range []string{code, code + "/grpc"} {
			if !slices.ContainsFunc(r.Skipped, func(s string) bool { return strings.HasPrefix(s, c+": ") && len(s) > len(c)+2 }) {
				t.Errorf("thiếu %q trong Skipped (kèm lý do): %v", c, r.Skipped)
			}
		}
	}
	if !slices.Equal(r.NotChecked, []string{"CTX-1/grpc: proxy gRPC gặp ctx đã huỷ thì trả về ngay mà không gửi, connector không nhận được lời gọi nào để kiểm"}) {
		t.Errorf("thao tác không khai thì WHK-6/grpc vào Skipped, chỉ CTX-1/grpc vào NotChecked: %v", r.NotChecked)
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

// tamperingPartner cài conformance.Tamperer: đổi trạng thái trong thân đã ký, giữ nguyên chữ ký.
type tamperingPartner struct{ *fakePartner }

func (p tamperingPartner) TamperedWebhook(raw connector.RawWebhook) connector.RawWebhook {
	var b hookBody
	_ = json.Unmarshal(raw.Body, &b)
	b.Status = string(connector.StatusFailed)
	raw.Body, _ = json.Marshal(b)
	return raw
}

func withTamperer(h conformance.Harness) conformance.Harness {
	inner := h.New
	h.New = func(tb testing.TB) (connector.Connector, conformance.Partner) {
		c, p := inner(tb)
		return c, tamperingPartner{p.(*fakePartner)}
	}
	return h
}

// createInMode dựng wrapper cho Create: ở mode m thì (nếu treo thì chờ như connector thật) trả err.
func createInMode(m conformance.Mode, err error) func(f *connectortest.Fake, p *fakePartner) connector.Connector {
	return func(f *connectortest.Fake, p *fakePartner) connector.Connector {
		good := f.OnCreate
		f.OnCreate = func(ctx context.Context, req connector.CreatePaymentRequest) (connector.CreatePaymentResult, error) {
			if p.currentMode() == m {
				if m == conformance.ModeHang {
					p.hang(ctx)
				}
				return connector.CreatePaymentResult{}, err
			}
			return good(ctx, req)
		}
		return f
	}
}

var errUnknownAndUnavailable = fmt.Errorf("%w; %w", connector.ErrUnknownOutcome, connector.ErrProviderUnavailable)

func TestCheck_Catches_TimeoutReportedAsNotSupported(t *testing.T) {
	t.Parallel()
	expectBoth(t, conformance.Check(t, harness(createInMode(conformance.ModeHang, connector.ErrNotSupported))), "PAY-4")
}

func TestCheck_Catches_GarbageReportedAsNotSupported(t *testing.T) {
	t.Parallel()
	expectBoth(t, conformance.Check(t, harness(createInMode(conformance.ModeGarbage, connector.ErrNotSupported))), "PAY-6")
}

func TestCheck_Catches_DownWrappedWithUnknown(t *testing.T) {
	t.Parallel()
	expectBoth(t, conformance.Check(t, harness(createInMode(conformance.ModeDown, errUnknownAndUnavailable))), "PAY-3")
}

func TestCheck_Catches_RejectWrappedWithUnknown(t *testing.T) {
	t.Parallel()
	rej := fmt.Errorf("%w: %w", connector.ErrUnknownOutcome, &connector.RejectedError{Code: "R01"})
	expectBoth(t, conformance.Check(t, harness(createInMode(conformance.ModeReject, rej))), "PAY-5")
}

func TestCheck_Catches_HangWrappedWithUnknownIsFine(t *testing.T) {
	t.Parallel()
	// Lỗi bọc cả ErrUnknownOutcome lẫn ErrProviderUnavailable: core xét ErrUnknownOutcome trước nên là chưa rõ, đạt.
	r := conformance.Check(t, harness(createInMode(conformance.ModeHang, errUnknownAndUnavailable)))
	if len(r.Findings) != 0 {
		t.Fatalf("không được có finding:%s", dump(r))
	}
}

func TestCheck_Catches_ChargeTimeoutAsUnavailable(t *testing.T) {
	t.Parallel()
	r := conformance.Check(t, harness(func(f *connectortest.Fake, p *fakePartner) connector.Connector {
		good := f.OnCharge
		f.OnCharge = func(ctx context.Context, req connector.ChargeRequest) (connector.ChargeResult, error) {
			if p.currentMode() == conformance.ModeHang {
				p.hang(ctx)
				return connector.ChargeResult{}, connector.ErrProviderUnavailable
			}
			return good(ctx, req)
		}
		return f
	}))
	expectBoth(t, r, "CHG-1")
}

func TestCheck_Catches_ChargeGarbageAsRejected(t *testing.T) {
	t.Parallel()
	r := conformance.Check(t, harness(func(f *connectortest.Fake, p *fakePartner) connector.Connector {
		good := f.OnCharge
		f.OnCharge = func(ctx context.Context, req connector.ChargeRequest) (connector.ChargeResult, error) {
			if p.currentMode() == conformance.ModeGarbage {
				return connector.ChargeResult{}, &connector.RejectedError{Code: "PARSE"}
			}
			return good(ctx, req)
		}
		return f
	}))
	expectBoth(t, r, "CHG-1")
}

func TestCheck_Catches_RefundGarbageAsUnavailable(t *testing.T) {
	t.Parallel()
	r := conformance.Check(t, harness(func(f *connectortest.Fake, p *fakePartner) connector.Connector {
		good := f.OnRefund
		f.OnRefund = func(ctx context.Context, req connector.RefundRequest) (connector.RefundResult, error) {
			if p.currentMode() == conformance.ModeGarbage {
				return connector.RefundResult{}, connector.ErrProviderUnavailable
			}
			return good(ctx, req)
		}
		return f
	}))
	expectBoth(t, r, "RFD-1")
}

func TestCheck_Catches_CancelTimeoutClaimsCanceled(t *testing.T) {
	t.Parallel()
	r := conformance.Check(t, harness(func(f *connectortest.Fake, p *fakePartner) connector.Connector {
		good := f.OnCancel
		f.OnCancel = func(ctx context.Context, req connector.CancelRequest) (connector.CancelResult, error) {
			if p.currentMode() == conformance.ModeHang {
				p.hang(ctx)
				return connector.CancelResult{Status: connector.StatusCanceled}, nil
			}
			return good(ctx, req)
		}
		return f
	}))
	expectBoth(t, r, "CXL-2")
}

// Connector chỉ huỷ được theo ProviderRef mà báo thiếu mã bằng ErrProviderUnavailable (hay "chưa rõ"):
// core không bao giờ cho hết hạn giao dịch mà Create chưa nghe được trả lời.
func TestCheck_Catches_CancelWithoutRefNotDefinite(t *testing.T) {
	for name, bad := range map[string]error{
		"unavailable": fmt.Errorf("%w: thiếu ProviderRef", connector.ErrProviderUnavailable),
		"unknown":     fmt.Errorf("%w: thiếu ProviderRef", connector.ErrUnknownOutcome),
		"lỗi thường":  errors.New("thiếu ProviderRef"),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := conformance.Check(t, harness(func(f *connectortest.Fake, _ *fakePartner) connector.Connector {
				good := f.OnCancel
				f.OnCancel = func(ctx context.Context, req connector.CancelRequest) (connector.CancelResult, error) {
					if req.ProviderRef == "" {
						return connector.CancelResult{}, bad
					}
					return good(ctx, req)
				}
				return f
			}))
			expectBoth(t, r, "CXL-3")
		})
	}
}

// Đối tác báo đã thu mà connector trả RejectedError: core truy vấn lại, truy vấn chậm còn báo chờ thì
// trước đây giao dịch bị cho hết hạn trong khi khách đã trả. Phải trả "đã thu" (err nil, succeeded/on_hold).
func TestCheck_Catches_CancelPaidAsRejected(t *testing.T) {
	t.Parallel()
	r := conformance.Check(t, harness(func(f *connectortest.Fake, _ *fakePartner) connector.Connector {
		good := f.OnCancel
		f.OnCancel = func(ctx context.Context, req connector.CancelRequest) (connector.CancelResult, error) {
			res, err := good(ctx, req)
			if err == nil && res.Status == connector.StatusSucceeded {
				return connector.CancelResult{}, &connector.RejectedError{Code: "already_paid"}
			}
			return res, err
		}
		return f
	}))
	expectBoth(t, r, "CXL-4")
}

// Trả "đã thu" bằng on_hold cũng qua CXL-4; trả trạng thái khác (pending) thì không.
func TestCheck_CancelPaidAnswers(t *testing.T) {
	for st, wantFinding := range map[connector.Status]bool{connector.StatusOnHold: false, connector.StatusPending: true} {
		t.Run(string(st), func(t *testing.T) {
			t.Parallel()
			r := conformance.Check(t, harness(func(f *connectortest.Fake, _ *fakePartner) connector.Connector {
				good := f.OnCancel
				f.OnCancel = func(ctx context.Context, req connector.CancelRequest) (connector.CancelResult, error) {
					res, err := good(ctx, req)
					if err == nil && res.Status == connector.StatusSucceeded {
						return connector.CancelResult{Status: st}, nil
					}
					return res, err
				}
				return f
			}))
			if wantFinding {
				expectBoth(t, r, "CXL-4")
			} else if len(r.Findings) != 0 {
				t.Fatalf("on_hold là câu trả lời \"đã thu\" hợp lệ:%s", dump(r))
			}
		})
	}
}

// Connector huỷ theo mã giao dịch khi thiếu ProviderRef, và báo đối tác treo là "bị từ chối" (gọi xong rồi
// mới đổi lỗi, hoặc đổi luôn): core hiểu là đối tác nói rõ không huỷ trong khi không ai biết lệnh huỷ đã tới chưa.
func TestCheck_Catches_CancelWithoutRefHangAsRejected(t *testing.T) {
	for name, waits := range map[string]bool{"chờ đối tác rồi đổi lỗi": true, "đổi lỗi ngay": false} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := conformance.Check(t, harness(func(f *connectortest.Fake, p *fakePartner) connector.Connector {
				good := f.OnCancel
				f.OnCancel = func(ctx context.Context, req connector.CancelRequest) (connector.CancelResult, error) {
					if req.ProviderRef == "" && p.currentMode() == conformance.ModeHang {
						if waits {
							p.hang(ctx)
						}
						return connector.CancelResult{}, &connector.RejectedError{Code: "timeout"}
					}
					return good(ctx, req)
				}
				return f
			}))
			expectBoth(t, r, "CXL-2")
		})
	}
}

// Các câu trả lời dứt khoát đều qua CXL-3.
func TestCheck_CancelWithoutRefDefiniteAnswersPass(t *testing.T) {
	for name, ans := range map[string]func() (connector.CancelResult, error){
		"rejected": func() (connector.CancelResult, error) {
			return connector.CancelResult{}, &connector.RejectedError{Code: "cannot_cancel"}
		},
		"not supported": func() (connector.CancelResult, error) { return connector.CancelResult{}, connector.ErrNotSupported },
		"pending": func() (connector.CancelResult, error) {
			return connector.CancelResult{Status: connector.StatusPending}, nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := conformance.Check(t, harness(func(f *connectortest.Fake, _ *fakePartner) connector.Connector {
				good := f.OnCancel
				f.OnCancel = func(ctx context.Context, req connector.CancelRequest) (connector.CancelResult, error) {
					if req.ProviderRef == "" {
						return ans()
					}
					return good(ctx, req)
				}
				return f
			}))
			// Từ chối tại chỗ (không gọi đối tác) cho mọi mode cũng không phải "đổi đối tác treo thành từ chối".
			if len(r.Findings) != 0 {
				t.Fatalf("câu trả lời dứt khoát khi thiếu ProviderRef không được sinh finding:%s", dump(r))
			}
		})
	}
}

func TestCheck_Catches_CancelGarbageAsNotSupported(t *testing.T) {
	t.Parallel()
	r := conformance.Check(t, harness(func(f *connectortest.Fake, p *fakePartner) connector.Connector {
		good := f.OnCancel
		f.OnCancel = func(ctx context.Context, req connector.CancelRequest) (connector.CancelResult, error) {
			if p.currentMode() == conformance.ModeGarbage {
				return connector.CancelResult{}, connector.ErrNotSupported
			}
			return good(ctx, req)
		}
		return f
	}))
	expectBoth(t, r, "CXL-2")
}

// parseFirst phân tích JSON trước rồi mới kiểm chữ ký, và báo chữ ký sai bằng lỗi thường.
func parseFirst(f *connectortest.Fake, _ *fakePartner) connector.Connector {
	good := f.OnParseWebhook
	f.OnParseWebhook = func(ctx context.Context, raw connector.RawWebhook) (connector.ProviderEvent, error) {
		var b hookBody
		if err := json.Unmarshal(raw.Body, &b); err != nil {
			return connector.ProviderEvent{}, fmt.Errorf("acme: malformed body: %w", err)
		}
		ev, err := good(ctx, raw)
		if errors.Is(err, connector.ErrInvalidSignature) {
			return connector.ProviderEvent{}, errors.New("acme: bad signature")
		}
		return ev, err
	}
	return f
}

func TestCheck_WHK4_ErrorWithoutTampererIsNotViolation(t *testing.T) {
	t.Parallel()
	r := conformance.Check(t, harness(parseFirst))
	if len(r.Findings) != 0 {
		t.Fatalf("lỗi khác ErrInvalidSignature khi không có Tamperer không phải vi phạm:%s", dump(r))
	}
}

func TestCheck_Catches_TamperedNotInvalidSignatureWithTamperer(t *testing.T) {
	t.Parallel()
	expectBoth(t, conformance.Check(t, withTamperer(harness(parseFirst))), "WHK-4")
}

func TestCheck_GoodFakeWithTampererPassesEverything(t *testing.T) {
	t.Parallel()
	r := conformance.Check(t, withTamperer(harness(nil)))
	if len(r.Findings) != 0 {
		t.Fatalf("connector tốt phải qua hết:%s", dump(r))
	}
	expectOnlyGRPCSkips(t, r)
}

func TestCheck_Catches_WebhookWrongKind(t *testing.T) {
	t.Parallel()
	r := conformance.Check(t, harness(func(f *connectortest.Fake, _ *fakePartner) connector.Connector {
		good := f.OnParseWebhook
		f.OnParseWebhook = func(ctx context.Context, raw connector.RawWebhook) (connector.ProviderEvent, error) {
			ev, err := good(ctx, raw)
			ev.Kind = connector.EventKindRefund
			return ev, err
		}
		return f
	}))
	expectBoth(t, r, "WHK-1")
}

func TestCheck_SetupFailureIsSkippedNotFinding(t *testing.T) {
	t.Parallel()
	r := conformance.Check(t, harness(createInMode(conformance.ModeNormal, errors.New("acme: internal error"))))
	expectBoth(t, r, "PAY-1")
	for _, code := range []string{"QRY-2", "QRY-3", "QRY-4", "CXL-1", "CXL-2", "RFD-1"} {
		for _, c := range []string{code, code + "/grpc"} {
			if !slices.ContainsFunc(r.Skipped, func(s string) bool { return strings.HasPrefix(s, c+": ") && strings.Contains(s, "PAY-1") }) {
				t.Errorf("%s phải vào Skipped với lý do trỏ tới PAY-1: %v", c, r.Skipped)
			}
		}
	}
}

// expectOnlyGRPCSkips: connector khai đủ thao tác thì Skipped rỗng, NotChecked đúng hai lượt /grpc theo thiết kế.
func expectOnlyGRPCSkips(t *testing.T, r conformance.Report) {
	t.Helper()
	if len(r.Skipped) != 0 {
		t.Errorf("connector khai đủ thao tác không được bỏ luật nào: %v", r.Skipped)
	}
	var codes []string
	for _, s := range r.NotChecked {
		code, reason, _ := strings.Cut(s, ": ")
		if reason == "" {
			t.Errorf("NotChecked thiếu lý do: %q", s)
		}
		codes = append(codes, code)
	}
	if !slices.Equal(codes, []string{"WHK-6/grpc", "CTX-1/grpc"}) {
		t.Errorf("NotChecked phải đúng WHK-6/grpc và CTX-1/grpc: %v", r.NotChecked)
	}
}

// queryInMode: ở mode m (treo thì chờ như connector thật) QueryPayment trả st, nil.
func queryInMode(st connector.PaymentStatus, modes ...conformance.Mode) func(f *connectortest.Fake, p *fakePartner) connector.Connector {
	return func(f *connectortest.Fake, p *fakePartner) connector.Connector {
		good := f.OnQuery
		f.OnQuery = func(ctx context.Context, req connector.QueryPaymentRequest) (connector.PaymentStatus, error) {
			if m := p.currentMode(); slices.Contains(modes, m) {
				if m == conformance.ModeHang {
					p.hang(ctx)
				}
				return st, nil
			}
			return good(ctx, req)
		}
		return f
	}
}

func queryRefundInMode(st connector.RefundStatus, modes ...conformance.Mode) func(f *connectortest.Fake, p *fakePartner) connector.Connector {
	return func(f *connectortest.Fake, p *fakePartner) connector.Connector {
		good := f.OnQueryRefund
		f.OnQueryRefund = func(ctx context.Context, req connector.QueryRefundRequest) (connector.RefundStatus, error) {
			if m := p.currentMode(); slices.Contains(modes, m) {
				if m == conformance.ModeHang {
					p.hang(ctx)
				}
				return st, nil
			}
			return good(ctx, req)
		}
		return f
	}
}

var (
	foundFailed       = connector.PaymentStatus{Found: true, Status: connector.StatusFailed}
	refundFoundFailed = connector.RefundStatus{Found: true, Status: connector.StatusFailed}
)

func TestCheck_Catches_QueryGarbageAsFailed(t *testing.T) {
	t.Parallel()
	expectBoth(t, conformance.Check(t, harness(queryInMode(foundFailed, conformance.ModeGarbage))), "QRY-4")
}

func TestCheck_Catches_QueryGarbageAsNotFound(t *testing.T) {
	t.Parallel()
	expectBoth(t, conformance.Check(t, harness(queryInMode(connector.PaymentStatus{}, conformance.ModeGarbage))), "QRY-4")
}

func TestCheck_Catches_QueryTimeoutAsNotFound(t *testing.T) {
	t.Parallel()
	expectBoth(t, conformance.Check(t, harness(queryInMode(connector.PaymentStatus{}, conformance.ModeHang))), "QRY-3")
}

func TestCheck_Catches_QueryRefundTimeoutAsFailed(t *testing.T) {
	t.Parallel()
	expectBoth(t, conformance.Check(t, harness(queryRefundInMode(refundFoundFailed, conformance.ModeHang))), "RFD-3")
}

func TestCheck_Catches_QueryRefundGarbageAsFailed(t *testing.T) {
	t.Parallel()
	expectBoth(t, conformance.Check(t, harness(queryRefundInMode(refundFoundFailed, conformance.ModeGarbage))), "RFD-3")
}

func TestCheck_Catches_QueryRefundTimeoutAsNotFound(t *testing.T) {
	t.Parallel()
	expectBoth(t, conformance.Check(t, harness(queryRefundInMode(connector.RefundStatus{}, conformance.ModeHang))), "RFD-3")
}

func TestCheck_Catches_QueryRefundGarbageAsNotFound(t *testing.T) {
	t.Parallel()
	expectBoth(t, conformance.Check(t, harness(queryRefundInMode(connector.RefundStatus{}, conformance.ModeGarbage))), "RFD-3")
}

func TestCheck_Catches_CancelGarbageClaimsCanceled(t *testing.T) {
	t.Parallel()
	r := conformance.Check(t, harness(func(f *connectortest.Fake, p *fakePartner) connector.Connector {
		good := f.OnCancel
		f.OnCancel = func(ctx context.Context, req connector.CancelRequest) (connector.CancelResult, error) {
			if p.currentMode() == conformance.ModeGarbage {
				return connector.CancelResult{Status: connector.StatusCanceled}, nil
			}
			return good(ctx, req)
		}
		return f
	}))
	expectBoth(t, r, "CXL-2")
}

func TestCheck_CancelPendingOnHangIsAccepted(t *testing.T) {
	t.Parallel()
	// Huỷ treo/rác trả nil + pending: core hiểu là chưa huỷ và truy vấn lại, không mất tiền.
	r := conformance.Check(t, harness(func(f *connectortest.Fake, p *fakePartner) connector.Connector {
		good := f.OnCancel
		f.OnCancel = func(ctx context.Context, req connector.CancelRequest) (connector.CancelResult, error) {
			if m := p.currentMode(); m == conformance.ModeHang || m == conformance.ModeGarbage {
				return connector.CancelResult{Status: connector.StatusPending}, nil
			}
			return good(ctx, req)
		}
		return f
	}))
	if len(r.Findings) != 0 {
		t.Fatalf("nil + pending khi huỷ treo/rác được chấp nhận:%s", dump(r))
	}
}
