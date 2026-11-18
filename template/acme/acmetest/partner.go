// Package acmetest là đối tác "acme" giả để test connector: một máy chủ HTTP thật trên 127.0.0.1 nói
// đúng giao thức acme mô tả trong README của template. Nó cài conformance.Partner.
//
// Cố ý KHÔNG import gói acme: đối tác giả là bằng chứng độc lập về giao thức, nếu hai bên dùng chung
// mã thì một hiểu lầm về giao thức sẽ lọt qua test.
package acmetest

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"bitbucket.org/hasaki-tech/payment-connector-sdk/conformance"
	"bitbucket.org/hasaki-tech/payment-connector-sdk/connector"
	"bitbucket.org/hasaki-tech/payment-connector-sdk/paykit"
)

const (
	secret     = "acme-test-secret"
	merchantID = "M-TEST"
)

var (
	_ conformance.Partner  = (*Partner)(nil)
	_ conformance.Tamperer = (*Partner)(nil)

	_ conformance.RequestCounter  = (*Partner)(nil)
	_ conformance.RequestRecorder = (*Partner)(nil)
)

type payment struct {
	ID, OrderCode, Status string
	Paid                  paykit.Money
	Customer              customerJSON // acme nhắc lại khách trong mọi phản hồi thanh toán, như nhiều đối tác thật
}

type customerJSON struct {
	Name    string `json:"name,omitempty"`
	Email   string `json:"email,omitempty"`
	Phone   string `json:"phone,omitempty"`
	Address string `json:"address,omitempty"`
}

// Partner an toàn cho nhiều goroutine. Mọi trạng thái nằm sau mu.
type Partner struct {
	tb   testing.TB
	srv  *http.Server
	addr string
	done chan struct{} // đóng khi test xong để giải phóng các request ModeHang

	mu       sync.Mutex
	lis      net.Listener
	mode     conformance.Mode
	tokens   map[string]bool
	payments map[string]*payment // theo order_code (yymmdd_<mã giao dịch>), như acme thật
	byTx     map[string]*payment // theo mã giao dịch của core (phần sau "yymmdd_"), cho các hàm của test
	byID     map[string]*payment
	orders   []string          // order_code của mọi lệnh tạo hợp lệ đã nhận (conformance.RequestRecorder)
	refunds  map[string]string // refund_code -> status
	seq      int

	rejectAuth, failTokens, fail5xx        bool
	refuseCancel, lagQueries               bool
	tokenFetches, paymentAttempts, created int
	businessRequests                       int
}

// NewPartner khởi động đối tác giả và tự dọn khi test xong.
func NewPartner(tb testing.TB) *Partner {
	tb.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatal(err)
	}
	p := &Partner{
		tb: tb, addr: lis.Addr().String(), lis: lis, done: make(chan struct{}),
		tokens: map[string]bool{}, payments: map[string]*payment{}, byTx: map[string]*payment{}, byID: map[string]*payment{},
		refunds: map[string]string{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /oauth/token", p.handleToken)
	mux.HandleFunc("POST /v1/payments", p.business(p.handleCreate))
	mux.HandleFunc("GET /v1/payments", p.business(p.handleQuery))
	mux.HandleFunc("POST /v1/payments/{id}/cancel", p.business(p.handleCancel))
	mux.HandleFunc("POST /v1/refunds", p.business(p.handleRefund))
	mux.HandleFunc("GET /v1/refunds", p.business(p.handleQueryRefund))
	p.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = p.srv.Serve(lis) }()
	tb.Cleanup(func() {
		close(p.done)
		_ = p.srv.Close()
	})
	return p
}

func (p *Partner) URL() string        { return "http://" + p.addr }
func (p *Partner) Secret() string     { return secret }
func (p *Partner) MerchantID() string { return merchantID }

// Các bộ đếm cho test hỏi đối tác "bạn đã thấy gì".
func (p *Partner) TokenFetches() int    { return p.get(func() int { return p.tokenFetches }) }
func (p *Partner) PaymentAttempts() int { return p.get(func() int { return p.paymentAttempts }) } // kể cả bị 401
func (p *Partner) PaymentsCreated() int { return p.get(func() int { return p.created }) }         // chỉ cái đã xử lý

// CustomerAddress là địa chỉ khách acme nhận ở lệnh tạo của giao dịch tx; rỗng nếu chưa nhận.
func (p *Partner) CustomerAddress(tx string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if pay := p.byTx[tx]; pay != nil {
		return pay.Customer.Address
	}
	return ""
}

// OrderCodes cài conformance.RequestRecorder: order_code của mọi lệnh tạo hợp lệ đã nhận, theo thứ tự.
func (p *Partner) OrderCodes() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.orders...)
}

// txOf tách mã giao dịch khỏi order_code "yymmdd_<mã>" (giao thức acme); ok sai nếu sai dạng.
func txOf(order string) (string, bool) {
	if len(order) < 8 || order[6] != '_' {
		return "", false
	}
	for _, c := range order[:6] {
		if c < '0' || c > '9' {
			return "", false
		}
	}
	return order[7:], true
}

// BusinessRequests cài conformance.RequestCounter: mọi request nghiệp vụ đã tới (kể cả 401 và khi treo), không tính token.
func (p *Partner) BusinessRequests() int { return p.get(func() int { return p.businessRequests }) }

func (p *Partner) get(f func() int) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return f()
}

// RevokeTokens làm mọi token đã phát hết hiệu lực: request kế tiếp nhận 401.
func (p *Partner) RevokeTokens() { p.set(func() { p.tokens = map[string]bool{} }) }

// RejectAuth: mọi request nghiệp vụ nhận 401 dù token nào.
func (p *Partner) RejectAuth(on bool) { p.set(func() { p.rejectAuth = on }) }

// FailTokens: /oauth/token trả 500.
func (p *Partner) FailTokens(on bool) { p.set(func() { p.failTokens = on }) }

// Fail5xx: request nghiệp vụ trả 503 (đối tác không nói gì về việc đã xử lý hay chưa).
func (p *Partner) Fail5xx(on bool) { p.set(func() { p.fail5xx = on }) }

// RefuseCancel bật thì acme từ chối mọi lệnh huỷ bằng 422 declined, kể cả thanh toán còn chờ khách trả.
func (p *Partner) RefuseCancel(on bool) { p.set(func() { p.refuseCancel = on }) }

// LagQueries bật thì truy vấn báo thanh toán đã trả là còn chờ, như chỉ mục tra cứu của acme bị chậm.
func (p *Partner) LagQueries(on bool) { p.set(func() { p.lagQueries = on }) }

func (p *Partner) set(f func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	f()
}

// SetMode đổi cách đối tác cư xử với request nghiệp vụ kế tiếp (conformance.Partner).
// ModeDown đóng listener thật nên kết nối bị từ chối; ra khỏi ModeDown thì nghe lại đúng cổng cũ.
// /oauth/token không bị ModeHang/ModeReject/ModeGarbage ảnh hưởng: các chế độ đó mô tả lỗi của lời gọi nghiệp vụ.
func (p *Partner) SetMode(m conformance.Mode) {
	p.mu.Lock()
	defer p.mu.Unlock()
	was := p.mode
	p.mode = m
	switch {
	case m == conformance.ModeDown && was != conformance.ModeDown:
		_ = p.lis.Close()
	case m != conformance.ModeDown && was == conformance.ModeDown:
		lis, err := net.Listen("tcp", p.addr)
		if err != nil {
			p.tb.Errorf("acmetest: không nghe lại được %s: %v", p.addr, err)
			return
		}
		p.lis = lis
		go func() { _ = p.srv.Serve(lis) }()
	}
}

// SetPaymentStatus đặt câu trả lời cho lần truy vấn kế tiếp của giao dịch (conformance.Partner). Giao dịch acme
// chưa nhận lệnh tạo thì chưa biết order_code: truy vấn theo bất kỳ ngày nào của tx cũng thấy nó.
func (p *Partner) SetPaymentStatus(tx string, st connector.Status, paid paykit.Money) {
	p.set(func() {
		pay := p.byTx[tx]
		if pay == nil {
			pay = p.newPayment(tx, "")
		}
		pay.Status, pay.Paid = acmeStatus(st), paid
	})
}

// SignedWebhook dựng webhook thanh toán hợp lệ đã ký như acme gửi thật (conformance.Partner).
func (p *Partner) SignedWebhook(tx, ref string, st connector.Status, paid paykit.Money) connector.RawWebhook {
	return p.SignedWebhookOfType("payment.updated", tx, ref, st, paid)
}

// TamperedWebhook cài conformance.Tamperer: đổi nội dung được ký (trạng thái và số tiền) nhưng giữ nguyên
// chữ ký, nên WHK-4 chạy ở mức chặt (đòi đúng ErrInvalidSignature).
func (p *Partner) TamperedWebhook(raw connector.RawWebhook) connector.RawWebhook {
	var m map[string]any
	if json.Unmarshal(raw.Body, &m) != nil {
		p.tb.Errorf("acmetest: thân webhook không phải JSON")
		return raw
	}
	m["status"], m["amount_paid"] = "paid", int64(1)
	raw.Body, _ = json.Marshal(m)
	return raw
}

// SignedWebhookOfType như SignedWebhook nhưng chọn loại sự kiện, để test sự kiện không liên quan. order_code là
// mã acme đã nhận lúc tạo; giao dịch chưa từng tạo thì lấy ngày hôm nay (đối tác giả, không phải connector).
func (p *Partner) SignedWebhookOfType(typ, tx, ref string, st connector.Status, paid paykit.Money) connector.RawWebhook {
	order := time.Now().UTC().Format("060102") + "_" + tx
	p.set(func() {
		if pay := p.byTx[tx]; pay != nil && pay.OrderCode != "" {
			order = pay.OrderCode
		}
	})
	body, _ := json.Marshal(map[string]any{
		"event_id": "evt_" + tx + "_" + acmeStatus(st), "type": typ, "order_code": order, "payment_id": ref,
		"status": acmeStatus(st), "amount_paid": paid.Amount, "currency": paid.Currency,
	})
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return connector.RawWebhook{
		Headers: map[string][]string{"X-Acme-Signature": {hex.EncodeToString(mac.Sum(nil))}, "Content-Type": {"application/json"}},
		Body:    body,
	}
}

// acmeStatus: từ vựng trạng thái của acme (khác từ vựng chung của SDK).
func acmeStatus(st connector.Status) string {
	switch st {
	case connector.StatusSucceeded:
		return "paid"
	case connector.StatusFailed:
		return "failed"
	case connector.StatusCanceled:
		return "canceled"
	default:
		return "pending"
	}
}

// newPayment gọi khi đã giữ mu; order rỗng là chưa biết order_code (đặt trạng thái trước khi tạo).
func (p *Partner) newPayment(tx, order string) *payment {
	p.seq++
	pay := &payment{ID: fmt.Sprintf("pay_%d_%s", p.seq, tx), OrderCode: order, Status: "pending"}
	p.byTx[tx], p.byID[pay.ID] = pay, pay
	if order != "" {
		p.payments[order] = pay
	}
	return pay
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": msg}})
}

func (p *Partner) handleToken(w http.ResponseWriter, r *http.Request) {
	// Đóng kết nối sau mỗi phản hồi: client không bao giờ tái dùng kết nối đã chết khi ModeDown,
	// nên "từ chối kết nối" luôn là kết quả của lần quay số mới và test tất định.
	w.Header().Set("Connection", "close")
	var in struct {
		MerchantID string `json:"merchant_id"`
		Secret     string `json:"secret"`
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.tokenFetches++
	if p.failTokens {
		writeError(w, http.StatusInternalServerError, "server_error", "token tạm thời hỏng")
		return
	}
	if json.NewDecoder(r.Body).Decode(&in) != nil || in.MerchantID != merchantID || in.Secret != secret {
		writeError(w, http.StatusUnauthorized, "invalid_client", "sai thông tin")
		return
	}
	tok := fmt.Sprintf("tok_%d", p.tokenFetches)
	p.tokens[tok] = true
	writeJSON(w, http.StatusOK, map[string]any{"access_token": tok, "expires_in": 300, "token_type": "Bearer"})
}

// business bọc mọi endpoint nghiệp vụ: đếm, xác thực (401 là từ chối TRƯỚC khi xử lý), rồi áp ModeXxx.
func (p *Partner) business(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Connection", "close")
		p.mu.Lock()
		p.businessRequests++
		if r.Method == http.MethodPost && r.URL.Path == "/v1/payments" {
			p.paymentAttempts++
		}
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		authed := p.tokens[tok] && !p.rejectAuth
		mode, fail5xx := p.mode, p.fail5xx
		p.mu.Unlock()

		switch {
		case !authed:
			writeError(w, http.StatusUnauthorized, "invalid_token", "token không hợp lệ")
		case mode == conformance.ModeHang:
			// Nhận rồi im tới khi client bỏ đi hoặc test xong.
			select {
			case <-r.Context().Done():
			case <-p.done:
			}
		case mode == conformance.ModeReject:
			writeError(w, http.StatusUnprocessableEntity, "declined", "đối tác từ chối giao dịch")
		case mode == conformance.ModeGarbage:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("<<không phải JSON>>"))
		case fail5xx:
			writeError(w, http.StatusServiceUnavailable, "unavailable", "lỗi tạm thời")
		default:
			next(w, r)
		}
	}
}

type paymentJSON struct {
	PaymentID   string       `json:"payment_id"`
	OrderCode   string       `json:"order_code"`
	Status      string       `json:"status"`
	RedirectURL string       `json:"redirect_url,omitempty"`
	AmountPaid  int64        `json:"amount_paid,omitempty"`
	Currency    string       `json:"currency,omitempty"`
	Customer    customerJSON `json:"customer"`
}

func (pay *payment) json() paymentJSON {
	return paymentJSON{PaymentID: pay.ID, OrderCode: pay.OrderCode, Status: pay.Status, AmountPaid: pay.Paid.Amount,
		Currency: pay.Paid.Currency, Customer: pay.Customer}
}

func (p *Partner) handleCreate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		OrderCode string       `json:"order_code"`
		Amount    int64        `json:"amount"`
		Currency  string       `json:"currency"`
		Customer  customerJSON `json:"customer"`
	}
	if json.NewDecoder(r.Body).Decode(&in) != nil || in.Amount <= 0 {
		writeError(w, http.StatusUnprocessableEntity, "invalid_request", "thiếu order_code hoặc amount")
		return
	}
	tx, ok := txOf(in.OrderCode)
	if !ok {
		writeError(w, http.StatusUnprocessableEntity, "invalid_request", "order_code phải có dạng yymmdd_<mã>")
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.orders = append(p.orders, in.OrderCode)
	pay := p.payments[in.OrderCode] // gửi lại cùng order_code (cùng Idempotency-Key) trả lại thanh toán cũ
	if pay == nil {
		if pre := p.byTx[tx]; pre != nil && pre.OrderCode == "" {
			pay = pre // trạng thái đã được đặt trước khi tạo
			pay.OrderCode = in.OrderCode
			p.payments[in.OrderCode] = pay
		} else {
			pay = p.newPayment(tx, in.OrderCode)
		}
		pay.Customer = in.Customer
		p.created++
	}
	out := pay.json()
	out.RedirectURL = "https://pay.acme.example/checkout/" + pay.ID
	writeJSON(w, http.StatusCreated, out)
}

func (p *Partner) handleQuery(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	order := r.URL.Query().Get("order_code")
	pay := p.payments[order]
	if tx, ok := txOf(order); pay == nil && ok {
		if pre := p.byTx[tx]; pre != nil && pre.OrderCode == "" {
			pay = pre // trạng thái đặt trước khi acme nhận lệnh tạo: chưa gắn ngày nào
		}
	}
	if pay == nil {
		writeError(w, http.StatusNotFound, "not_found", "không có giao dịch này")
		return
	}
	out := pay.json()
	if p.lagQueries && out.Status == "paid" {
		out.Status, out.AmountPaid, out.Currency = "pending", 0, ""
	}
	writeJSON(w, http.StatusOK, out)
}

func (p *Partner) handleCancel(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	pay := p.byID[r.PathValue("id")]
	switch {
	case pay == nil:
		writeError(w, http.StatusNotFound, "not_found", "không có giao dịch này")
	case p.refuseCancel:
		writeError(w, http.StatusUnprocessableEntity, "declined", "acme không cho huỷ giao dịch này")
	case pay.Status == "paid":
		writeError(w, http.StatusConflict, "already_paid", "giao dịch đã thanh toán, không huỷ được")
	default:
		pay.Status = "canceled"
		writeJSON(w, http.StatusOK, pay.json())
	}
}

func (p *Partner) handleRefund(w http.ResponseWriter, r *http.Request) {
	var in struct {
		RefundCode string `json:"refund_code"`
		PaymentID  string `json:"payment_id"`
		Amount     int64  `json:"amount"`
	}
	if json.NewDecoder(r.Body).Decode(&in) != nil || in.RefundCode == "" || in.Amount <= 0 {
		writeError(w, http.StatusUnprocessableEntity, "invalid_request", "thiếu refund_code hoặc amount")
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.byID[in.PaymentID] == nil {
		writeError(w, http.StatusNotFound, "not_found", "không có giao dịch này")
		return
	}
	if _, ok := p.refunds[in.RefundCode]; !ok {
		p.refunds[in.RefundCode] = "pending"
	}
	writeJSON(w, http.StatusCreated, map[string]string{"refund_id": "rf_" + in.RefundCode, "status": p.refunds[in.RefundCode]})
}

func (p *Partner) handleQueryRefund(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("refund_code")
	p.mu.Lock()
	defer p.mu.Unlock()
	st, ok := p.refunds[code]
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "không có lệnh hoàn này")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"refund_id": "rf_" + code, "status": st})
}
