package conformance_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"bitbucket.org/hasaki-tech/payment-connector-sdk/conformance"
	"bitbucket.org/hasaki-tech/payment-connector-sdk/connector"
	"bitbucket.org/hasaki-tech/payment-connector-sdk/connectortest"
	"bitbucket.org/hasaki-tech/payment-connector-sdk/paykit"
	"bitbucket.org/hasaki-tech/payment-connector-sdk/providerhttp"
)

// fakePartner là đối tác giả trong tiến trình; connector tốt dựng trên connectortest.Fake hỏi nó mỗi lần "gọi mạng".
type fakePartner struct {
	mu      sync.Mutex
	mode    conformance.Mode
	reqs    int // request nghiệp vụ đã nhận (conformance.RequestCounter)
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

func (p *fakePartner) BusinessRequests() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.reqs
}

// arriveLater mô phỏng request đang bay: đối tác giả nhận (và đếm) nó sau d, khi connector đã trả về.
func (p *fakePartner) arriveLater(d time.Duration) {
	time.AfterFunc(d, func() {
		p.mu.Lock()
		p.reqs++
		p.mu.Unlock()
	})
}

// call mô phỏng một lần gọi mạng của connector viết đúng. ModeDown là chưa nhận nên không đếm.
func (p *fakePartner) call(ctx context.Context) error {
	p.mu.Lock()
	mode := p.mode
	if mode != conformance.ModeDown {
		p.reqs++
	}
	p.mu.Unlock()
	switch mode {
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

// Câu trả lời huỷ cho giao dịch đã thu: "đã thu" (on_hold) hoặc "chưa rõ" qua CXL-4. err nil kèm trạng thái
// khác (pending, failed) được core xếp chung với RejectedError nên bắn CXL-4, như "chắc chắn chưa xử lý".
func TestCheck_CancelPaidAnswers(t *testing.T) {
	for name, tc := range map[string]struct {
		res         connector.CancelResult
		err         error
		wantFinding bool
	}{
		"on_hold":     {res: connector.CancelResult{Status: connector.StatusOnHold}},
		"pending":     {res: connector.CancelResult{Status: connector.StatusPending}, wantFinding: true},
		"failed":      {res: connector.CancelResult{Status: connector.StatusFailed}, wantFinding: true},
		"unknown":     {err: fmt.Errorf("%w: không phân biệt được", connector.ErrUnknownOutcome)},
		"unavailable": {err: fmt.Errorf("%w: giả", connector.ErrProviderUnavailable), wantFinding: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := conformance.Check(t, harness(func(f *connectortest.Fake, _ *fakePartner) connector.Connector {
				good := f.OnCancel
				f.OnCancel = func(ctx context.Context, req connector.CancelRequest) (connector.CancelResult, error) {
					res, err := good(ctx, req)
					if err == nil && res.Status == connector.StatusSucceeded {
						return tc.res, tc.err
					}
					return res, err
				}
				return f
			}))
			if tc.wantFinding {
				expectBoth(t, r, "CXL-4")
			} else if len(r.Findings) != 0 {
				t.Fatalf("\"đã thu\" hoặc \"chưa rõ\" không được sinh finding:%s", dump(r))
			}
		})
	}
}

// Connector huỷ theo mã giao dịch khi thiếu ProviderRef: gọi đối tác, gặp treo rồi trả "bị từ chối" (chờ hết
// hạn của luật, hoặc có timeout nội bộ ngắn hơn). Bộ đếm của đối tác giả thấy request đã tới nên bắt được.
func TestCheck_Catches_CancelWithoutRefHangAsRejected(t *testing.T) {
	for name, internal := range map[string]time.Duration{"chờ hết hạn ctx": 0, "timeout nội bộ 20ms": 20 * time.Millisecond} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := conformance.Check(t, harness(func(f *connectortest.Fake, p *fakePartner) connector.Connector {
				good := f.OnCancel
				f.OnCancel = func(ctx context.Context, req connector.CancelRequest) (connector.CancelResult, error) {
					if req.ProviderRef != "" {
						return good(ctx, req)
					}
					if internal > 0 {
						var cancel context.CancelFunc
						ctx, cancel = context.WithTimeout(ctx, internal)
						defer cancel()
					}
					if err := p.call(ctx); err != nil {
						return connector.CancelResult{}, &connector.RejectedError{Code: "cannot_cancel", Message: err.Error()}
					}
					return connector.CancelResult{Status: connector.StatusCanceled}, nil
				}
				return f
			}))
			expectBoth(t, r, "CXL-2")
		})
	}
}

// Đối tác giả không cài RequestCounter: lượt huỷ thiếu ProviderRef khi treo không đoán bằng thời gian mà bị bỏ,
// có lý do; các phần khác của CXL-2 vẫn chạy (vi phạm ở lượt có mã vẫn bị bắt).
func TestCheck_CancelWithoutRefOnHang_SkippedWithoutCounter(t *testing.T) {
	t.Parallel()
	h := harness(nil)
	inner := h.New
	h.New = func(tb testing.TB) (connector.Connector, conformance.Partner) {
		c, p := inner(tb)
		return c, plainPartner{p}
	}
	r := conformance.Check(t, h)
	if len(r.Findings) != 0 {
		t.Fatalf("connector tốt không được có finding:%s", dump(r))
	}
	for _, code := range []string{"CXL-2", "CXL-2/grpc"} {
		if !slices.ContainsFunc(r.Skipped, func(s string) bool {
			return strings.HasPrefix(s, code+": ") && strings.Contains(s, "RequestCounter")
		}) {
			t.Errorf("thiếu %s trong Skipped với lý do nêu RequestCounter: %v", code, r.Skipped)
		}
	}
	if len(r.Skipped) != 2 {
		t.Errorf("chỉ lượt thêm của CXL-2 bị bỏ: %v", r.Skipped)
	}
}

// plainPartner giấu mọi interface tuỳ chọn của đối tác giả (RequestCounter).
type plainPartner struct{ conformance.Partner }

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

// Huỷ treo/rác mà connector bịa "nil + pending" (không gọi đối tác, chỉ nhìn mode): core xếp nó chung với
// RejectedError và quyết theo truy vấn lại, trong khi không ai biết lệnh huỷ đã tới chưa. Lượt có mã bắt nó.
func TestCheck_Catches_CancelPendingOnHang(t *testing.T) {
	t.Parallel()
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
	expectBoth(t, r, "CXL-2")
}

// Connector gọi đối tác thật, gặp treo hay thân rác thì trả "nil + pending" thay vì ErrUnknownOutcome. Ca "chỉ
// khi thiếu ProviderRef" chỉ lượt mã rỗng bắt được (bộ đếm thấy request đã tới đối tác).
func TestCheck_Catches_CancelTimeoutAsPending(t *testing.T) {
	for name, onlyWithoutRef := range map[string]bool{"mọi lời huỷ": false, "chỉ khi thiếu ProviderRef": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := conformance.Check(t, harness(func(f *connectortest.Fake, p *fakePartner) connector.Connector {
				good := f.OnCancel
				f.OnCancel = func(ctx context.Context, req connector.CancelRequest) (connector.CancelResult, error) {
					if onlyWithoutRef && req.ProviderRef != "" {
						return good(ctx, req)
					}
					if err := p.call(ctx); err != nil {
						if connector.Classify(err) == connector.CallUnknown {
							return connector.CancelResult{Status: connector.StatusPending}, nil
						}
						return connector.CancelResult{}, err
					}
					return good(ctx, req)
				}
				return f
			}))
			expectBoth(t, r, "CXL-2")
		})
	}
}

// Request tới đối tác giả trễ ~50 ms sau khi connector đã trả về (connector bỏ request đang bay): nhánh chờ
// settleWait phải thấy nó, nên lời từ chối vẫn bị bắt chứ không bị coi là quyết định tại chỗ.
func TestCheck_Catches_CancelWithoutRefLateArrival(t *testing.T) {
	t.Parallel()
	r := conformance.Check(t, harness(func(f *connectortest.Fake, p *fakePartner) connector.Connector {
		good := f.OnCancel
		f.OnCancel = func(ctx context.Context, req connector.CancelRequest) (connector.CancelResult, error) {
			if req.ProviderRef != "" || p.currentMode() != conformance.ModeHang {
				return good(ctx, req)
			}
			p.arriveLater(50 * time.Millisecond)
			return connector.CancelResult{}, &connector.RejectedError{Code: "cannot_cancel"}
		}
		return f
	}))
	expectBoth(t, r, "CXL-2")
}

// customerJSON là thân request kiểu đối tác thật: khách đi nguyên văn trong JSON.
func customerJSON(c paykit.Customer) string {
	b, _ := json.Marshal(map[string]string{"name": c.Name, "email": c.Email, "phone": c.Phone, "address": c.Address})
	return string(b)
}

// asciiEscaped là JSON mà mọi ký tự ngoài ASCII thành \uXXXX (như json_encode mặc định của nhiều ngôn ngữ).
func asciiEscaped(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r < 0x80 {
			b.WriteRune(r)
			continue
		}
		fmt.Fprintf(&b, `\u%04x`, r)
	}
	return b.String()
}

// Connector viết ẩu: đặt dữ liệu khách vào RawExchange ở nhiều dạng, trên từng thao tác. LOG-1 phải bắt
// mọi ca, cả qua gRPC (RawExchange đi qua dây).
func TestCheck_Catches_CustomerDataInRaw(t *testing.T) {
	leaks := map[string]func(c paykit.Customer) connector.RawExchange{
		"thô trong request": func(c paykit.Customer) connector.RawExchange {
			return connector.RawExchange{RequestBody: customerJSON(c)}
		},
		"\\u trong response": func(c paykit.Customer) connector.RawExchange {
			return connector.RawExchange{ResponseBody: `{"holder":"` + asciiEscaped(c.Name) + `"}`}
		},
		"form URL-encode": func(c paykit.Customer) connector.RawExchange {
			return connector.RawExchange{RequestBody: "email=" + url.QueryEscape(c.Email) + "&amount=1"}
		},
		"email viết hoa": func(c paykit.Customer) connector.RawExchange {
			return connector.RawExchange{ResponseBody: strings.ToUpper(c.Email)}
		},
		"điện thoại +84": func(c paykit.Customer) connector.RawExchange {
			return connector.RawExchange{RequestBody: `{"msisdn":"+84` + strings.TrimPrefix(c.Phone, "0") + `"}`}
		},
		"địa chỉ thô": func(c paykit.Customer) connector.RawExchange {
			return connector.RawExchange{RequestBody: "bill_to=" + url.QueryEscape(c.Address)}
		},
		"một dòng của địa chỉ": func(c paykit.Customer) connector.RawExchange {
			line, _, _ := strings.Cut(c.Address, "\n")
			return connector.RawExchange{ResponseBody: `{"street":"` + line + `"}`}
		},
	}
	for name, leak := range leaks {
		t.Run("create/"+name, func(t *testing.T) {
			t.Parallel()
			r := conformance.Check(t, harness(func(f *connectortest.Fake, _ *fakePartner) connector.Connector {
				good := f.OnCreate
				f.OnCreate = func(ctx context.Context, req connector.CreatePaymentRequest) (connector.CreatePaymentResult, error) {
					res, err := good(ctx, req)
					res.Raw = leak(req.Customer)
					return res, err
				}
				return f
			}))
			expectBoth(t, r, "LOG-1")
		})
	}
	t.Run("charge", func(t *testing.T) {
		t.Parallel()
		r := conformance.Check(t, harness(func(f *connectortest.Fake, _ *fakePartner) connector.Connector {
			good := f.OnCharge
			f.OnCharge = func(ctx context.Context, req connector.ChargeRequest) (connector.ChargeResult, error) {
				res, err := good(ctx, req)
				res.Raw = connector.RawExchange{RequestBody: customerJSON(req.Customer)}
				return res, err
			}
			return f
		}))
		expectBoth(t, r, "LOG-1")
	})
	t.Run("query nhắc lại khách đã gửi lúc tạo", func(t *testing.T) {
		t.Parallel()
		r := conformance.Check(t, harness(func(f *connectortest.Fake, _ *fakePartner) connector.Connector {
			var mu sync.Mutex
			seen := map[string]paykit.Customer{}
			goodCreate, goodQuery := f.OnCreate, f.OnQuery
			f.OnCreate = func(ctx context.Context, req connector.CreatePaymentRequest) (connector.CreatePaymentResult, error) {
				mu.Lock()
				seen[req.TransactionCode] = req.Customer
				mu.Unlock()
				return goodCreate(ctx, req)
			}
			f.OnQuery = func(ctx context.Context, req connector.QueryPaymentRequest) (connector.PaymentStatus, error) {
				st, err := goodQuery(ctx, req)
				mu.Lock()
				c := seen[req.TransactionCode]
				mu.Unlock()
				st.Raw = connector.RawExchange{ResponseBody: customerJSON(c)}
				return st, err
			}
			return f
		}))
		expectBoth(t, r, "LOG-1")
	})
}

// Connector che bằng providerhttp.Redact thì qua LOG-1 dù thân có đủ trường của khách.
func TestCheck_RedactedRawPasses(t *testing.T) {
	t.Parallel()
	r := conformance.Check(t, harness(func(f *connectortest.Fake, _ *fakePartner) connector.Connector {
		good := f.OnCreate
		f.OnCreate = func(ctx context.Context, req connector.CreatePaymentRequest) (connector.CreatePaymentResult, error) {
			res, err := good(ctx, req)
			c := req.Customer
			res.Raw = connector.RawExchange{RequestBody: providerhttp.Redact(customerJSON(c), c.Name, c.Email, c.Phone, c.Address)}
			return res, err
		}
		return f
	}))
	if len(r.Findings) != 0 {
		t.Fatalf("connector đã che không được có finding:%s", dump(r))
	}
}
