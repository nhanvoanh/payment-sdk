// Package connectortest cung cấp connector giả có thể lập trình, dùng cho test của core.
package connectortest

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"bitbucket.org/hasaki-tech/payment-connector-sdk/connector"
	"bitbucket.org/hasaki-tech/payment-connector-sdk/paykit"
	"bitbucket.org/hasaki-tech/payment-connector-sdk/providerhttp"
)

// Fake cài Connector và mọi interface tuỳ chọn; năng lực bật/tắt bằng Capabilities. An toàn khi gọi đồng thời.
// Hook khác nil thay hoàn toàn hành vi mặc định; lời gọi vẫn được đếm và ghi log trước khi hook chạy.
type Fake struct {
	NameValue    string
	MethodsValue []paykit.Method
	Capabilities map[connector.Operation]bool

	OnCreate       func(ctx context.Context, req connector.CreatePaymentRequest) (connector.CreatePaymentResult, error)
	OnCharge       func(ctx context.Context, req connector.ChargeRequest) (connector.ChargeResult, error)
	OnQuery        func(ctx context.Context, req connector.QueryPaymentRequest) (connector.PaymentStatus, error)
	OnCancel       func(ctx context.Context, req connector.CancelRequest) (connector.CancelResult, error)
	OnRefund       func(ctx context.Context, req connector.RefundRequest) (connector.RefundResult, error)
	OnQueryRefund  func(ctx context.Context, req connector.QueryRefundRequest) (connector.RefundStatus, error)
	OnParseWebhook func(ctx context.Context, raw connector.RawWebhook) (connector.ProviderEvent, error)

	mu  sync.Mutex
	log []string
}

// New tạo Fake với mọi capability bật; mặc định Create trả pending kèm redirect.
func New(name string, methods ...paykit.Method) *Fake {
	return &Fake{
		NameValue:    name,
		MethodsValue: methods,
		Capabilities: map[connector.Operation]bool{
			connector.OpQueryPayment: true,
			connector.OpWebhook:      true,
			connector.OpRedirect:     true,
			connector.OpCharge:       true,
			connector.OpCancel:       true,
			connector.OpRefund:       true,
			connector.OpQueryRefund:  true,
		},
	}
}

// Calls trả số lần gọi của op: create, charge, query, cancel, refund, query_refund, parse_webhook.
func (f *Fake) Calls(op string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, o := range f.log {
		if o == op {
			n++
		}
	}
	return n
}

// CallLog trả bản sao danh sách lời gọi theo thứ tự.
func (f *Fake) CallLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.log...)
}

func (f *Fake) record(op string) {
	f.mu.Lock()
	f.log = append(f.log, op)
	f.mu.Unlock()
}

// enter ghi nhận lời gọi; false nếu capability đang tắt (khi đó không đếm).
func (f *Fake) enter(capab connector.Operation, op string) bool {
	if !f.Supports(capab) {
		return false
	}
	f.record(op)
	return true
}

func (f *Fake) Name() string             { return f.NameValue }
func (f *Fake) Methods() []paykit.Method { return f.MethodsValue }

func (f *Fake) Supports(op connector.Operation) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Capabilities[op]
}

func (f *Fake) CreatePayment(ctx context.Context, req connector.CreatePaymentRequest) (connector.CreatePaymentResult, error) {
	f.record("create")
	if f.OnCreate != nil {
		return f.OnCreate(ctx, req)
	}
	return connector.CreatePaymentResult{
		ProviderRef: "fake_" + req.TransactionCode,
		Status:      connector.StatusPending,
		NextAction:  paykit.NextAction{Kind: paykit.NextActionRedirect, URL: "https://pay.example/" + req.TransactionCode},
		Raw:         requestRaw(req.TransactionCode, req.Money, req.Customer),
	}, nil
}

func (f *Fake) Charge(ctx context.Context, req connector.ChargeRequest) (connector.ChargeResult, error) {
	if !f.enter(connector.OpCharge, "charge") {
		return connector.ChargeResult{}, connector.ErrNotSupported
	}
	if f.OnCharge != nil {
		return f.OnCharge(ctx, req)
	}
	return connector.ChargeResult{
		ProviderRef: "fake_" + req.TransactionCode,
		Status:      connector.StatusSucceeded,
		AmountPaid:  req.Money,
		PaidAt:      time.Now().UTC(),
		Raw:         requestRaw(req.TransactionCode, req.Money, req.Customer),
	}, nil
}

// requestRaw là vết mặc định của lời gọi mang dữ liệu khách: thân như đối tác thật nhận, đã che dữ liệu khách
// đúng hợp đồng của connector.RawExchange (luật LOG-1).
func requestRaw(tx string, m paykit.Money, c paykit.Customer) connector.RawExchange {
	body, _ := json.Marshal(map[string]any{"transaction_code": tx, "amount": m.Amount, "currency": m.Currency,
		"customer": map[string]string{"id": c.ID, "name": c.Name, "email": c.Email, "phone": c.Phone, "address": c.Address}})
	return connector.RawExchange{RequestBody: providerhttp.Redact(string(body), c.Name, c.Email, c.Phone, c.Address), HTTPStatus: 200}
}

func (f *Fake) QueryPayment(ctx context.Context, req connector.QueryPaymentRequest) (connector.PaymentStatus, error) {
	if !f.enter(connector.OpQueryPayment, "query") {
		return connector.PaymentStatus{}, connector.ErrNotSupported
	}
	if f.OnQuery != nil {
		return f.OnQuery(ctx, req)
	}
	return connector.PaymentStatus{Found: false}, nil
}

func (f *Fake) CancelPayment(ctx context.Context, req connector.CancelRequest) (connector.CancelResult, error) {
	if !f.enter(connector.OpCancel, "cancel") {
		return connector.CancelResult{}, connector.ErrNotSupported
	}
	if f.OnCancel != nil {
		return f.OnCancel(ctx, req)
	}
	return connector.CancelResult{Status: connector.StatusCanceled}, nil
}

func (f *Fake) CreateRefund(ctx context.Context, req connector.RefundRequest) (connector.RefundResult, error) {
	if !f.enter(connector.OpRefund, "refund") {
		return connector.RefundResult{}, connector.ErrNotSupported
	}
	if f.OnRefund != nil {
		return f.OnRefund(ctx, req)
	}
	return connector.RefundResult{ProviderRef: "fake_" + req.RefundCode, Status: connector.StatusSucceeded}, nil
}

func (f *Fake) QueryRefund(ctx context.Context, req connector.QueryRefundRequest) (connector.RefundStatus, error) {
	if !f.enter(connector.OpQueryRefund, "query_refund") {
		return connector.RefundStatus{}, connector.ErrNotSupported
	}
	if f.OnQueryRefund != nil {
		return f.OnQueryRefund(ctx, req)
	}
	return connector.RefundStatus{Found: false}, nil
}

func (f *Fake) ParseWebhook(ctx context.Context, raw connector.RawWebhook) (connector.ProviderEvent, error) {
	if !f.enter(connector.OpWebhook, "parse_webhook") {
		return connector.ProviderEvent{}, connector.ErrNotSupported
	}
	return f.parse(ctx, raw)
}

// ParseRedirect dùng chung hook và mặc định với ParseWebhook.
func (f *Fake) ParseRedirect(ctx context.Context, raw connector.RawWebhook) (connector.ProviderEvent, error) {
	if !f.enter(connector.OpRedirect, "parse_webhook") {
		return connector.ProviderEvent{}, connector.ErrNotSupported
	}
	return f.parse(ctx, raw)
}

func (f *Fake) parse(ctx context.Context, raw connector.RawWebhook) (connector.ProviderEvent, error) {
	if f.OnParseWebhook != nil {
		return f.OnParseWebhook(ctx, raw)
	}
	for k, vs := range raw.Headers {
		if !strings.EqualFold(k, "X-Fake-Signature") {
			continue
		}
		for _, v := range vs {
			if v == "bad" {
				return connector.ProviderEvent{}, connector.ErrInvalidSignature
			}
		}
	}
	var ev connector.ProviderEvent
	if err := json.Unmarshal(raw.Body, &ev); err != nil {
		return connector.ProviderEvent{}, err
	}
	return ev, nil
}

func (f *Fake) AckWebhook(connector.WebhookOutcome) (int, []byte, string) {
	return 200, []byte("OK"), "text/plain"
}
