package connector

import (
	"context"
	"time"

	"bitbucket.org/hasaki-tech/payment-connector-sdk/paykit"
)

// Connector là phần bắt buộc; mọi năng lực khác là interface tuỳ chọn bên dưới.
type Connector interface {
	Name() string
	Methods() []paykit.Method
	CreatePayment(ctx context.Context, req CreatePaymentRequest) (CreatePaymentResult, error)
}

// PaymentQuerier ứng với OpQueryPayment.
type PaymentQuerier interface {
	QueryPayment(ctx context.Context, req QueryPaymentRequest) (PaymentStatus, error)
}

// WebhookReceiver ứng với OpWebhook.
type WebhookReceiver interface {
	ParseWebhook(ctx context.Context, raw RawWebhook) (ProviderEvent, error)
	AckWebhook(outcome WebhookOutcome) (status int, body []byte, contentType string)
}

// Redirecter ứng với OpRedirect.
type Redirecter interface {
	ParseRedirect(ctx context.Context, raw RawWebhook) (ProviderEvent, error)
}

// Charger ứng với OpCharge.
type Charger interface {
	Charge(ctx context.Context, req ChargeRequest) (ChargeResult, error)
}

// Canceler ứng với OpCancel.
//
// Core gọi huỷ cả với giao dịch mà Create chưa nghe được trả lời (ProviderRef rỗng, đối tác có thể không
// biết nó). Khi biết chắc không có gì để huỷ, hoặc không thể huỷ bằng thông tin được cho (ví dụ thiếu mã
// tham chiếu mà đối tác chỉ huỷ theo mã đó), CancelPayment phải trả lời DỨT KHOÁT: *RejectedError với mã do
// connector đặt (ví dụ "cannot_cancel"); core sẽ truy vấn lại rồi mới quyết. KHÔNG trả ErrProviderUnavailable
// hay ErrUnknownOutcome trong trường hợp đó: core hiểu là "không rõ" và không bao giờ cho giao dịch hết hạn.
// Unavailable/Unknown chỉ dành cho khi đã cố gọi đối tác mà không tới hoặc không rõ kết quả (luật CXL-3).
// Từ chối như vậy là quyết định tại chỗ; đã gọi đối tác mà không nghe được trả lời thì là ErrUnknownOutcome,
// không bao giờ RejectedError (luật CXL-2).
//
// Đối tác trả lời "đã thu rồi, không huỷ được" thì trả CancelResult{Status: StatusSucceeded} (hoặc StatusOnHold)
// với err nil, KHÔNG trả RejectedError: với câu trả lời "đã thu" core không bao giờ cho hết hạn ở lượt đó, còn
// với lời từ chối core truy vấn lại và quyết theo truy vấn, có thể còn chậm (luật CXL-4). Lần truy vấn
// gần nhất thấy đối tác vẫn giữ thanh toán chưa kết thúc thì core không cho hết hạn; core chỉ cho hết hạn khi
// đối tác nhận huỷ hoặc mọi lần truy vấn của lượt đều không thấy giao dịch. Đối tác không phân biệt được
// "đã thu" khi huỷ thì truy vấn xác nhận rồi mới trả lời, hoặc trả ErrUnknownOutcome.
type Canceler interface {
	CancelPayment(ctx context.Context, req CancelRequest) (CancelResult, error)
}

// Refunder ứng với OpRefund.
type Refunder interface {
	CreateRefund(ctx context.Context, req RefundRequest) (RefundResult, error)
}

// RefundQuerier ứng với OpQueryRefund.
type RefundQuerier interface {
	QueryRefund(ctx context.Context, req QueryRefundRequest) (RefundStatus, error)
}

// RawExchange là bản ghi một lần gọi đối tác, để lưu vết.
//
// Hợp đồng (luật LOG-1): RequestBody và ResponseBody connector trả về KHÔNG được chứa dữ liệu cá nhân của
// khách (tên, email, điện thoại, địa chỉ) ở bất kỳ dạng nào (thô, JSON-escape, URL-encode) cũng như dữ liệu
// thẻ: che bằng providerhttp.Redact(body, các giá trị khách của request) trước khi trả. Đối tác nhắc lại
// khách trong phản hồi của lệnh không mang khách (truy vấn, huỷ) thì che bằng giá trị đọc từ chính phản hồi.
// Core che lại lần nữa để phòng thủ, nhưng chỉ với giá trị nó biết; câu lỗi cũng không được chứa dữ liệu khách.
type RawExchange struct {
	RequestBody, ResponseBody string
	HTTPStatus                int
	DurationMS                int64
}

// RawWebhook là request đến nguyên bản, chưa kiểm chữ ký.
type RawWebhook struct {
	Headers map[string][]string
	// Query là bản đã phân tích để tiện đọc; có thể thiếu cặp không hợp lệ (như "x=%zz" hay cặp ngăn bằng ";")
	// và đã giải mã (+ thành dấu cách). Khi chữ ký tính trên query thô, dùng RawQuery.
	Query map[string][]string
	// RawQuery là query nguyên văn như trên đường dây, không có dấu "?".
	RawQuery string
	Body     []byte
	RemoteIP string
}

// Giá trị đối tác trả về (trong các *Result, PaymentStatus, RefundStatus, ProviderEvent và RejectedError)
// được core chuẩn hoá trước khi lưu, nên connector chuyển nguyên giá trị đối tác gửi, không tự cắt:
//   - ProviderRef: UTF-8 hỏng thay bằng U+FFFD, bỏ ký tự điều khiển, cắt còn 255 ký tự.
//   - EventID: giữ nguyên nếu là UTF-8 hợp lệ, không có ký tự điều khiển và dài tối đa 255 ký tự; nếu
//     không, core dùng "sha256:" + hex(sha256(byte gốc)) làm khoá khử trùng (vẫn ổn định qua các lần gửi lại).
//   - AmountPaid.Currency: khác rỗng mà không đúng ba chữ A–Z thì thành "XXX"; vì khác tiền tệ của giao
//     dịch, giao dịch sang on_hold (lệch tiền) chứ không bị ghi lỗi. Số tiền giữ nguyên.
//   - ProviderCode, ProviderMessage: UTF-8 hợp lệ, bỏ ký tự điều khiển; core cắt còn 64 và 500 ký tự.
//   - NextAction và Card: UTF-8 hỏng thay bằng U+FFFD, không giới hạn độ dài.

type CreatePaymentRequest struct {
	TransactionCode, IdempotencyKey string
	Money                           paykit.Money
	Method                          paykit.Method
	Customer                        paykit.Customer
	ReturnURL, CancelURL, ClientIP  string
	Metadata                        map[string]string
}

type CreatePaymentResult struct {
	ProviderRef string
	Status      Status
	NextAction  paykit.NextAction
	ExpiresAt   time.Time
	Raw         RawExchange
}

type QueryPaymentRequest struct {
	TransactionCode, ProviderRef string
}

type PaymentStatus struct {
	Found       bool
	Status      Status
	ProviderRef string
	// AmountPaid.Amount == 0 nghĩa là đối tác không báo số tiền; core coi là đã thu đủ.
	AmountPaid                    paykit.Money
	PaidAt                        time.Time
	Card                          *paykit.CardMasked
	ProviderCode, ProviderMessage string
	Raw                           RawExchange
}

type EventKind string

const (
	EventKindPayment EventKind = "payment"
	EventKindRefund  EventKind = "refund"
)

// ProviderEvent là sự kiện đã chuẩn hoá từ webhook hoặc redirect.
type ProviderEvent struct {
	// EventID bắt buộc và phải giống hệt nhau qua mọi lần đối tác gửi lại cùng một sự kiện: core khử
	// trùng theo (đối tác, EventID). Quá 255 ký tự hoặc không lưu nguyên được thì core băm (xem ở trên).
	EventID                     string
	Kind                        EventKind
	TransactionCode, RefundCode string
	ProviderRef                 string
	Status                      Status
	// AmountPaid.Amount == 0 nghĩa là đối tác không báo số tiền; core coi là đã thu đủ.
	AmountPaid                    paykit.Money
	PaidAt                        time.Time
	Card                          *paykit.CardMasked
	ProviderCode, ProviderMessage string
	Raw                           []byte
}

// WebhookOutcome là kết quả xử lý phía core, để connector dựng phản hồi đúng định dạng đối tác.
type WebhookOutcome string

const (
	OutcomeApplied   WebhookOutcome = "applied"
	OutcomeDuplicate WebhookOutcome = "duplicate"
	OutcomeIgnored   WebhookOutcome = "ignored"
	OutcomeRejected  WebhookOutcome = "rejected"
)

type ChargeRequest struct {
	TransactionCode, IdempotencyKey string
	Money                           paykit.Money
	Method                          paykit.Method
	Customer                        paykit.Customer
	ReturnURL, CancelURL, ClientIP  string
	Metadata                        map[string]string
}

type ChargeResult struct {
	ProviderRef string
	Status      Status
	// AmountPaid.Amount == 0 nghĩa là đối tác không báo số tiền; core coi là đã thu đủ.
	AmountPaid paykit.Money
	PaidAt     time.Time
	NextAction paykit.NextAction
	Raw        RawExchange
}

type CancelRequest struct {
	TransactionCode, ProviderRef, IdempotencyKey string
}

type CancelResult struct {
	// Status phải là StatusCanceled thì lần huỷ mới được tính; trạng thái khác là chưa huỷ.
	Status Status
	Raw    RawExchange
}

type RefundRequest struct {
	RefundCode, IdempotencyKey, TransactionCode, ProviderRef string
	Money                                                    paykit.Money
	Reason, CustomerID                                       string
}

type RefundResult struct {
	ProviderRef string
	Status      Status
	Raw         RawExchange
}

type QueryRefundRequest struct {
	RefundCode, ProviderRef, TransactionCode string
}

type RefundStatus struct {
	Found                                      bool
	Status                                     Status
	ProviderRef, ProviderCode, ProviderMessage string
	Raw                                        RawExchange
}
