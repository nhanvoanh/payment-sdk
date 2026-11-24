package connector

import (
	"context"
	"time"

	"github.com/nhanvoanh/payment-sdk/paykit"
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

// RawExchange là bản ghi một lần gọi đối tác, để lưu vết. Trả cả khi lỗi — đây là thứ cần nhất khi tranh chấp:
// mọi nhánh lỗi SAU khi đã gửi (đối tác từ chối, 5xx, thân không đọc được, timeout giữa chừng) đặt Raw vào kết quả
// đi kèm lỗi (*providerhttp.Error.Raw khi lời gọi hỏng giữa chừng), cùng luật che như nhánh thành công. Core lưu vết
// và đi qua gRPC cả khi lỗi.
//
// Hợp đồng (luật LOG-1): RequestBody và ResponseBody connector trả về KHÔNG được chứa dữ liệu cá nhân của
// khách (tên, từng chữ của tên, email, điện thoại, địa chỉ) ở bất kỳ dạng nào (thô, JSON-escape, URL-encode,
// bỏ dấu) cũng như dữ liệu thẻ: che bằng providerhttp.RedactCustomer(body, khách của request) trước khi trả.
// Đối tác nhắc lại khách trong phản hồi của lệnh không mang khách (truy vấn, huỷ, hoàn, truy vấn hoàn) thì che
// bằng khách đọc từ chính phản hồi. Connector tự tách họ/tên hay đổi cách viết thì che cả phần đã tách/đổi.
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

// Thời điểm tạo trong mọi request (CreatedAt, TransactionCreatedAt) là giá trị core ĐÃ LƯU, UTC, độ chính xác
// micro giây, giống hệt nhau ở mọi lần gọi lại cho cùng một giao dịch/lệnh hoàn (kể cả sau khi core khởi động
// lại). Connector sinh mã phía đối tác (ví dụ tiền tố yymmdd) TẤT ĐỊNH từ TransactionCode/RefundCode và các
// thời điểm này, KHÔNG BAO GIỜ từ đồng hồ lúc gọi: truy vấn hay huỷ một lệnh tạo "chưa rõ kết quả" (không có
// ProviderRef) phải dựng lại đúng mã đã gửi, nếu không đối tác trả "không thấy" cho một đơn khách đã trả (luật
// ID-4). Múi giờ của ngày trong mã là việc của connector (đổi từ UTC).
//
// Triển khai CORE TRƯỚC, connector sau. Core cũ không gửi thì giá trị là zero, và connector cần nó KHÔNG BAO GIỜ
// đoán ngày (luật ID-5): tạo/thu trả ErrProviderUnavailable (chưa gửi gì); truy vấn, huỷ, hoàn, truy vấn hoàn trả
// ErrUnknownOutcome, không bao giờ Found=false hay RejectedError. Đổi định dạng mã phía đối tác của connector đang
// chạy thì mã mới "không tìm thấy" phải truy vấn thử mã định dạng cũ trước khi trả Found=false.

type CreatePaymentRequest struct {
	TransactionCode, IdempotencyKey string
	Money                           paykit.Money
	Method                          paykit.Method
	Customer                        paykit.Customer
	ReturnURL, CancelURL, ClientIP  string
	Metadata                        map[string]string
	// CreatedAt là thời điểm tạo giao dịch (xem trên).
	CreatedAt time.Time
}

type CreatePaymentResult struct {
	ProviderRef string
	Status      Status
	NextAction  paykit.NextAction
	// ExpiresAt là hạn của phiên thanh toán PHÍA ĐỐI TÁC (sau thời điểm này khách không trả được nữa), ở UTC
	// (t.UTC(); đối tác ghi giờ địa phương thì đổi đúng múi giờ/offset của nó, đừng coi giờ địa phương là UTC).
	// Zero nghĩa là đối tác không báo: core dùng hạn mặc định của nó. Core chỉ nhận hạn trong
	// (now + 1 phút, now + 7 ngày]; ngoài khoảng core bỏ hạn đó (giữ hạn mặc định), nên connector gặp hạn ngoài
	// khoảng thì trả zero, không tự chế hạn (luật PAY-8). ChargeResult không có trường này.
	ExpiresAt time.Time
	Raw       RawExchange
}

type QueryPaymentRequest struct {
	TransactionCode, ProviderRef string
	// CreatedAt là thời điểm tạo giao dịch, đúng giá trị đã gửi ở CreatePayment/Charge.
	CreatedAt time.Time
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
	// Raw là thân thông báo core lưu vào provider_events để đối soát. Connector nên che dữ liệu khách mà đối tác
	// nhắc lại (luật LOG-1; providerhttp.RedactCustomer) hoặc để rỗng. Core che lại theo khách của giao dịch và
	// lọc số thẻ trước khi lưu; khử trùng theo EventID, không theo byte của Raw.
	Raw []byte
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
	// CreatedAt là thời điểm tạo giao dịch.
	CreatedAt time.Time
	// PaymentCode là mã thanh toán khách đưa ra để quét: chuỗi thô thu ngân quét từ thiết bị của khách, sống ngắn,
	// dùng một lần và mang quyền trừ tiền của khách — coi như một thông tin xác thực.
	//
	// Chỉ Charge mang mã này; QueryPayment, CancelPayment, CreateRefund không bao giờ mang. Một lần thu "chưa rõ"
	// được chốt bằng truy vấn, không bao giờ bằng một lần Charge thứ hai.
	//
	// Rỗng nghĩa là client không gửi mã hoặc core cũ hơn v0.2.0. Connector cần mã mà nhận rỗng thì trả
	// &RejectedError{Code: "payment_code_required"} và không gọi đối tác; KHÔNG trả ErrProviderUnavailable (lỗi
	// đó tính vào bộ ngắt mạch, trong khi đây là lỗi của client).
	//
	// Connector không bao giờ đặt giá trị này vào RawExchange, câu lỗi, ProviderRef, NextAction hay một dòng log,
	// và chỉ gửi cho đối tác ở đúng dạng đối tác yêu cầu (luật CHG-2). Thêm ở v0.2.0.
	PaymentCode string
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
	// CreatedAt là thời điểm tạo giao dịch, đúng giá trị đã gửi ở CreatePayment/Charge.
	CreatedAt time.Time
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
	// TransactionCreatedAt là thời điểm tạo giao dịch gốc (như ở CreatePayment); CreatedAt là thời điểm tạo lệnh hoàn.
	TransactionCreatedAt, CreatedAt time.Time
}

type RefundResult struct {
	ProviderRef string
	Status      Status
	Raw         RawExchange
}

// QueryRefundRequest: ProviderRef có thể RỖNG — core truy vấn cả lệnh hoàn mà nó chưa nghe được kết quả tạo
// (tiến trình chết sau lời gọi, lần ghi kết quả hỏng). Connector chỉ có hai cách đạt:
//   - (a) truy vấn theo mã dựng tất định từ RefundCode + CreatedAt (+ TransactionCreatedAt), đúng mã đã gửi ở
//     CreateRefund (luật ID-4), nên truy vấn được cả khi ProviderRef rỗng (luật RFD-2, RFD-4);
//   - (b) không khai OpQueryRefund: core không truy vấn lệnh hoàn, refund kẹt `new` không bao giờ được tự chốt mà
//     chờ người vận hành xử lý.
//
// Không có lựa chọn thứ ba: trả lỗi cho mọi truy vấn thiếu ProviderRef trượt RFD-2 (lệnh hoàn đối tác không biết
// phải là Found=false, không lỗi), còn Found=false cho lệnh hoàn đối tác đã nhận là hoàn tiền hai lần (RFD-4).
// ErrUnknownOutcome chỉ dành cho lỗi đối tác tạm thời (hết giờ, mất kết nối, trả rác) như mọi thao tác khác.
type QueryRefundRequest struct {
	RefundCode, ProviderRef, TransactionCode string
	// CreatedAt là thời điểm tạo lệnh hoàn, TransactionCreatedAt của giao dịch gốc; đúng giá trị đã gửi ở CreateRefund.
	CreatedAt, TransactionCreatedAt time.Time
}

// RefundStatus: Found=false chỉ khi đối tác CHẮC CHẮN không giữ lệnh hoàn nào dưới mã dựng từ RefundCode + CreatedAt
// (xem QueryRefundRequest). Core tin câu trả lời đó: refund còn `new` (chưa từng nghe được trả lời của lệnh tạo) từ
// 30 phút mà truy vấn trả Found=false bị core chốt failed và NHẢ số tiền đang giữ, nên "không thấy" sai cho một lệnh
// hoàn đối tác đã nhận có thể khiến khách được hoàn hai lần. Đối tác trả lời không chắc (lỗi tạm thời, trả rác) thì
// trả ErrUnknownOutcome cho lần đó; không tìm được theo mã dựng từ RefundCode + CreatedAt thì đừng khai OpQueryRefund.
type RefundStatus struct {
	Found                                      bool
	Status                                     Status
	ProviderRef, ProviderCode, ProviderMessage string
	Raw                                        RawExchange
}
