// Package acme là connector mẫu cho đối tác tưởng tượng "acme". Sao chép nó làm điểm xuất phát; phần
// đáng đọc kỹ nhất là các chỗ chọn lỗi trả về (send, reply, tokenError), vì đó là chỗ tiền bị đúng hay sai.
//
// Luật tiền, gọn: ErrProviderUnavailable nghĩa là CHẮC CHẮN acme chưa xử lý; ErrProviderRejected nghĩa
// là acme nói rõ từ chối; mọi thứ còn lại (timeout, 5xx, thân không hiểu được...) là ErrUnknownOutcome vì
// acme có thể đã trừ tiền. Xem README.md ở thư mục template cho bảng đầy đủ.
package acme

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"bitbucket.org/hasaki-tech/payment-connector-sdk/connector"
	"bitbucket.org/hasaki-tech/payment-connector-sdk/paykit"
	"bitbucket.org/hasaki-tech/payment-connector-sdk/providerhttp"
	"bitbucket.org/hasaki-tech/payment-connector-sdk/signing"
	"bitbucket.org/hasaki-tech/payment-connector-sdk/tokencache"
)

// Name là khoá cấu hình của core và là một phần khoá khử trùng webhook: không bao giờ đổi.
const Name = "acme"

const tokenTTLFallback = 5 * time.Minute

// Connector cài Connector và mọi interface tuỳ chọn trừ Charger và Redirecter (acme không có hai thao tác đó).
type Connector struct {
	base   *url.URL
	cfg    Config
	http   *providerhttp.Client
	tokens *tokencache.Cache
}

var (
	_ connector.Connector       = (*Connector)(nil)
	_ connector.PaymentQuerier  = (*Connector)(nil)
	_ connector.WebhookReceiver = (*Connector)(nil)
	_ connector.Canceler        = (*Connector)(nil)
	_ connector.Refunder        = (*Connector)(nil)
	_ connector.RefundQuerier   = (*Connector)(nil)
)

// New kiểm cấu hình và dựng connector. Không gọi mạng.
func New(cfg Config) (*Connector, error) {
	base, err := cfg.validate()
	if err != nil {
		return nil, err
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultTimeout
	}
	hc, err := providerhttp.New(providerhttp.Config{Timeout: cfg.Timeout, MaxConns: cfg.MaxConns})
	if err != nil {
		return nil, err
	}
	c := &Connector{base: base, cfg: cfg, http: hc}
	c.tokens = tokencache.New(c.fetchToken, tokencache.Options{FetchTimeout: cfg.Timeout})
	return c, nil
}

func (c *Connector) Name() string             { return Name }
func (c *Connector) Methods() []paykit.Method { return []paykit.Method{paykit.MethodWallet} }

// CreatePayment tạo thanh toán và trả URL để khách đi trả tiền.
func (c *Connector) CreatePayment(ctx context.Context, req connector.CreatePaymentRequest) (connector.CreatePaymentResult, error) {
	resp, err := c.send(ctx, http.MethodPost, pathPayments, nil, req.IdempotencyKey, newPaymentRequest(req))
	if err != nil {
		return connector.CreatePaymentResult{}, err
	}
	var w paymentJSON
	if err := reply(resp, &w, rejectCreate); err != nil {
		return connector.CreatePaymentResult{}, err
	}
	st, na, expires, err := w.forCreate()
	if err == nil && w.OrderCode != req.TransactionCode {
		// Phản hồi nói về giao dịch khác: tin nó là gán nhầm tiền của khách khác.
		err = fmt.Errorf("order_code %q không khớp mã đã gửi", w.OrderCode)
	}
	if err != nil {
		return connector.CreatePaymentResult{}, badBody(err)
	}
	raw := redactRaw(resp.Raw, append(newPaymentRequest(req).Customer.values(), w.Customer.values()...)...)
	return connector.CreatePaymentResult{ProviderRef: w.PaymentID, Status: st, NextAction: na, ExpiresAt: expires, Raw: raw}, nil
}

// QueryPayment tra theo mã giao dịch của ta. Acme không biết mã đó là chuyện bình thường (core truy vấn cả
// giao dịch chưa từng tới được acme), nên 404 not_found là Found=false chứ không phải lỗi: nếu là lỗi thì
// core không bao giờ đóng được giao dịch đó.
func (c *Connector) QueryPayment(ctx context.Context, req connector.QueryPaymentRequest) (connector.PaymentStatus, error) {
	resp, err := c.send(ctx, http.MethodGet, pathPayments, url.Values{"order_code": {req.TransactionCode}}, "", nil)
	if err != nil {
		return connector.PaymentStatus{}, err
	}
	if isNotFound(resp) {
		return connector.PaymentStatus{Found: false, Raw: resp.Raw}, nil
	}
	var w paymentJSON
	if err := reply(resp, &w, nil); err != nil {
		return connector.PaymentStatus{}, err
	}
	if err := w.validate(); err != nil {
		return connector.PaymentStatus{}, badBody(err)
	}
	st, _ := w.status() // đã kiểm ở validate
	paidAt, _ := parseTime("paid_at", w.PaidAt)
	return connector.PaymentStatus{Found: true, Status: st, ProviderRef: w.PaymentID, AmountPaid: w.paid(), PaidAt: paidAt,
		Raw: redactRaw(resp.Raw, w.Customer.values()...)}, nil
}

// CancelPayment huỷ thanh toán chưa trả. Acme từ chối thanh toán đã trả bằng 409 already_paid: đó là câu
// trả lời "đã thu", nên trả Status succeeded với err nil (core không bao giờ cho hết hạn ở lượt đó), không
// phải RejectedError. Ta không bao giờ tự báo "canceled" nếu acme không nói vậy (core sẽ huỷ đơn của khách đã trả tiền).
//
// Acme chỉ huỷ theo payment_id. Thiếu ProviderRef (Create chưa nghe được trả lời) thì biết chắc không huỷ được
// bằng thông tin được cho: trả lời dứt khoát cannot_cancel, không gọi mạng. Không trả ErrProviderUnavailable:
// core hiểu đó là "không rõ" và không bao giờ cho giao dịch hết hạn; với Rejected, core truy vấn lại rồi mới quyết.
func (c *Connector) CancelPayment(ctx context.Context, req connector.CancelRequest) (connector.CancelResult, error) {
	if req.ProviderRef == "" {
		return connector.CancelResult{}, &connector.RejectedError{Code: "cannot_cancel", Message: "thiếu ProviderRef, acme chỉ huỷ theo payment_id"}
	}
	resp, err := c.send(ctx, http.MethodPost, pathPayments+"/"+url.PathEscape(req.ProviderRef)+"/cancel", nil, req.IdempotencyKey, struct{}{})
	if err != nil {
		return connector.CancelResult{}, err
	}
	if code, _, ok := errorCode(resp.Body); ok && code == "already_paid" && resp.Status >= 400 && resp.Status < 500 {
		return connector.CancelResult{Status: connector.StatusSucceeded, Raw: resp.Raw}, nil
	}
	var w paymentJSON
	if err := reply(resp, &w, rejectCancel); err != nil {
		return connector.CancelResult{}, err
	}
	if err := w.validate(); err != nil {
		return connector.CancelResult{}, badBody(err)
	}
	st, _ := w.status()
	return connector.CancelResult{Status: st, Raw: redactRaw(resp.Raw, w.Customer.values()...)}, nil
}

// CreateRefund tạo lệnh hoàn. Cùng luật lỗi như CreatePayment: tiền đi ra cũng không được đoán.
func (c *Connector) CreateRefund(ctx context.Context, req connector.RefundRequest) (connector.RefundResult, error) {
	if req.ProviderRef == "" {
		return connector.RefundResult{}, notSent("thiếu ProviderRef để hoàn tiền")
	}
	body := refundRequest{RefundCode: req.RefundCode, PaymentID: req.ProviderRef, Amount: req.Money.Amount, Currency: req.Money.Currency, Reason: req.Reason}
	resp, err := c.send(ctx, http.MethodPost, pathRefunds, nil, req.IdempotencyKey, body)
	if err != nil {
		return connector.RefundResult{}, err
	}
	var w refundJSON
	if err := reply(resp, &w, rejectRefund); err != nil {
		return connector.RefundResult{}, err
	}
	st, err := w.status()
	if err != nil || w.RefundID == "" {
		return connector.RefundResult{}, badBody(fmt.Errorf("thân hoàn tiền thiếu refund_id hoặc trạng thái lạ"))
	}
	return connector.RefundResult{ProviderRef: w.RefundID, Status: st, Raw: resp.Raw}, nil
}

// QueryRefund: như QueryPayment, 404 not_found là Found=false.
func (c *Connector) QueryRefund(ctx context.Context, req connector.QueryRefundRequest) (connector.RefundStatus, error) {
	resp, err := c.send(ctx, http.MethodGet, pathRefunds, url.Values{"refund_code": {req.RefundCode}}, "", nil)
	if err != nil {
		return connector.RefundStatus{}, err
	}
	if isNotFound(resp) {
		return connector.RefundStatus{Found: false, Raw: resp.Raw}, nil
	}
	var w refundJSON
	if err := reply(resp, &w, nil); err != nil {
		return connector.RefundStatus{}, err
	}
	st, err := w.status()
	if err != nil || w.RefundID == "" {
		return connector.RefundStatus{}, badBody(fmt.Errorf("thân hoàn tiền thiếu refund_id hoặc trạng thái lạ"))
	}
	return connector.RefundStatus{Found: true, Status: st, ProviderRef: w.RefundID, Raw: resp.Raw}, nil
}

// ParseWebhook kiểm chữ ký trên byte thô TRƯỚC khi đọc bất cứ trường nào: thân chưa xác thực là dữ liệu
// của kẻ lạ. Lỗi ở đây không đi vào đường tiền nên là lỗi thường, không phải sentinel của lời gọi đối tác.
func (c *Connector) ParseWebhook(_ context.Context, raw connector.RawWebhook) (connector.ProviderEvent, error) {
	if !c.validSignature(raw) {
		return connector.ProviderEvent{}, connector.ErrInvalidSignature
	}
	var w webhookJSON
	if err := json.Unmarshal(raw.Body, &w); err != nil {
		return connector.ProviderEvent{}, fmt.Errorf("acme: thân webhook không đọc được: %w", err)
	}
	if w.Type != "payment.updated" {
		return connector.ProviderEvent{}, connector.ErrEventNotRelevant
	}
	// EventID là khoá khử trùng của core: lấy nguyên event_id của acme (ổn định qua mọi lần gửi lại,
	// khác nhau giữa các sự kiện), không tự sinh và không dùng giờ nhận.
	if w.EventID == "" || w.OrderCode == "" {
		return connector.ProviderEvent{}, fmt.Errorf("acme: webhook thiếu event_id hoặc order_code")
	}
	if err := w.validate(); err != nil {
		return connector.ProviderEvent{}, fmt.Errorf("acme: webhook không hợp lệ: %w", err)
	}
	st, _ := w.status()
	paidAt, _ := parseTime("paid_at", w.PaidAt)
	return connector.ProviderEvent{
		EventID: w.EventID, Kind: connector.EventKindPayment, TransactionCode: w.OrderCode, ProviderRef: w.PaymentID,
		Status: st, AmountPaid: w.paid(), PaidAt: paidAt, Raw: raw.Body,
	}, nil
}

// validSignature: X-Acme-Signature là HMAC-SHA256 hex của toàn bộ thân. Tên header không phân biệt hoa thường.
func (c *Connector) validSignature(raw connector.RawWebhook) bool {
	// Đúng một giá trị trên mọi biến thể hoa/thường của tên header; nhiều hơn là mơ hồ, và chọn một
	// giá trị theo thứ tự duyệt map sẽ cho kết quả không tất định.
	var sigs []string
	for k, v := range raw.Headers {
		if strings.EqualFold(k, "X-Acme-Signature") {
			sigs = append(sigs, v...)
		}
	}
	if len(sigs) != 1 {
		return false
	}
	// signing.EqualHex: hex hỏng hay rỗng là sai; so sánh thời gian không đổi.
	return signing.EqualHex(signing.HMACSHA256([]byte(c.cfg.SecretKey), raw.Body), sigs[0])
}

// AckWebhook là hàm thuần: core hỏi một lần lúc nạp connector. Acme coi 2xx là "đã nhận, đừng gửi lại";
// 4xx là "bỏ cuộc, đừng gửi lại"; 5xx hoặc không trả lời là "gửi lại". Duplicate và ignored cũng là 200: gửi lại
// cũng không thay đổi gì nên đừng để acme gửi mãi.
func (c *Connector) AckWebhook(outcome connector.WebhookOutcome) (int, []byte, string) {
	const ct = "application/json"
	if outcome == connector.OutcomeRejected {
		return http.StatusBadRequest, []byte(`{"received":false}`), ct
	}
	return http.StatusOK, []byte(`{"received":true}`), ct
}

// redactRaw che dữ liệu khách khỏi vết trước khi trả cho core (hợp đồng của connector.RawExchange, luật LOG-1):
// giá trị khách của request, và của phản hồi khi acme nhắc lại khách (lệnh truy vấn không mang khách nên chỉ
// biết giá trị từ phản hồi). Phản hồi không đọc được (lỗi) thì không trả Raw nào.
func redactRaw(raw connector.RawExchange, values ...string) connector.RawExchange {
	raw.RequestBody = providerhttp.Redact(raw.RequestBody, values...)
	raw.ResponseBody = providerhttp.Redact(raw.ResponseBody, values...)
	return raw
}

// ---- Gọi acme và phân loại lỗi ----

// fetchToken là một lời gọi riêng, chưa có tác dụng phụ về tiền.
func (c *Connector) fetchToken(ctx context.Context) (string, time.Time, error) {
	body, err := json.Marshal(tokenRequest{MerchantID: c.cfg.MerchantID, Secret: c.cfg.SecretKey})
	if err != nil {
		return "", time.Time{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url(pathToken, nil), bytes.NewReader(body))
	if err != nil {
		return "", time.Time{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", time.Time{}, err
	}
	var t tokenResponse
	if resp.Status != http.StatusOK || json.Unmarshal(resp.Body, &t) != nil || t.AccessToken == "" {
		return "", time.Time{}, fmt.Errorf("acme: lấy token thất bại (HTTP %d)", resp.Status)
	}
	ttl := time.Duration(t.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = tokenTTLFallback
	}
	return t.AccessToken, time.Now().Add(ttl), nil
}

// tokenError: lấy token là lời gọi riêng và chưa mang thông tin về tiền; lỗi ở đây (kể cả timeout, 5xx,
// thân hỏng) nghĩa là request nghiệp vụ CHƯA từng được gửi, hoặc (lượt lấy token sau một lần 401) đã gửi
// nhưng acme từ chối trước khi xử lý: chắc chắn acme chưa xử lý gì, đúng là ErrProviderUnavailable. Nguyên nhân chỉ được ghi bằng %v, không %w: nếu bọc cả ErrUnknownOutcome của
// providerhttp thì lỗi khớp hai sentinel và core (xét Unknown trước) chốt sai chiều.
func tokenError(cause error) error {
	return fmt.Errorf("%w: không lấy được token: %v", connector.ErrProviderUnavailable, cause)
}

// send gửi một request nghiệp vụ có token; body (nếu có) được mã hoá JSON.
//
// 401 là lời từ chối của acme TRƯỚC khi xử lý (token hết hạn hay bị thu hồi), nên làm mới token rồi
// gửi lại ĐÚNG MỘT lần là an toàn: lần đầu chưa có tác dụng gì. Gửi cùng Idempotency-Key. Hai lần
// 401 liên tiếp: vẫn là từ chối trước khi xử lý theo giao thức acme nên chưa có gì xảy ra, báo
// ErrProviderUnavailable. Nếu đối tác của bạn không bảo đảm điều đó cho 401, đổi thành ErrUnknownOutcome.
func (c *Connector) send(ctx context.Context, method, path string, q url.Values, idemKey string, body any) (*providerhttp.Response, error) {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return nil, notSent(err.Error())
		}
	}
	for attempt := 0; ; attempt++ {
		tok, err := c.tokens.Get(ctx)
		if err != nil {
			return nil, tokenError(err)
		}
		resp, err := c.once(ctx, method, c.url(path, q), tok, idemKey, payload)
		if err != nil {
			// Lỗi vận chuyển đã được providerhttp phân loại đúng (chưa gửi: unavailable; đã gửi: unknown);
			// chỉ thêm ngữ cảnh bằng %w, không đổi sentinel.
			return nil, fmt.Errorf("acme %s %s: %w", method, path, err)
		}
		if resp.Status != http.StatusUnauthorized {
			return resp, nil
		}
		c.tokens.Invalidate(tok)
		if attempt == 1 {
			// Chỉ đúng khi TÀI LIỆU của đối tác bảo đảm 401 là từ chối trước khi xử lý; không được
			// giả định. Không có bảo đảm đó thì trả ErrUnknownOutcome.
			return nil, notSent("acme từ chối thông tin xác thực hai lần (401)")
		}
	}
}

func (c *Connector) once(ctx context.Context, method, u, token, idemKey string, payload []byte) (*providerhttp.Response, error) {
	var rd io.Reader
	if payload != nil {
		rd = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", connector.ErrProviderUnavailable, err) // request không dựng được: chưa gửi gì
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	return c.http.Do(req)
}

func (c *Connector) url(path string, q url.Values) string {
	u := *c.base
	u.Path = strings.TrimRight(u.Path, "/") + path
	u.RawPath = ""
	if q != nil {
		u.RawQuery = q.Encode()
	}
	return u.String()
}

// Mã từ chối THEO TỪNG THAO TÁC. Cùng một mã có thể là từ chối cuối cùng ở thao tác này và là "tiền đã
// đi" ở thao tác khác, nên không có tập chung. Chỉ thêm mã khi tài liệu của đối tác nói rõ nó là từ chối
// cuối cùng, chắc chắn không có tiền nào đổi chủ, cho ĐÚNG thao tác đó. Mã ngoài tập: ErrUnknownOutcome.
var (
	// Tạo: declined (acme từ chối thu) và invalid_request (request sai, chưa tạo gì) là cuối cùng.
	// already_paid KHÔNG ở đây: lệnh tạo trước bị thất lạc phản hồi rồi gửi lại thì nó nghĩa là acme ĐÃ thu tiền.
	rejectCreate = map[string]bool{"declined": true, "invalid_request": true}
	// Huỷ: not_found là không có gì để huỷ. already_paid KHÔNG ở đây: nó là "đã thu" (CancelPayment trả
	// succeeded), vì từ chối thì core truy vấn lại và có thể cho hết hạn khi truy vấn còn chậm.
	rejectCancel = map[string]bool{"declined": true, "invalid_request": true, "not_found": true}
	// Hoàn: declined và invalid_request chưa hoàn gì; not_found: giao dịch gốc không tồn tại nên không có gì để hoàn.
	rejectRefund = map[string]bool{"declined": true, "invalid_request": true, "not_found": true}
	// Truy vấn không có "từ chối": nil. not_found là Found=false (isNotFound), mã khác là chưa rõ.
)

// reply đổi phản hồi đã đọc trọn thành nil (và điền out) hoặc lỗi đúng luật tiền:
//   - 2xx với thân hợp lệ: thành công.
//   - 4xx có mã trong rejects (tập của thao tác đang gọi): acme nói rõ từ chối, RejectedError.
//   - mọi thứ khác (5xx, 3xx, 4xx mã ngoài tập hoặc không có mã, 2xx với thân rác):
//     ErrUnknownOutcome. Không bao giờ unavailable: acme đã trả lời nên có thể đã xử lý, ta chỉ không
//     hiểu được. Chốt "thất bại" ở đây là để khách trả tiền hai lần.
func reply(resp *providerhttp.Response, out any, rejects map[string]bool) error {
	switch {
	case resp.Status >= 200 && resp.Status < 300:
		if err := json.Unmarshal(resp.Body, out); err != nil {
			return badBody(err)
		}
		return nil
	case resp.Status >= 400 && resp.Status < 500:
		if code, msg, ok := errorCode(resp.Body); ok && rejects[code] {
			return &connector.RejectedError{Code: code, Message: msg}
		}
	}
	return fmt.Errorf("%w: acme trả HTTP %d không hiểu được hoặc chưa rõ", connector.ErrUnknownOutcome, resp.Status)
}

func isNotFound(resp *providerhttp.Response) bool {
	code, _, ok := errorCode(resp.Body)
	return ok && code == "not_found" // theo mã của acme, bất kể mã HTTP
}

// badBody: acme đã trả lời nhưng ta không hiểu: không rõ kết quả.
func badBody(cause error) error {
	return fmt.Errorf("%w: thân phản hồi của acme không hợp lệ: %v", connector.ErrUnknownOutcome, cause)
}

// notSent: lỗi ta tự phát hiện trước khi gửi bất cứ gì tới acme, nên chắc chắn chưa xử lý.
func notSent(reason string) error {
	return fmt.Errorf("%w: %s", connector.ErrProviderUnavailable, reason)
}
