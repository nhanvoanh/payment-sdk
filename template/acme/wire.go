package acme

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"bitbucket.org/hasaki-tech/payment-connector-sdk/connector"
	"bitbucket.org/hasaki-tech/payment-connector-sdk/paykit"
)

// File này chỉ có hình dạng dữ liệu trên đường dây của acme và việc đổi qua lại với kiểu của SDK.
// Không có quyết định về lỗi ở đây ngoài việc báo "thân không hợp lệ"; người gọi (connector.go) biến
// nó thành ErrUnknownOutcome.

const (
	pathToken    = "/oauth/token"
	pathPayments = "/v1/payments"
	pathRefunds  = "/v1/refunds"
)

type tokenRequest struct {
	MerchantID string `json:"merchant_id"`
	Secret     string `json:"secret"`
}

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"` // giây
}

type customerJSON struct {
	ID    string `json:"id,omitempty"`
	Name  string `json:"name,omitempty"`
	Email string `json:"email,omitempty"`
	Phone string `json:"phone,omitempty"`
	// Address: acme dùng địa chỉ để chống gian lận; một chuỗi, có thể nhiều dòng.
	Address string `json:"address,omitempty"`
}

type paymentRequest struct {
	OrderCode string       `json:"order_code"`
	Amount    int64        `json:"amount"`
	Currency  string       `json:"currency"`
	ReturnURL string       `json:"return_url,omitempty"`
	CancelURL string       `json:"cancel_url,omitempty"`
	ClientIP  string       `json:"client_ip,omitempty"`
	Customer  customerJSON `json:"customer"`
}

// paymentJSON là hình dạng thanh toán của acme, dùng cho tạo, truy vấn, huỷ và webhook.
type paymentJSON struct {
	PaymentID   string `json:"payment_id"`
	OrderCode   string `json:"order_code"`
	Status      string `json:"status"`
	RedirectURL string `json:"redirect_url"`
	ExpiresAt   string `json:"expires_at"`
	AmountPaid  int64  `json:"amount_paid"`
	Currency    string `json:"currency"`
	PaidAt      string `json:"paid_at"`
	// Customer: acme nhắc lại khách trong phản hồi; chỉ đọc để che khỏi vết (redactRaw), không dùng vào việc gì khác.
	Customer customerJSON `json:"customer"`
}

type refundRequest struct {
	RefundCode string `json:"refund_code"`
	PaymentID  string `json:"payment_id"`
	Amount     int64  `json:"amount"`
	Currency   string `json:"currency"`
	Reason     string `json:"reason,omitempty"`
}

type refundJSON struct {
	RefundID string `json:"refund_id"`
	Status   string `json:"status"`
}

// webhookJSON là thân webhook; Type quyết định có liên quan tới connector này hay không.
type webhookJSON struct {
	EventID string `json:"event_id"`
	Type    string `json:"type"`
	paymentJSON
}

// errorBody là thân lỗi 4xx: {"error":{"code":"...","message":"..."}}.
type errorBody struct {
	Error struct{ Code, Message string } `json:"error"`
}

// errorCode trả mã lỗi của acme trong thân; ok sai nếu thân không phải thân lỗi có mã.
func errorCode(body []byte) (code, message string, ok bool) {
	var e errorBody
	if json.Unmarshal(body, &e) != nil || e.Error.Code == "" {
		return "", "", false
	}
	return e.Error.Code, e.Error.Message, true
}

// values là các giá trị của khách cần che khỏi vết (hợp đồng của connector.RawExchange, luật LOG-1).
func (c customerJSON) values() []string { return []string{c.Name, c.Email, c.Phone, c.Address} }

// orderCode là mã đơn gửi acme: yymmdd (ngày TẠO giao dịch ở múi giờ loc) + "_" + mã giao dịch của core. Chỉ
// dựng từ TransactionCode và CreatedAt mà core cấp (giống nhau ở mọi lần gọi lại), KHÔNG BAO GIỜ từ time.Now():
// truy vấn một lệnh tạo chưa rõ kết quả phải dựng lại đúng mã đã gửi (luật ID-4). CreatedAt zero (core cũ) vẫn
// tất định, chỉ là ngày vô nghĩa.
func orderCode(tx string, createdAt time.Time, loc *time.Location) string {
	if loc == nil {
		loc = time.UTC
	}
	return createdAt.In(loc).Format("060102") + "_" + tx
}

// transactionCodeOf tách mã giao dịch của core khỏi mã đơn của acme (bỏ "yymmdd_"); ok sai nếu sai dạng.
func transactionCodeOf(order string) (string, bool) {
	date, tx, found := strings.Cut(order, "_")
	if !found || len(date) != 6 || tx == "" {
		return "", false
	}
	for i := 0; i < len(date); i++ {
		if date[i] < '0' || date[i] > '9' {
			return "", false
		}
	}
	return tx, true
}

func newPaymentRequest(req connector.CreatePaymentRequest, loc *time.Location) paymentRequest {
	return paymentRequest{
		OrderCode: orderCode(req.TransactionCode, req.CreatedAt, loc), Amount: req.Money.Amount, Currency: req.Money.Currency,
		ReturnURL: req.ReturnURL, CancelURL: req.CancelURL, ClientIP: req.ClientIP,
		Customer: customerJSON{ID: req.Customer.ID, Name: req.Customer.Name, Email: req.Customer.Email, Phone: req.Customer.Phone,
			Address: req.Customer.Address},
	}
}

// status đổi trạng thái thanh toán của acme sang từ vựng chung. Trạng thái lạ là lỗi: không đoán,
// vì đoán sai "failed" cho một khoản đã trả là khách trả hai lần.
func (p paymentJSON) status() (connector.Status, error) {
	switch p.Status {
	case "pending":
		return connector.StatusPending, nil
	case "paid":
		return connector.StatusSucceeded, nil
	case "failed":
		return connector.StatusFailed, nil
	case "canceled":
		return connector.StatusCanceled, nil
	}
	return "", fmt.Errorf("trạng thái thanh toán lạ %q", p.Status)
}

// status của lệnh hoàn: từ vựng khác của thanh toán ("refunded" chứ không phải "paid").
func (r refundJSON) status() (connector.Status, error) {
	switch r.Status {
	case "pending":
		return connector.StatusPending, nil
	case "refunded":
		return connector.StatusSucceeded, nil
	case "failed":
		return connector.StatusFailed, nil
	}
	return "", fmt.Errorf("trạng thái hoàn tiền lạ %q", r.Status)
}

// validate kiểm thân thanh toán đủ dùng được: có mã, trạng thái hiểu được, thời điểm đọc được.
func (p paymentJSON) validate() error {
	if p.PaymentID == "" {
		return fmt.Errorf("thiếu payment_id")
	}
	if _, err := p.status(); err != nil {
		return err
	}
	if _, err := parseTime("paid_at", p.PaidAt); err != nil {
		return err
	}
	_, err := parseTime("expires_at", p.ExpiresAt)
	return err
}

func (p paymentJSON) paid() paykit.Money {
	return paykit.Money{Amount: p.AmountPaid, Currency: p.Currency}
}

// nextAction: thanh toán đang chờ phải có URL để khách đi tiếp; thiếu thì khách không trả được.
func (p paymentJSON) nextAction() (paykit.NextAction, error) {
	if p.RedirectURL == "" {
		if p.Status == "pending" {
			return paykit.NextAction{}, fmt.Errorf("thanh toán pending nhưng thiếu redirect_url")
		}
		return paykit.NextAction{Kind: paykit.NextActionNone}, nil
	}
	u, err := url.Parse(p.RedirectURL)
	if err != nil || !u.IsAbs() || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return paykit.NextAction{}, fmt.Errorf("redirect_url không phải URL tuyệt đối http(s)")
	}
	return paykit.NextAction{Kind: paykit.NextActionRedirect, URL: p.RedirectURL}, nil
}

// parseTime đọc RFC 3339; rỗng là zero. field là tên trường, chỉ để câu lỗi dễ đọc.
func parseTime(field, s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("thời điểm không hợp lệ ở %s", field)
	}
	return t, nil
}

// forCreate gom những gì CreatePayment cần từ thân và kiểm chúng cùng lúc.
func (p paymentJSON) forCreate() (connector.Status, paykit.NextAction, time.Time, error) {
	if err := p.validate(); err != nil {
		return "", paykit.NextAction{}, time.Time{}, err
	}
	st, _ := p.status()
	na, err := p.nextAction()
	if err != nil {
		return "", paykit.NextAction{}, time.Time{}, err
	}
	expires, _ := parseTime("expires_at", p.ExpiresAt)
	return st, na, expires, nil
}
