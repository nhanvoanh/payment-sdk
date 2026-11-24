package conformance_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
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

	"github.com/nhanvoanh/payment-sdk/conformance"
	"github.com/nhanvoanh/payment-sdk/connector"
	"github.com/nhanvoanh/payment-sdk/connectortest"
	"github.com/nhanvoanh/payment-sdk/paykit"
	"github.com/nhanvoanh/payment-sdk/providerhttp"
	"github.com/nhanvoanh/payment-sdk/redact"
)

// fakePartner là đối tác giả trong tiến trình; connector tốt dựng trên connectortest.Fake hỏi nó mỗi lần "gọi mạng".
type fakePartner struct {
	mu      sync.Mutex
	mode    conformance.Mode
	reqs    int      // request nghiệp vụ đã nhận (conformance.RequestCounter)
	orders  []string // mã đơn của các lệnh tạo đã nhận (conformance.RequestRecorder)
	refunds []string // mã hoàn của các lệnh hoàn đã nhận (conformance.RefundRecorder)
	pays    map[string]paid
	expiry  map[string]time.Time // hạn phiên đối tác đặt cho mỗi giao dịch (conformance.ExpiryReporter)
	release chan struct{}        // đóng khi test xong, để goroutine của connector treo được giải phóng
	logs    strings.Builder      // log connector ghi qua logf (conformance.LogReporter)
}

// ConnectorLogs cài conformance.LogReporter.
func (p *fakePartner) ConnectorLogs() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.logs.String()
}

// logf là logger mà connector giả ghi vào (như developer nối logger của connector vào một bộ đệm).
func (p *fakePartner) logf(format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	fmt.Fprintf(&p.logs, format+"\n", args...)
}

// PaymentExpiry cài conformance.ExpiryReporter.
func (p *fakePartner) PaymentExpiry(tx string) (time.Time, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.expiry[tx]
	return e, ok
}

// expiryOf: hạn phiên đối tác đặt lúc nhận lệnh tạo đầu tiên của tx (15 phút, tới giây, UTC).
func (p *fakePartner) expiryOf(tx string) time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.expiry[tx]; ok {
		return e
	}
	e := time.Now().Add(15 * time.Minute).UTC().Truncate(time.Second)
	p.expiry[tx] = e
	return e
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

func (p *fakePartner) OrderCodes() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.orders...)
}

func (p *fakePartner) RefundCodes() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.refunds...)
}

func (p *fakePartner) hasRefund(code string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Contains(p.refunds, code)
}

func (p *fakePartner) recordRefund(code string) {
	p.mu.Lock()
	p.refunds = append(p.refunds, code)
	p.mu.Unlock()
}

func (p *fakePartner) recordOrder(code string) {
	p.mu.Lock()
	p.orders = append(p.orders, code)
	p.mu.Unlock()
}

// orderCode là mã đơn phía đối tác của connector viết đúng: ngày tạo (từ CreatedAt) + mã giao dịch.
func orderCode(req connector.CreatePaymentRequest) string {
	return req.CreatedAt.UTC().Format("060102") + "_" + req.TransactionCode
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
	p := &fakePartner{pays: map[string]paid{}, expiry: map[string]time.Time{}, release: make(chan struct{})}
	tb.Cleanup(func() { close(p.release) })
	f := connectortest.New("acme", paykit.MethodVisa, paykit.MethodQR)
	f.OnCreate = func(ctx context.Context, req connector.CreatePaymentRequest) (connector.CreatePaymentResult, error) {
		if req.CreatedAt.IsZero() { // mã mang ngày tạo: không đoán ngày, chưa gửi gì (ID-5)
			return connector.CreatePaymentResult{}, fmt.Errorf("%w: core chưa gửi CreatedAt", connector.ErrProviderUnavailable)
		}
		if err := p.call(ctx); err != nil {
			return connector.CreatePaymentResult{}, err
		}
		p.recordOrder(orderCode(req))
		return connector.CreatePaymentResult{
			ProviderRef: "acme_" + req.TransactionCode,
			Status:      connector.StatusPending,
			NextAction:  paykit.NextAction{Kind: paykit.NextActionRedirect, URL: "https://pay.acme.example/" + req.TransactionCode},
			ExpiresAt:   p.expiryOf(req.TransactionCode),
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
		p.recordRefund(req.RefundCode)
		return connector.RefundResult{ProviderRef: "acme_" + req.RefundCode, Status: connector.StatusPending}, nil
	}
	f.OnQueryRefund = func(ctx context.Context, req connector.QueryRefundRequest) (connector.RefundStatus, error) {
		if err := p.call(ctx); err != nil {
			return connector.RefundStatus{}, err
		}
		// Tra theo mã hoàn (RefundCode), không cần ProviderRef: lệnh hoàn đã nhận luôn tìm được (RFD-4).
		if !p.hasRefund(req.RefundCode) {
			return connector.RefundStatus{Found: false}, nil
		}
		return connector.RefundStatus{Found: true, Status: connector.StatusPending, ProviderRef: "acme_" + req.RefundCode}, nil
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
	want := []string{"PAY-3", "PAY-4", "PAY-5", "PAY-6", "QRY-1", "QRY-2", "QRY-3", "QRY-4", "CHG-1", "CHG-2", "CXL-1", "CXL-2", "CXL-3",
		"CXL-4", "WHK-1", "WHK-2", "WHK-3", "WHK-4", "WHK-5", "WHK-6", "RFD-1", "RFD-2", "RFD-3", "RFD-4"}
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
	t.Parallel()
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

// Connector chỉ truy vấn được theo mã của đối tác nên trả "không thấy" khi ProviderRef rỗng: RFD-4 phải bắt, cả qua
// gRPC (core sẽ chốt not_sent một lệnh hoàn đối tác đã nhận).
func TestCheck_Catches_QueryRefundNotFoundWithoutProviderRef(t *testing.T) {
	t.Parallel()
	r := conformance.Check(t, harness(func(f *connectortest.Fake, p *fakePartner) connector.Connector {
		good := f.OnQueryRefund
		f.OnQueryRefund = func(ctx context.Context, req connector.QueryRefundRequest) (connector.RefundStatus, error) {
			if req.ProviderRef == "" && p.currentMode() == conformance.ModeNormal {
				return connector.RefundStatus{Found: false}, nil
			}
			return good(ctx, req)
		}
		return f
	}))
	expectBoth(t, r, "RFD-4")
}

// Connector không truy vấn được khi thiếu ProviderRef mà trả ErrUnknownOutcome cho MỌI truy vấn hoàn ProviderRef rỗng
// (không nhìn trạng thái đối tác, như connector thật) KHÔNG đạt: RFD-2 (lệnh hoàn đối tác không biết phải là
// Found=false, không lỗi) bắt, cả qua gRPC; RFD-3/RFD-4 không bắn (lỗi ở đó là "chưa rõ" hợp lệ). Hai lựa chọn đạt
// là truy vấn theo mã dựng từ RefundCode + CreatedAt, hoặc không khai OpQueryRefund (ca dưới).
func TestCheck_Catches_QueryRefundUnknownWithoutProviderRef(t *testing.T) {
	t.Parallel()
	r := conformance.Check(t, harness(func(f *connectortest.Fake, _ *fakePartner) connector.Connector {
		good := f.OnQueryRefund
		f.OnQueryRefund = func(ctx context.Context, req connector.QueryRefundRequest) (connector.RefundStatus, error) {
			if req.ProviderRef == "" {
				return connector.RefundStatus{}, connector.ErrUnknownOutcome
			}
			return good(ctx, req)
		}
		return f
	}))
	expectBoth(t, r, "RFD-2")
}

// Không khai OpQueryRefund (vẫn khai refund): RFD-2/3/4 vào Skipped kèm lý do, cả bản /grpc, và connector đạt.
func TestCheck_RefundQueryUndeclared_SkipsAndPasses(t *testing.T) {
	t.Parallel()
	r := conformance.Check(t, harness(func(f *connectortest.Fake, _ *fakePartner) connector.Connector {
		f.Capabilities[connector.OpQueryRefund] = false
		return f
	}))
	if len(r.Findings) != 0 {
		t.Fatalf("không khai truy vấn hoàn thì phải đạt:%s", dump(r))
	}
	for _, code := range []string{"RFD-2", "RFD-3", "RFD-4"} {
		for _, c := range []string{code, code + "/grpc"} {
			if !slices.ContainsFunc(r.Skipped, func(s string) bool { return strings.HasPrefix(s, c+": ") && len(s) > len(c)+2 }) {
				t.Errorf("thiếu %q trong Skipped (kèm lý do): %v", c, r.Skipped)
			}
		}
	}
	if slices.ContainsFunc(r.Skipped, func(s string) bool { return strings.HasPrefix(s, "RFD-1: ") }) {
		t.Errorf("RFD-1 vẫn áp dụng khi khai refund: %v", r.Skipped)
	}
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	others := slices.DeleteFunc(slices.Clone(r.Skipped), func(s string) bool {
		// thiếu RequestRecorder, ExpiryReporter, LogReporter
		return strings.HasPrefix(s, "ID-4") || strings.HasPrefix(s, "PAY-8") || strings.HasPrefix(s, "CHG-2")
	})
	if len(others) != 2 {
		t.Errorf("ngoài ID-4 (thiếu RequestRecorder), chỉ lượt thêm của CXL-2 bị bỏ: %v", r.Skipped)
	}
}

// plainPartner giấu mọi interface tuỳ chọn của đối tác giả (RequestCounter, RequestRecorder, ExpiryReporter, LogReporter).
type plainPartner struct{ conformance.Partner }

// Các câu trả lời dứt khoát đều qua CXL-3.
func TestCheck_CancelWithoutRefDefiniteAnswersPass(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
		"tên chủ thẻ bỏ dấu": func(c paykit.Customer) connector.RawExchange {
			return connector.RawExchange{RequestBody: `{"holder":"` + strings.ToUpper(redact.Fold(c.Name)) + `"}`}
		},
		"họ tên tách": func(c paykit.Customer) connector.RawExchange {
			f := strings.Fields(c.Name)
			return connector.RawExchange{RequestBody: `{"first":"` + f[len(f)-1] + `","last":"` + f[0] + `"}`}
		},
		"họ bỏ dấu đứng riêng": func(c paykit.Customer) connector.RawExchange {
			return connector.RawExchange{RequestBody: `{"last":"` + strings.ToUpper(redact.Fold(strings.Fields(c.Name)[0])) + `"}`}
		},
		"điện thoại có dấu cách": func(c paykit.Customer) connector.RawExchange {
			return connector.RawExchange{RequestBody: `{"tel":"` + c.Phone[:4] + " " + c.Phone[4:7] + " " + c.Phone[7:] + `"}`}
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
			res.Raw = connector.RawExchange{RequestBody: providerhttp.RedactCustomer(customerJSON(c), c),
				ResponseBody: providerhttp.RedactCustomer(splitEcho(c), c)}
			return res, err
		}
		return f
	}))
	if len(r.Findings) != 0 {
		t.Fatalf("connector đã che không được có finding:%s", dump(r))
	}
}

// splitEcho là phản hồi của đối tác tách họ/tên và bỏ dấu (tên chủ thẻ ASCII hoa, mỗi chữ một trường).
func splitEcho(c paykit.Customer) string {
	f := strings.Fields(strings.ToUpper(redact.Fold(c.Name)))
	return `{"last":"` + f[0] + `","middle":"` + strings.Join(f[1:len(f)-1], " ") + `","first":"` + f[len(f)-1] +
		`","holder":"` + strings.Join(f, " ") + `"}`
}

// Đối tác nhắc lại khách đã tách họ/tên bỏ dấu mà connector không che: LOG-1 bắt (cả qua gRPC).
func TestCheck_Catches_SplitFoldedNameEcho(t *testing.T) {
	t.Parallel()
	r := conformance.Check(t, harness(func(f *connectortest.Fake, _ *fakePartner) connector.Connector {
		good := f.OnCreate
		f.OnCreate = func(ctx context.Context, req connector.CreatePaymentRequest) (connector.CreatePaymentResult, error) {
			res, err := good(ctx, req)
			f := strings.Fields(strings.ToUpper(redact.Fold(req.Customer.Name)))
			res.Raw = connector.RawExchange{ResponseBody: `{"middle":"` + f[1] + `"}`}
			return res, err
		}
		return f
	}))
	expectBoth(t, r, "LOG-1")
}

// Mã đơn phía đối tác dựng từ đồng hồ lúc gọi (nano giây, như một nonce) thay vì CreatedAt: ID-4 bắt, cả qua gRPC.
func TestCheck_Catches_OrderCodeFromWallClock(t *testing.T) {
	t.Parallel()
	r := conformance.Check(t, harness(func(f *connectortest.Fake, p *fakePartner) connector.Connector {
		f.OnCreate = func(ctx context.Context, req connector.CreatePaymentRequest) (connector.CreatePaymentResult, error) {
			if err := p.call(ctx); err != nil {
				return connector.CreatePaymentResult{}, err
			}
			p.recordOrder(fmt.Sprintf("%d_%s", time.Now().UnixNano(), req.TransactionCode))
			return connector.CreatePaymentResult{ProviderRef: "acme_" + req.TransactionCode, Status: connector.StatusPending,
				NextAction: paykit.NextAction{Kind: paykit.NextActionRedirect, URL: "https://pay.acme.example/x"}}, nil
		}
		return f
	}))
	expectBoth(t, r, "ID-4")
}

// Ngày trong mã lấy từ đồng hồ (hôm nay) chứ không từ CreatedAt: hai lần gọi cùng ngày giống nhau, nhưng ID-4
// vẫn bắt vì mã mang ngày hôm nay trong khi CreatedAt ở ngày khác.
func TestCheck_Catches_OrderCodeWithTodaysDate(t *testing.T) {
	t.Parallel()
	r := conformance.Check(t, harness(func(f *connectortest.Fake, p *fakePartner) connector.Connector {
		f.OnCreate = func(ctx context.Context, req connector.CreatePaymentRequest) (connector.CreatePaymentResult, error) {
			if err := p.call(ctx); err != nil {
				return connector.CreatePaymentResult{}, err
			}
			p.recordOrder(time.Now().UTC().Format("060102") + "_" + req.TransactionCode)
			return connector.CreatePaymentResult{ProviderRef: "acme_" + req.TransactionCode, Status: connector.StatusPending,
				NextAction: paykit.NextAction{Kind: paykit.NextActionRedirect, URL: "https://pay.acme.example/x"}}, nil
		}
		return f
	}))
	expectBoth(t, r, "ID-4")
}

// noRecorder giấu RequestRecorder của đối tác giả.
type noRecorder struct{ *fakePartner }

func (noRecorder) OrderCodes() {}

// Đối tác giả không cài RequestRecorder: ID-4 vào Skipped kèm lý do, không phải vi phạm.
func TestCheck_ID4_SkippedWithoutRecorder(t *testing.T) {
	t.Parallel()
	h := harness(nil)
	h.New = func(tb testing.TB) (connector.Connector, conformance.Partner) {
		f, p := newGood(tb)
		return f, noRecorder{p}
	}
	r := conformance.Check(t, h)
	if len(r.Findings) != 0 {
		t.Fatalf("không được có finding:%s", dump(r))
	}
	for _, code := range []string{"ID-4", "ID-4/grpc"} {
		if !slices.Contains(r.Skipped, code+": đối tác giả không cài conformance.RequestRecorder") {
			t.Errorf("thiếu %s trong Skipped: %v", code, r.Skipped)
		}
	}
}

// Các thao tác không mang khách nhưng đối tác nhắc lại khách đã gửi lúc tạo: LOG-1 phải bắt từng thao tác.
func TestCheck_Catches_CustomerEchoedByOtherOperations(t *testing.T) {
	t.Parallel()
	type echo struct {
		mu   sync.Mutex
		last paykit.Customer
	}
	wrap := func(set func(f *connectortest.Fake, e *echo)) func(f *connectortest.Fake, _ *fakePartner) connector.Connector {
		return func(f *connectortest.Fake, _ *fakePartner) connector.Connector {
			e := &echo{}
			good := f.OnCreate
			f.OnCreate = func(ctx context.Context, req connector.CreatePaymentRequest) (connector.CreatePaymentResult, error) {
				e.mu.Lock()
				e.last = req.Customer
				e.mu.Unlock()
				return good(ctx, req)
			}
			set(f, e)
			return f
		}
	}
	body := func(e *echo) string {
		e.mu.Lock()
		defer e.mu.Unlock()
		return customerJSON(e.last)
	}
	cases := map[string]func(f *connectortest.Fake, e *echo){
		"refund": func(f *connectortest.Fake, e *echo) {
			good := f.OnRefund
			f.OnRefund = func(ctx context.Context, req connector.RefundRequest) (connector.RefundResult, error) {
				r, err := good(ctx, req)
				r.Raw.ResponseBody = body(e)
				return r, err
			}
		},
		"query_refund": func(f *connectortest.Fake, e *echo) {
			good := f.OnQueryRefund
			f.OnQueryRefund = func(ctx context.Context, req connector.QueryRefundRequest) (connector.RefundStatus, error) {
				r, err := good(ctx, req)
				r.Raw.ResponseBody = body(e)
				return r, err
			}
		},
		"cancel": func(f *connectortest.Fake, e *echo) {
			good := f.OnCancel
			f.OnCancel = func(ctx context.Context, req connector.CancelRequest) (connector.CancelResult, error) {
				r, err := good(ctx, req)
				r.Raw.ResponseBody = body(e)
				return r, err
			}
		},
		"webhook Raw": func(f *connectortest.Fake, e *echo) {
			good := f.OnParseWebhook
			f.OnParseWebhook = func(ctx context.Context, raw connector.RawWebhook) (connector.ProviderEvent, error) {
				ev, err := good(ctx, raw)
				ev.Raw = []byte(body(e))
				return ev, err
			}
		},
	}
	for name, set := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			expectBoth(t, conformance.Check(t, harness(wrap(set))), "LOG-1")
		})
	}
}

// createRecording thay OnCreate của connector tốt: gọi đối tác giả, ghi mã do code dựng, trả pending. refuseZero:
// CreatedAt zero thì ErrProviderUnavailable, không gọi đối tác (như connector tốt).
func createRecording(f *connectortest.Fake, p *fakePartner, refuseZero bool, code func(req connector.CreatePaymentRequest) string) {
	f.OnCreate = func(ctx context.Context, req connector.CreatePaymentRequest) (connector.CreatePaymentResult, error) {
		if refuseZero && req.CreatedAt.IsZero() {
			return connector.CreatePaymentResult{}, connector.ErrProviderUnavailable
		}
		if err := p.call(ctx); err != nil {
			return connector.CreatePaymentResult{}, err
		}
		p.recordOrder(code(req))
		return connector.CreatePaymentResult{ProviderRef: "acme_" + req.TransactionCode, Status: connector.StatusPending,
			NextAction: paykit.NextAction{Kind: paykit.NextActionRedirect, URL: "https://pay.acme.example/x"}}, nil
	}
}

// Mã mang ngày của CreatedAt lần đầu rồi nhớ theo mã giao dịch: CreatedAt khác ngày vẫn ra mã cũ. ID-4 bắt.
func TestCheck_Catches_OrderCodeIgnoresLaterCreatedAt(t *testing.T) {
	t.Parallel()
	r := conformance.Check(t, harness(func(f *connectortest.Fake, p *fakePartner) connector.Connector {
		var mu sync.Mutex
		first := map[string]string{}
		createRecording(f, p, true, func(req connector.CreatePaymentRequest) string {
			mu.Lock()
			defer mu.Unlock()
			if c, ok := first[req.TransactionCode]; ok {
				return c
			}
			first[req.TransactionCode] = orderCode(req)
			return first[req.TransactionCode]
		})
		return f
	}))
	expectBoth(t, r, "ID-4")
}

// Connector tự trả lại kết quả theo khoá idempotency (không gọi đối tác lần hai): ID-4 không so được, vào
// NotChecked kèm lý do, không phải vi phạm hay Skipped.
func TestCheck_ID4_NotCheckedWhenConnectorCaches(t *testing.T) {
	t.Parallel()
	r := conformance.Check(t, harness(func(f *connectortest.Fake, _ *fakePartner) connector.Connector {
		good := f.OnCreate
		var mu sync.Mutex
		cache := map[string]connector.CreatePaymentResult{}
		f.OnCreate = func(ctx context.Context, req connector.CreatePaymentRequest) (connector.CreatePaymentResult, error) {
			mu.Lock()
			res, ok := cache[req.IdempotencyKey]
			mu.Unlock()
			if ok {
				return res, nil
			}
			res, err := good(ctx, req)
			if err == nil {
				mu.Lock()
				cache[req.IdempotencyKey] = res
				mu.Unlock()
			}
			return res, err
		}
		return f
	}))
	if slices.Contains(rules(r), "ID-4") || slices.Contains(rules(r), "ID-4/grpc") {
		t.Fatalf("ID-4 không được bắn:%s", dump(r))
	}
	for _, code := range []string{"ID-4", "ID-4/grpc"} {
		if !slices.ContainsFunc(r.NotChecked, func(s string) bool { return strings.HasPrefix(s, code+": ") && strings.Contains(s, "idempotency") }) {
			t.Errorf("thiếu %s trong NotChecked: %v", code, r.NotChecked)
		}
	}
}

// CreatedAt zero (core cũ): connector đoán ngày 0001-01-01 vào mã đơn. ID-5 bắt.
func TestCheck_Catches_ZeroCreatedAtBecomesDate(t *testing.T) {
	t.Parallel()
	r := conformance.Check(t, harness(func(f *connectortest.Fake, p *fakePartner) connector.Connector {
		createRecording(f, p, false, orderCode)
		return f
	}))
	expectBoth(t, r, "ID-5")
}

// CreatedAt zero ở truy vấn: connector dựng mã sai rồi trả Found=false (đối tác "không thấy"). ID-5 bắt.
func TestCheck_Catches_QueryZeroCreatedAtNotFound(t *testing.T) {
	t.Parallel()
	r := conformance.Check(t, harness(func(f *connectortest.Fake, _ *fakePartner) connector.Connector {
		good := f.OnQuery
		f.OnQuery = func(ctx context.Context, req connector.QueryPaymentRequest) (connector.PaymentStatus, error) {
			if req.CreatedAt.IsZero() {
				return connector.PaymentStatus{Found: false}, nil
			}
			return good(ctx, req)
		}
		return f
	}))
	expectBoth(t, r, "ID-5")
}

// Connector không dùng ngày (mã = mã giao dịch): tạo với CreatedAt zero thành công và truy vấn thấy giao dịch, hợp lệ.
func TestCheck_ID5_DatelessConnectorPasses(t *testing.T) {
	t.Parallel()
	r := conformance.Check(t, harness(func(f *connectortest.Fake, p *fakePartner) connector.Connector {
		createRecording(f, p, false, func(req connector.CreatePaymentRequest) string { return req.TransactionCode })
		return f
	}))
	if len(r.Findings) != 0 {
		t.Fatalf("connector không dùng ngày không được có finding:%s", dump(r))
	}
}

// Connector bỏ qua ctx rồi trả lời TRỄ hơn hạn của luật (không treo mãi): goroutine của guard vẫn ghi kết quả sau
// khi luật đã bỏ đi. Bộ luật không được đọc kết quả đó (race thật, -race bắt) mà chỉ báo "không trả về".
func TestCheck_LateAnswerPastLimitIsNotRead(t *testing.T) {
	t.Parallel()
	r := conformance.Check(t, harness(func(f *connectortest.Fake, p *fakePartner) connector.Connector {
		good, goodQuery, goodParse := f.OnCreate, f.OnQuery, f.OnParseWebhook
		f.OnCreate = func(ctx context.Context, req connector.CreatePaymentRequest) (connector.CreatePaymentResult, error) {
			if p.currentMode() == conformance.ModeHang {
				time.Sleep(2600 * time.Millisecond) // quá 300 ms + 2 s của luật treo, rồi mới trả lời
				return connector.CreatePaymentResult{Status: connector.StatusFailed}, connector.ErrProviderUnavailable
			}
			return good(ctx, req)
		}
		// Hai ca dưới phủ (*env).query và (*env).parse (lời gọi thường, hạn 5 s + 1 s): bỏ đi thì bản query/parse
		// đọc biến do goroutine bị bỏ mặc ghi không còn test nào bắt.
		f.OnQuery = func(ctx context.Context, req connector.QueryPaymentRequest) (connector.PaymentStatus, error) {
			if strings.Contains(req.TransactionCode, "QRY1") {
				time.Sleep(6300 * time.Millisecond) // quá hạn 5 s + 1 s của lời gọi thường
				return connector.PaymentStatus{Found: true}, errors.New("trễ")
			}
			return goodQuery(ctx, req)
		}
		f.OnParseWebhook = func(ctx context.Context, raw connector.RawWebhook) (connector.ProviderEvent, error) {
			if strings.Contains(string(raw.Body), "CFWHK1") {
				time.Sleep(6300 * time.Millisecond)
				return connector.ProviderEvent{EventID: "trễ"}, errors.New("trễ")
			}
			return goodParse(ctx, raw)
		}
		return f
	}))
	got := rules(r)
	for _, want := range []string{"PAY-4", "QRY-1", "WHK-1"} {
		if !slices.Contains(got, want) {
			t.Errorf("Check không bắt được %s:%s", want, dump(r))
		}
	}
	for _, f := range r.Findings {
		// Lượt /grpc: proxy tự hết hạn và trả lỗi DeadlineExceeded, đó là câu trả lời thật của proxy, không phải kết quả trễ.
		if strings.HasSuffix(f.Rule, "/grpc") {
			continue
		}
		if !strings.Contains(f.Detail, "không trả về") && !strings.Contains(f.Detail, "ctx đã huỷ") {
			t.Errorf("%s đọc kết quả trả trễ thay vì báo không trả về: %s", f.Rule, f.Detail)
		}
	}
}

// Connector che vết ở nhánh thành công nhưng nhánh LỖI (đối tác từ chối hay trả rác) trả Raw thô còn khách: LOG-1
// phải bắt, cả qua gRPC (Raw đi qua dây kể cả khi lỗi).
func TestCheck_Catches_CustomerDataInRawOnError(t *testing.T) {
	t.Parallel()
	for _, op := range []string{"create", "charge"} {
		t.Run(op, func(t *testing.T) {
			t.Parallel()
			r := conformance.Check(t, harness(func(f *connectortest.Fake, _ *fakePartner) connector.Connector {
				if op == "create" {
					good := f.OnCreate
					f.OnCreate = func(ctx context.Context, req connector.CreatePaymentRequest) (connector.CreatePaymentResult, error) {
						res, err := good(ctx, req)
						if err != nil {
							res.Raw = connector.RawExchange{RequestBody: customerJSON(req.Customer), HTTPStatus: 422}
						}
						return res, err
					}
					return f
				}
				good := f.OnCharge
				f.OnCharge = func(ctx context.Context, req connector.ChargeRequest) (connector.ChargeResult, error) {
					res, err := good(ctx, req)
					if err != nil {
						res.Raw = connector.RawExchange{RequestBody: customerJSON(req.Customer), HTTPStatus: 422}
					}
					return res, err
				}
				return f
			}))
			expectBoth(t, r, "LOG-1")
		})
	}
}

// expiryWrap: connector tốt nhưng ExpiresAt của lệnh tạo đổi bằng f (nhận hạn đối tác đặt).
func expiryWrap(f func(partnerSet time.Time) time.Time) func(*connectortest.Fake, *fakePartner) connector.Connector {
	return func(fk *connectortest.Fake, _ *fakePartner) connector.Connector {
		good := fk.OnCreate
		fk.OnCreate = func(ctx context.Context, req connector.CreatePaymentRequest) (connector.CreatePaymentResult, error) {
			res, err := good(ctx, req)
			if err == nil {
				res.ExpiresAt = f(res.ExpiresAt)
			}
			return res, err
		}
		return fk
	}
}

// ExpiresAt ở quá khứ, sát hiện tại, hay quá 7 ngày: PAY-8 bắt, cả qua gRPC.
func TestCheck_Catches_ExpiryOutOfRange(t *testing.T) {
	t.Parallel()
	for name, f := range map[string]func(time.Time) time.Time{
		"quá khứ":    func(time.Time) time.Time { return time.Now().Add(-time.Minute).UTC() },
		"quá 7 ngày": func(time.Time) time.Time { return time.Now().Add(7*24*time.Hour + time.Hour).UTC() },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			expectBoth(t, conformance.Check(t, harness(expiryWrap(f))), "PAY-8")
		})
	}
}

// Đối tác gửi hạn giờ địa phương (UTC+7) không kèm offset, connector coi là UTC: hạn trễ 7 giờ nhưng vẫn trong
// khoảng. PAY-8 bắt nhờ ExpiryReporter, cả qua gRPC.
func TestCheck_Catches_ExpiryWrongZone(t *testing.T) {
	t.Parallel()
	r := conformance.Check(t, harness(expiryWrap(func(set time.Time) time.Time {
		local := set.In(time.FixedZone("UTC+7", 7*3600))
		return time.Date(local.Year(), local.Month(), local.Day(), local.Hour(), local.Minute(), local.Second(), 0, time.UTC)
	})))
	expectBoth(t, r, "PAY-8")
}

// Đúng thời điểm nhưng Location khác UTC: PAY-8 bắt ở lượt thẳng (gRPC luôn chở UTC nên lượt /grpc qua).
func TestCheck_Catches_ExpiryNotUTC(t *testing.T) {
	t.Parallel()
	r := conformance.Check(t, harness(expiryWrap(func(set time.Time) time.Time {
		return set.In(time.FixedZone("UTC+7", 7*3600))
	})))
	expectOnly(t, r, "PAY-8")
	if slices.Contains(rules(r), "PAY-8/grpc") {
		t.Errorf("PAY-8/grpc không được bắn (gRPC chở thời điểm, không chở múi giờ):%s", dump(r))
	}
}

// ExpiresAt zero (đối tác không báo hạn) là hợp lệ.
func TestCheck_ZeroExpiryPasses(t *testing.T) {
	t.Parallel()
	r := conformance.Check(t, harness(expiryWrap(func(time.Time) time.Time { return time.Time{} })))
	if len(r.Findings) != 0 {
		t.Fatalf("ExpiresAt zero không được có finding:%s", dump(r))
	}
}

// Đối tác giả không cài ExpiryReporter: PAY-8 vẫn kiểm khoảng (bắt hạn quá 7 ngày) và, khi không có vi phạm, vào
// Skipped với lý do nêu ExpiryReporter; lệch múi giờ trong khoảng thì không bắt được.
func TestCheck_PAY8_WithoutReporter(t *testing.T) {
	t.Parallel()
	h := func(f func(time.Time) time.Time) conformance.Harness {
		h := harness(expiryWrap(f))
		inner := h.New
		h.New = func(tb testing.TB) (connector.Connector, conformance.Partner) {
			c, p := inner(tb)
			return c, plainPartner{p}
		}
		return h
	}
	r := conformance.Check(t, h(func(set time.Time) time.Time { return set.Add(7 * time.Hour) }))
	if slices.Contains(rules(r), "PAY-8") || slices.Contains(rules(r), "PAY-8/grpc") {
		t.Fatalf("không có ExpiryReporter thì lệch trong khoảng không bắt được:%s", dump(r))
	}
	for _, code := range []string{"PAY-8", "PAY-8/grpc"} {
		if !slices.ContainsFunc(r.Skipped, func(s string) bool { return strings.HasPrefix(s, code+": ") && strings.Contains(s, "ExpiryReporter") }) {
			t.Errorf("thiếu %s trong Skipped với lý do nêu ExpiryReporter: %v", code, r.Skipped)
		}
	}
	r = conformance.Check(t, h(func(time.Time) time.Time { return time.Now().AddDate(0, 0, 30).UTC() }))
	expectBoth(t, r, "PAY-8")
}

// Mã đơn mang ngày đúng của CreatedAt nhưng giờ-phút-giây lấy từ đồng hồ lúc gọi: hai lần gọi liên tiếp thường rơi
// cùng một giây nên giống nhau. ID-4 chờ ~1,1 giây (lượt thẳng) rồi gọi lại cùng request, nên vẫn bắt.
func TestCheck_Catches_OrderCodeWithClockSeconds(t *testing.T) {
	t.Parallel()
	r := conformance.Check(t, harness(func(f *connectortest.Fake, p *fakePartner) connector.Connector {
		createRecording(f, p, true, func(req connector.CreatePaymentRequest) string {
			return req.CreatedAt.UTC().Format("060102") + time.Now().UTC().Format("150405") + "_" + req.TransactionCode
		})
		return f
	}))
	expectOnly(t, r, "ID-4")
}

// Mã hoàn gửi đối tác lấy thêm đồng hồ (nano giây): hai lần CreateRefund cùng request gửi hai mã khác nhau. ID-4
// bắt qua RefundRecorder, cả qua gRPC. Hệ quả của cùng lỗi: đối tác giữ lệnh hoàn dưới mã khác RefundCode nên truy
// vấn theo RefundCode không thấy, RFD-4 cũng bắn (được phép, không luật nào khác).
func TestCheck_Catches_RefundCodeFromWallClock(t *testing.T) {
	t.Parallel()
	r := conformance.Check(t, harness(func(f *connectortest.Fake, p *fakePartner) connector.Connector {
		f.OnRefund = func(ctx context.Context, req connector.RefundRequest) (connector.RefundResult, error) {
			if err := p.call(ctx); err != nil {
				return connector.RefundResult{}, err
			}
			p.recordRefund(fmt.Sprintf("%s_%d", req.RefundCode, time.Now().UnixNano()))
			return connector.RefundResult{ProviderRef: "acme_" + req.RefundCode, Status: connector.StatusPending}, nil
		}
		return f
	}))
	got := rules(r)
	for _, want := range []string{"ID-4", "ID-4/grpc"} {
		if !slices.Contains(got, want) {
			t.Fatalf("Check không bắt được %s; findings:%s", want, dump(r))
		}
	}
	for _, g := range got {
		if g != "ID-4" && g != "ID-4/grpc" && g != "RFD-4" && g != "RFD-4/grpc" {
			t.Errorf("luật %s bắn nhầm (chỉ chờ ID-4, cho phép RFD-4):%s", g, dump(r))
		}
	}
}

// noRefundRecorder giấu RefundRecorder của đối tác giả (giữ RequestRecorder).
type noRefundRecorder struct{ *fakePartner }

func (noRefundRecorder) RefundCodes() {}

// Đối tác giả có RequestRecorder mà không có RefundRecorder: lượt mã hoàn của ID-4 vào Skipped kèm lý do.
func TestCheck_ID4_RefundSkippedWithoutRefundRecorder(t *testing.T) {
	t.Parallel()
	h := harness(nil)
	h.New = func(tb testing.TB) (connector.Connector, conformance.Partner) {
		f, p := newGood(tb)
		return f, noRefundRecorder{p}
	}
	r := conformance.Check(t, h)
	if len(r.Findings) != 0 {
		t.Fatalf("không được có finding:%s", dump(r))
	}
	for _, code := range []string{"ID-4", "ID-4/grpc"} {
		if !slices.ContainsFunc(r.Skipped, func(s string) bool { return strings.HasPrefix(s, code+": ") && strings.Contains(s, "RefundRecorder") }) {
			t.Errorf("thiếu %s trong Skipped với lý do nêu RefundRecorder: %v", code, r.Skipped)
		}
	}
}

// Mã không mang ngày nhưng có sẵn chuỗi 010101 (hằng số của connector): không phải ngày 0001-01-01 đoán ra, ID-5
// không được bắn (dò theo vị trí ngày trong mã của lệnh tạo có CreatedAt, không theo chuỗi con bất kỳ).
func TestCheck_ID5_ConstantLookingLikeZeroDateIsFine(t *testing.T) {
	t.Parallel()
	r := conformance.Check(t, harness(func(f *connectortest.Fake, p *fakePartner) connector.Connector {
		createRecording(f, p, false, func(req connector.CreatePaymentRequest) string { return "M010101_" + req.TransactionCode })
		return f
	}))
	if len(r.Findings) != 0 {
		t.Fatalf("mã không mang ngày không được có finding:%s", dump(r))
	}
}

// Ngày ở cuối mã (mã giao dịch + yyyymmdd): CreatedAt zero thành 00010101 ở đúng vị trí đó, ID-5 bắt.
func TestCheck_Catches_ZeroCreatedAtDateSuffix(t *testing.T) {
	t.Parallel()
	r := conformance.Check(t, harness(func(f *connectortest.Fake, p *fakePartner) connector.Connector {
		createRecording(f, p, false, func(req connector.CreatePaymentRequest) string {
			return req.TransactionCode + "-" + req.CreatedAt.UTC().Format("20060102")
		})
		return f
	}))
	expectBoth(t, r, "ID-5")
}

// payCode là mã canh của CHG-2 (conformance dùng đúng chuỗi này khi không có Harness.ChargeCode).
const payCode = "CFPAYCODE7Q4X9K2M8ZT3"

// codeSeq là Harness.ChargeCode trả "PCODE-0001", "PCODE-0002"... mỗi lần gọi một mã mới.
func codeSeq() func(conformance.Partner) string {
	var n atomic.Int64
	return func(conformance.Partner) string { return fmt.Sprintf("PCODE-%04d", n.Add(1)) }
}

// Có hook thì mọi ChargeRequest bộ luật dựng mang mã của hook, mỗi request một lần gọi (mã dùng một lần).
func TestCheck_ChargeCodeHookSuppliesEveryCharge(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var got []string
	h := harness(func(f *connectortest.Fake, _ *fakePartner) connector.Connector {
		good := f.OnCharge
		f.OnCharge = func(ctx context.Context, req connector.ChargeRequest) (connector.ChargeResult, error) {
			mu.Lock()
			got = append(got, req.PaymentCode)
			mu.Unlock()
			return good(ctx, req)
		}
		return f
	})
	h.ChargeCode = codeSeq()
	r := conformance.Check(t, h)
	if len(r.Findings) != 0 {
		t.Fatalf("không được có finding:%s", dump(r))
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) == 0 {
		t.Fatal("không có lời Charge nào")
	}
	seen := map[string]bool{}
	for i, c := range got {
		if c == "" {
			t.Errorf("Charge thứ %d không mang mã dù có Harness.ChargeCode", i+1)
		}
		if seen[c] {
			t.Errorf("Charge thứ %d dùng lại mã đã dùng: hook phải được gọi một lần cho mỗi request", i+1)
		}
		seen[c] = true
	}
}

// codeRequired là connector cần mã: rỗng thì từ chối tại chỗ, không gọi đối tác.
func codeRequired(f *connectortest.Fake, _ *fakePartner) connector.Connector {
	good := f.OnCharge
	f.OnCharge = func(ctx context.Context, req connector.ChargeRequest) (connector.ChargeResult, error) {
		if req.PaymentCode == "" {
			return connector.ChargeResult{}, &connector.RejectedError{Code: "payment_code_required"}
		}
		return good(ctx, req)
	}
	return f
}

// Connector cần mã: không có hook thì CHG-1 hỏng (ModeDown đòi unavailable mà connector từ chối tại chỗ); có hook thì qua.
func TestCheck_CodeRequiringConnector_NeedsHookForCHG1(t *testing.T) {
	t.Parallel()
	r := conformance.Check(t, harness(codeRequired))
	if !slices.Contains(rules(r), "CHG-1") {
		t.Errorf("không có hook thì connector cần mã phải hỏng CHG-1; findings:%s", dump(r))
	}
	h := harness(codeRequired)
	h.ChargeCode = codeSeq()
	r = conformance.Check(t, h)
	if len(r.Findings) != 0 {
		t.Errorf("có hook thì connector cần mã phải qua; findings:%s", dump(r))
	}
}

// Không có hook: ngoài CHG-2 mọi Charge mang mã rỗng (connector hiện có thấy đúng request như ở v0.1.0); CHG-2 mang mã canh.
func TestCheck_NoHook_CodeEmptyOutsideCHG2(t *testing.T) {
	t.Parallel()
	type seen struct{ tx, code string }
	var mu sync.Mutex
	var got []seen
	r := conformance.Check(t, harness(func(f *connectortest.Fake, _ *fakePartner) connector.Connector {
		good := f.OnCharge
		f.OnCharge = func(ctx context.Context, req connector.ChargeRequest) (connector.ChargeResult, error) {
			mu.Lock()
			got = append(got, seen{req.TransactionCode, req.PaymentCode})
			mu.Unlock()
			return good(ctx, req)
		}
		return f
	}))
	if len(r.Findings) != 0 {
		t.Fatalf("không được có finding:%s", dump(r))
	}
	mu.Lock()
	defer mu.Unlock()
	var chg2 int
	for _, s := range got {
		if strings.HasPrefix(s.tx, "CFCHG2") {
			chg2++
			if s.code != payCode {
				t.Errorf("Charge %s của CHG-2 không mang mã canh", s.tx)
			}
		} else if s.code != "" {
			t.Errorf("Charge %s ngoài CHG-2 mang mã thanh toán khi không có hook", s.tx)
		}
	}
	if chg2 == 0 {
		t.Error("CHG-2 không gọi Charge")
	}
}

// codeLeak là connector tốt nhắc lại mã ở một chỗ.
type codeLeak func(f *connectortest.Fake, p *fakePartner)

func onCharge(change func(p *fakePartner, req connector.ChargeRequest, res connector.ChargeResult, err error) (connector.ChargeResult, error)) codeLeak {
	return func(f *connectortest.Fake, p *fakePartner) {
		good := f.OnCharge
		f.OnCharge = func(ctx context.Context, req connector.ChargeRequest) (connector.ChargeResult, error) {
			res, err := good(ctx, req)
			if req.PaymentCode == "" {
				return res, err
			}
			return change(p, req, res, err)
		}
	}
}

var codeLeaks = map[string]codeLeak{
	"Raw.RequestBody": onCharge(func(_ *fakePartner, req connector.ChargeRequest, res connector.ChargeResult, err error) (connector.ChargeResult, error) {
		if err == nil {
			res.Raw.RequestBody = `{"order":"` + req.TransactionCode + `","code":"` + req.PaymentCode + `"}`
		}
		return res, err
	}),
	"Raw.ResponseBody URL-encode": onCharge(func(_ *fakePartner, req connector.ChargeRequest, res connector.ChargeResult, err error) (connector.ChargeResult, error) {
		if err == nil {
			res.Raw.ResponseBody = "status=00&order=" + req.TransactionCode + "&code=" + url.QueryEscape(strings.ToLower(req.PaymentCode)) + "&x=1"
		}
		return res, err
	}),
	"Raw kèm lỗi ở ModeReject": onCharge(func(p *fakePartner, req connector.ChargeRequest, res connector.ChargeResult, err error) (connector.ChargeResult, error) {
		if p.currentMode() == conformance.ModeReject {
			res.Raw.ResponseBody = `{"error":"R01","code":"` + req.PaymentCode + `"}`
		}
		return res, err
	}),
	"RejectedError.Message": onCharge(func(p *fakePartner, req connector.ChargeRequest, res connector.ChargeResult, err error) (connector.ChargeResult, error) {
		if p.currentMode() == conformance.ModeReject {
			return res, &connector.RejectedError{Code: "R01", Message: "mã " + req.PaymentCode + " không hợp lệ"}
		}
		return res, err
	}),
	"câu lỗi ModeGarbage": onCharge(func(p *fakePartner, req connector.ChargeRequest, res connector.ChargeResult, err error) (connector.ChargeResult, error) {
		if p.currentMode() == conformance.ModeGarbage {
			return res, fmt.Errorf("%w: không đọc được phản hồi cho mã %s", connector.ErrUnknownOutcome, req.PaymentCode)
		}
		return res, err
	}),
	"ProviderRef": onCharge(func(_ *fakePartner, req connector.ChargeRequest, res connector.ChargeResult, err error) (connector.ChargeResult, error) {
		if err == nil {
			res.ProviderRef = "acme_" + req.PaymentCode
		}
		return res, err
	}),
	"NextAction.URL": onCharge(func(_ *fakePartner, req connector.ChargeRequest, res connector.ChargeResult, err error) (connector.ChargeResult, error) {
		if err == nil {
			res.NextAction = paykit.NextAction{Kind: paykit.NextActionRedirect, URL: "https://pay.acme.example/c?code=" + req.PaymentCode}
		}
		return res, err
	}),
	"NextAction.FormFields": onCharge(func(_ *fakePartner, req connector.ChargeRequest, res connector.ChargeResult, err error) (connector.ChargeResult, error) {
		if err == nil {
			res.NextAction = paykit.NextAction{Kind: paykit.NextActionRedirect, URL: "https://pay.acme.example/c",
				FormFields: map[string]string{"order": req.TransactionCode, "auth": req.PaymentCode}}
		}
		return res, err
	}),
	"Raw của QueryPayment sau đó": func(f *connectortest.Fake, _ *fakePartner) {
		var mu sync.Mutex
		codes := map[string]string{}
		goodCharge, goodQuery := f.OnCharge, f.OnQuery
		f.OnCharge = func(ctx context.Context, req connector.ChargeRequest) (connector.ChargeResult, error) {
			mu.Lock()
			codes[req.TransactionCode] = req.PaymentCode
			mu.Unlock()
			return goodCharge(ctx, req)
		}
		f.OnQuery = func(ctx context.Context, req connector.QueryPaymentRequest) (connector.PaymentStatus, error) {
			st, err := goodQuery(ctx, req)
			mu.Lock()
			c := codes[req.TransactionCode]
			mu.Unlock()
			if err == nil && c != "" {
				st.Raw.ResponseBody = `{"order":"` + req.TransactionCode + `","paid_with":"` + c + `"}`
			}
			return st, err
		}
	},
	"câu lỗi ModeDown (URL có mã)": onCharge(func(p *fakePartner, req connector.ChargeRequest, res connector.ChargeResult, err error) (connector.ChargeResult, error) {
		if p.currentMode() == conformance.ModeDown {
			return res, fmt.Errorf("%w: Get \"https://pay.acme.example/c?code=%s\": dial tcp: connection refused", connector.ErrProviderUnavailable, req.PaymentCode)
		}
		return res, err
	}),
	"câu lỗi ModeHang (URL có mã)": onCharge(func(p *fakePartner, req connector.ChargeRequest, res connector.ChargeResult, err error) (connector.ChargeResult, error) {
		if p.currentMode() == conformance.ModeHang {
			return res, fmt.Errorf("%w: Get \"https://pay.acme.example/c?code=%s\": context deadline exceeded", connector.ErrUnknownOutcome, req.PaymentCode)
		}
		return res, err
	}),
	"base64 lệch một byte trong Raw.RequestBody": onCharge(func(_ *fakePartner, req connector.ChargeRequest, res connector.ChargeResult, err error) (connector.ChargeResult, error) {
		if err == nil {
			res.Raw.RequestBody = `{"payload":"` + base64.StdEncoding.EncodeToString([]byte("x"+req.PaymentCode+"y")) + `"}`
		}
		return res, err
	}),
	"base64url không đệm trong Raw.ResponseBody": onCharge(func(_ *fakePartner, req connector.ChargeRequest, res connector.ChargeResult, err error) (connector.ChargeResult, error) {
		if err == nil {
			res.Raw.ResponseBody = "token=" + base64.RawURLEncoding.EncodeToString([]byte(req.PaymentCode))
		}
		return res, err
	}),
	"hex thường trong ProviderRef": onCharge(func(_ *fakePartner, req connector.ChargeRequest, res connector.ChargeResult, err error) (connector.ChargeResult, error) {
		if err == nil {
			res.ProviderRef = "acme_" + hex.EncodeToString([]byte(req.PaymentCode))
		}
		return res, err
	}),
	"hex hoa trong câu lỗi ModeReject": onCharge(func(p *fakePartner, req connector.ChargeRequest, res connector.ChargeResult, err error) (connector.ChargeResult, error) {
		if p.currentMode() == conformance.ModeReject {
			return res, &connector.RejectedError{Code: "R01", Message: "sai " + strings.ToUpper(hex.EncodeToString([]byte(req.PaymentCode)))}
		}
		return res, err
	}),
	"log của connector": onCharge(func(p *fakePartner, req connector.ChargeRequest, res connector.ChargeResult, err error) (connector.ChargeResult, error) {
		p.logf("charge %s code=%s", req.TransactionCode, req.PaymentCode)
		return res, err
	}),
}

// Mỗi chỗ connector có thể để lộ mã thanh toán: CHG-2 bắt được, cả thẳng lẫn qua gRPC.
func TestCheck_Catches_PaymentCodeLeak(t *testing.T) {
	t.Parallel()
	for name, leak := range codeLeaks {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := conformance.Check(t, harness(func(f *connectortest.Fake, p *fakePartner) connector.Connector {
				leak(f, p)
				return f
			}))
			expectBoth(t, r, "CHG-2")
		})
	}
}

// Finding của CHG-2 nêu chỗ lộ, không bao giờ in mã.
func TestCheck_CHG2_FindingNeverPrintsCode(t *testing.T) {
	t.Parallel()
	r := conformance.Check(t, harness(func(f *connectortest.Fake, p *fakePartner) connector.Connector {
		codeLeaks["Raw.RequestBody"](f, p)
		return f
	}))
	var n int
	for _, f := range r.Findings {
		if strings.Contains(f.Detail, payCode) {
			t.Errorf("finding %s in mã thanh toán", f.Rule)
		}
		if strings.HasPrefix(f.Rule, "CHG-2") {
			n++
			if !strings.Contains(f.Detail, "RawExchange.RequestBody") {
				t.Errorf("finding %s không nêu chỗ lộ (RawExchange.RequestBody)", f.Rule)
			}
		}
	}
	if n == 0 {
		t.Fatalf("CHG-2 không bắn; findings:%s", dump(r))
	}
}

// Có hook, connector nhắc lại mã trong một panic hay một lỗi sai loại (dính liền chữ số khác): finding (của luật bất
// kỳ) không in mã — kể cả mã toàn chữ số (redact coi 8–15 chữ số là điện thoại, không cắt giữa nhóm số) và mã ngắn.
func TestCheck_HookCodeNeverInAnyFinding(t *testing.T) {
	t.Parallel()
	numeric := func() func(conformance.Partner) string {
		var n atomic.Int64
		return func(conformance.Partner) string { return fmt.Sprintf("2812345%05d", n.Add(1)) } // 12 chữ số
	}
	short := func() func(conformance.Partner) string {
		var n atomic.Int64
		return func(conformance.Partner) string { return fmt.Sprintf("Q%02d", n.Add(1)%100) } // 3 ký tự
	}
	for name, hook := range map[string]func() func(conformance.Partner) string{"chữ và số": codeSeq, "12 chữ số": numeric, "3 ký tự": short} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var mu sync.Mutex
			var used []string
			h := harness(func(f *connectortest.Fake, p *fakePartner) connector.Connector {
				f.OnCharge = func(_ context.Context, req connector.ChargeRequest) (connector.ChargeResult, error) {
					mu.Lock()
					used = append(used, req.PaymentCode)
					mu.Unlock()
					if p.currentMode() == conformance.ModeNormal {
						panic("boom ref=99" + req.PaymentCode + "77")
					}
					return connector.ChargeResult{}, errors.New("lỗi trơn ref=99" + req.PaymentCode + "77")
				}
				return f
			})
			h.ChargeCode = hook()
			r := conformance.Check(t, h)
			if !slices.Contains(rules(r), "CHG-1") {
				t.Fatalf("connector hỏng phải có finding CHG-1; findings:%s", dump(r))
			}
			mu.Lock()
			defer mu.Unlock()
			texts := append([]string(nil), r.Skipped...)
			texts = append(texts, r.NotChecked...)
			for _, f := range r.Findings {
				texts = append(texts, f.Rule+": "+f.Detail)
			}
			for i, x := range texts {
				for _, c := range used {
					if c != "" && strings.Contains(x, c) {
						t.Errorf("mục thứ %d của báo cáo in mã thanh toán", i) // không in chính mục đó
						break
					}
				}
			}
		})
	}
}

// Charge lỗi ở ModeNormal (đối tác giả từ chối mã canh): lượt thành công và lượt truy vấn của CHG-2 không kiểm được
// gì; luật vào Skipped kèm lý do, không xanh im lặng.
func TestCheck_CHG2_ChargeErrorAtNormal_Skipped(t *testing.T) {
	t.Parallel()
	r := conformance.Check(t, harness(func(f *connectortest.Fake, _ *fakePartner) connector.Connector {
		good := f.OnCharge
		f.OnCharge = func(ctx context.Context, req connector.ChargeRequest) (connector.ChargeResult, error) {
			if req.PaymentCode != "" {
				return connector.ChargeResult{}, &connector.RejectedError{Code: "invalid_code"}
			}
			return good(ctx, req)
		}
		return f
	}))
	if len(r.Findings) != 0 {
		t.Fatalf("không được có finding:%s", dump(r))
	}
	for _, code := range []string{"CHG-2", "CHG-2/grpc"} {
		if !slices.ContainsFunc(r.Skipped, func(s string) bool {
			return strings.HasPrefix(s, code+": ") && strings.Contains(s, "ModeNormal")
		}) {
			t.Errorf("thiếu %s (nêu Charge lỗi ở ModeNormal) trong Skipped: %v", code, r.Skipped)
		}
	}
}

// noLogReporter giấu LogReporter của đối tác giả.
type noLogReporter struct{ *fakePartner }

func (noLogReporter) ConnectorLogs() {}

// Đối tác giả không cài LogReporter: phần log của CHG-2 vào Skipped kèm lý do, không phải vi phạm.
func TestCheck_CHG2_NoLogReporter_Skipped(t *testing.T) {
	t.Parallel()
	h := harness(nil)
	h.New = func(tb testing.TB) (connector.Connector, conformance.Partner) {
		f, p := newGood(tb)
		return f, noLogReporter{p}
	}
	r := conformance.Check(t, h)
	if len(r.Findings) != 0 {
		t.Fatalf("không được có finding:%s", dump(r))
	}
	for _, code := range []string{"CHG-2", "CHG-2/grpc"} {
		if !slices.ContainsFunc(r.Skipped, func(s string) bool {
			return strings.HasPrefix(s, code+": ") && strings.Contains(s, "LogReporter")
		}) {
			t.Errorf("thiếu %s (nêu LogReporter) trong Skipped: %v", code, r.Skipped)
		}
	}
}
