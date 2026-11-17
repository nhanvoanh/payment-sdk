// Package connectorrpc chuyển đổi giữa struct Go của connector/paykit và message proto, và quy ước
// lỗi trên đường dây. Mọi hàm XxxToProto/XxxFromProto đều thuần, không I/O.
//
// Quy ước chung: giá trị zero Go ↔ trường không đặt; map nil và map rỗng đều ra nil sau một vòng đi-về
// (proto không phân biệt được); chuỗi UTF-8 hỏng được thay bằng U+FFFD để Marshal không thất bại.
package connectorrpc

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"google.golang.org/protobuf/types/known/timestamppb"

	"bitbucket.org/hasaki-tech/payment-connector-sdk/connector"
	"bitbucket.org/hasaki-tech/payment-connector-sdk/connectorpb"
	"bitbucket.org/hasaki-tech/payment-connector-sdk/paykit"
)

// ProtocolVersion là phiên bản contract gRPC; connector báo ở Describe, core từ chối phiên bản khác.
const ProtocolVersion = 1

// WebhookOutcomes là bốn khoá webhook_acks phải có ở Describe khi connector nhận webhook.
var WebhookOutcomes = []connector.WebhookOutcome{
	connector.OutcomeApplied, connector.OutcomeDuplicate, connector.OutcomeIgnored, connector.OutcomeRejected,
}

const (
	// maxRawBody giới hạn mỗi thân trong RawExchange để một phản hồi đối tác khổng lồ không làm
	// tràn thông điệp gRPC; phần bị cắt đánh dấu bằng truncatedSuffix.
	maxRawBody      = 64 << 10
	truncatedSuffix = "…[cắt]"
)

// valid thay byte hỏng bằng U+FFFD, giống cách core chuẩn hoá giá trị đối tác.
func valid(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	return strings.ToValidUTF8(s, "\uFFFD")
}

func truncateBody(s string) string {
	s = valid(s)
	if len(s) <= maxRawBody {
		return s
	}
	cut := maxRawBody
	for cut > 0 && !startsRune(s[cut]) {
		cut-- // lùi về ranh giới ký tự để không cắt đôi một ký tự
	}
	return s[:cut] + truncatedSuffix
}

func startsRune(b byte) bool { return b&0xC0 != 0x80 }

// eventIDToProto: EventID hỏng UTF-8 được băm theo đúng quy tắc của core. Thay bằng U+FFFD sẽ làm
// các EventID hỏng khác nhau trùng nhau và mất sự kiện do khử trùng.
func eventIDToProto(id string) string {
	if utf8.ValidString(id) {
		return id
	}
	sum := sha256.Sum256([]byte(id))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func mapToProto(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[valid(k)] = valid(v)
	}
	return out
}

func mapFromProto(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// TimeToProto: thời gian zero ↔ không đặt (nil).
func TimeToProto(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t)
}

func TimeFromProto(t *timestamppb.Timestamp) time.Time {
	if t == nil {
		return time.Time{}
	}
	return t.AsTime()
}

// StatusToProto: giá trị Go lạ ra STATUS_UNSPECIFIED để bên nhận coi là không rõ kết quả.
func StatusToProto(s connector.Status) connectorpb.Status {
	switch s {
	case connector.StatusPending:
		return connectorpb.Status_STATUS_PENDING
	case connector.StatusSucceeded:
		return connectorpb.Status_STATUS_SUCCEEDED
	case connector.StatusFailed:
		return connectorpb.Status_STATUS_FAILED
	case connector.StatusCanceled:
		return connectorpb.Status_STATUS_CANCELED
	case connector.StatusOnHold:
		return connectorpb.Status_STATUS_ON_HOLD
	}
	return connectorpb.Status_STATUS_UNSPECIFIED
}

// StatusFromProto: không đặt hoặc lạ là lỗi bọc ErrUnknownOutcome, không bao giờ đoán thành failed.
func StatusFromProto(s connectorpb.Status) (connector.Status, error) {
	switch s {
	case connectorpb.Status_STATUS_PENDING:
		return connector.StatusPending, nil
	case connectorpb.Status_STATUS_SUCCEEDED:
		return connector.StatusSucceeded, nil
	case connectorpb.Status_STATUS_FAILED:
		return connector.StatusFailed, nil
	case connectorpb.Status_STATUS_CANCELED:
		return connector.StatusCanceled, nil
	case connectorpb.Status_STATUS_ON_HOLD:
		return connector.StatusOnHold, nil
	}
	return "", fmt.Errorf("%w: trạng thái %d không hiểu được", connector.ErrUnknownOutcome, int32(s))
}

func MoneyToProto(m paykit.Money) *connectorpb.Money {
	return &connectorpb.Money{Amount: m.Amount, Currency: valid(m.Currency)}
}

func MoneyFromProto(m *connectorpb.Money) paykit.Money {
	return paykit.Money{Amount: m.GetAmount(), Currency: m.GetCurrency()}
}

func CustomerToProto(c paykit.Customer) *connectorpb.Customer {
	return &connectorpb.Customer{Id: valid(c.ID), Name: valid(c.Name), Email: valid(c.Email), Phone: valid(c.Phone)}
}

func CustomerFromProto(c *connectorpb.Customer) paykit.Customer {
	return paykit.Customer{ID: c.GetId(), Name: c.GetName(), Email: c.GetEmail(), Phone: c.GetPhone()}
}

// CardToProto: nil ↔ nil.
func CardToProto(c *paykit.CardMasked) *connectorpb.CardMasked {
	if c == nil {
		return nil
	}
	return &connectorpb.CardMasked{Brand: valid(c.Brand), Last4: valid(c.Last4), Holder: valid(c.Holder)}
}

func CardFromProto(c *connectorpb.CardMasked) *paykit.CardMasked {
	if c == nil {
		return nil
	}
	return &paykit.CardMasked{Brand: c.GetBrand(), Last4: c.GetLast4(), Holder: c.GetHolder()}
}

func NextActionToProto(n paykit.NextAction) *connectorpb.NextAction {
	return &connectorpb.NextAction{
		Kind: valid(string(n.Kind)), Url: valid(n.URL), QrData: valid(n.QRData), FormFields: mapToProto(n.FormFields),
	}
}

func NextActionFromProto(n *connectorpb.NextAction) paykit.NextAction {
	return paykit.NextAction{
		Kind: paykit.NextActionKind(n.GetKind()), URL: n.GetUrl(), QRData: n.GetQrData(),
		FormFields: mapFromProto(n.GetFormFields()),
	}
}

// RawExchangeToProto cắt mỗi thân còn 64 KiB (thêm hậu tố …[cắt]) để lưu vết không làm tràn gRPC.
func RawExchangeToProto(r connector.RawExchange) *connectorpb.RawExchange {
	return &connectorpb.RawExchange{
		RequestBody: truncateBody(r.RequestBody), ResponseBody: truncateBody(r.ResponseBody),
		HttpStatus: int32(r.HTTPStatus), DurationMs: r.DurationMS,
	}
}

func RawExchangeFromProto(r *connectorpb.RawExchange) connector.RawExchange {
	return connector.RawExchange{
		RequestBody: r.GetRequestBody(), ResponseBody: r.GetResponseBody(),
		HTTPStatus: int(r.GetHttpStatus()), DurationMS: r.GetDurationMs(),
	}
}

func listsToProto(m map[string][]string) map[string]*connectorpb.StringList {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]*connectorpb.StringList, len(m))
	for k, vs := range m {
		l := &connectorpb.StringList{Values: make([]string, len(vs))}
		for i, v := range vs {
			l.Values[i] = valid(v)
		}
		out[valid(k)] = l
	}
	return out
}

func listsFromProto(m map[string]*connectorpb.StringList) map[string][]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string][]string, len(m))
	for k, l := range m {
		out[k] = append([]string(nil), l.GetValues()...)
	}
	return out
}

func RawWebhookToProto(r connector.RawWebhook) *connectorpb.RawWebhook {
	return &connectorpb.RawWebhook{
		Headers: listsToProto(r.Headers), Query: listsToProto(r.Query), Body: r.Body, RemoteIp: valid(r.RemoteIP),
	}
}

func RawWebhookFromProto(r *connectorpb.RawWebhook) connector.RawWebhook {
	if r == nil {
		return connector.RawWebhook{}
	}
	out := connector.RawWebhook{Headers: listsFromProto(r.Headers), Query: listsFromProto(r.Query), RemoteIP: r.RemoteIp}
	if len(r.Body) > 0 {
		out.Body = r.Body
	}
	return out
}

// ---- Request ----

func CreatePaymentRequestToProto(r connector.CreatePaymentRequest) *connectorpb.CreatePaymentRequest {
	return &connectorpb.CreatePaymentRequest{
		TransactionCode: valid(r.TransactionCode), IdempotencyKey: valid(r.IdempotencyKey),
		Money: MoneyToProto(r.Money), Method: valid(string(r.Method)), Customer: CustomerToProto(r.Customer),
		ReturnUrl: valid(r.ReturnURL), CancelUrl: valid(r.CancelURL), ClientIp: valid(r.ClientIP),
		Metadata: mapToProto(r.Metadata),
	}
}

func CreatePaymentRequestFromProto(r *connectorpb.CreatePaymentRequest) connector.CreatePaymentRequest {
	return connector.CreatePaymentRequest{
		TransactionCode: r.GetTransactionCode(), IdempotencyKey: r.GetIdempotencyKey(),
		Money: MoneyFromProto(r.GetMoney()), Method: paykit.Method(r.GetMethod()),
		Customer:  CustomerFromProto(r.GetCustomer()),
		ReturnURL: r.GetReturnUrl(), CancelURL: r.GetCancelUrl(), ClientIP: r.GetClientIp(),
		Metadata: mapFromProto(r.GetMetadata()),
	}
}

func ChargeRequestToProto(r connector.ChargeRequest) *connectorpb.ChargeRequest {
	return &connectorpb.ChargeRequest{
		TransactionCode: valid(r.TransactionCode), IdempotencyKey: valid(r.IdempotencyKey),
		Money: MoneyToProto(r.Money), Method: valid(string(r.Method)), Customer: CustomerToProto(r.Customer),
		ReturnUrl: valid(r.ReturnURL), CancelUrl: valid(r.CancelURL), ClientIp: valid(r.ClientIP),
		Metadata: mapToProto(r.Metadata),
	}
}

func ChargeRequestFromProto(r *connectorpb.ChargeRequest) connector.ChargeRequest {
	return connector.ChargeRequest{
		TransactionCode: r.GetTransactionCode(), IdempotencyKey: r.GetIdempotencyKey(),
		Money: MoneyFromProto(r.GetMoney()), Method: paykit.Method(r.GetMethod()),
		Customer:  CustomerFromProto(r.GetCustomer()),
		ReturnURL: r.GetReturnUrl(), CancelURL: r.GetCancelUrl(), ClientIP: r.GetClientIp(),
		Metadata: mapFromProto(r.GetMetadata()),
	}
}

func QueryPaymentRequestToProto(r connector.QueryPaymentRequest) *connectorpb.QueryPaymentRequest {
	return &connectorpb.QueryPaymentRequest{TransactionCode: valid(r.TransactionCode), ProviderRef: valid(r.ProviderRef)}
}

func QueryPaymentRequestFromProto(r *connectorpb.QueryPaymentRequest) connector.QueryPaymentRequest {
	return connector.QueryPaymentRequest{TransactionCode: r.GetTransactionCode(), ProviderRef: r.GetProviderRef()}
}

func CancelRequestToProto(r connector.CancelRequest) *connectorpb.CancelPaymentRequest {
	return &connectorpb.CancelPaymentRequest{
		TransactionCode: valid(r.TransactionCode), ProviderRef: valid(r.ProviderRef), IdempotencyKey: valid(r.IdempotencyKey),
	}
}

func CancelRequestFromProto(r *connectorpb.CancelPaymentRequest) connector.CancelRequest {
	return connector.CancelRequest{
		TransactionCode: r.GetTransactionCode(), ProviderRef: r.GetProviderRef(), IdempotencyKey: r.GetIdempotencyKey(),
	}
}

func RefundRequestToProto(r connector.RefundRequest) *connectorpb.CreateRefundRequest {
	return &connectorpb.CreateRefundRequest{
		RefundCode: valid(r.RefundCode), IdempotencyKey: valid(r.IdempotencyKey),
		TransactionCode: valid(r.TransactionCode), ProviderRef: valid(r.ProviderRef),
		Money: MoneyToProto(r.Money), Reason: valid(r.Reason), CustomerId: valid(r.CustomerID),
	}
}

func RefundRequestFromProto(r *connectorpb.CreateRefundRequest) connector.RefundRequest {
	return connector.RefundRequest{
		RefundCode: r.GetRefundCode(), IdempotencyKey: r.GetIdempotencyKey(),
		TransactionCode: r.GetTransactionCode(), ProviderRef: r.GetProviderRef(),
		Money: MoneyFromProto(r.GetMoney()), Reason: r.GetReason(), CustomerID: r.GetCustomerId(),
	}
}

func QueryRefundRequestToProto(r connector.QueryRefundRequest) *connectorpb.QueryRefundRequest {
	return &connectorpb.QueryRefundRequest{
		RefundCode: valid(r.RefundCode), ProviderRef: valid(r.ProviderRef), TransactionCode: valid(r.TransactionCode),
	}
}

func QueryRefundRequestFromProto(r *connectorpb.QueryRefundRequest) connector.QueryRefundRequest {
	return connector.QueryRefundRequest{
		RefundCode: r.GetRefundCode(), ProviderRef: r.GetProviderRef(), TransactionCode: r.GetTransactionCode(),
	}
}

// ---- Kết quả ----
//
// XxxToProto dựng response thành công (error rỗng); phía server tự gán Error khi có lỗi.
// XxxFromProto trả lỗi theo thứ tự: response nil, rồi response.Error, rồi trạng thái không hiểu được.
// Khi có lỗi, Raw vẫn được điền để core lưu vết.

var errNilResponse = fmt.Errorf("%w: connector trả phản hồi rỗng", connector.ErrUnknownOutcome)

func CreatePaymentResultToProto(r connector.CreatePaymentResult) *connectorpb.CreatePaymentResponse {
	return &connectorpb.CreatePaymentResponse{
		Raw: RawExchangeToProto(r.Raw), ProviderRef: valid(r.ProviderRef), Status: StatusToProto(r.Status),
		NextAction: NextActionToProto(r.NextAction), ExpiresAt: TimeToProto(r.ExpiresAt),
	}
}

func CreatePaymentResultFromProto(p *connectorpb.CreatePaymentResponse) (connector.CreatePaymentResult, error) {
	if p == nil {
		return connector.CreatePaymentResult{}, errNilResponse
	}
	res := connector.CreatePaymentResult{Raw: RawExchangeFromProto(p.GetRaw())}
	if err := ErrorFromProto(p.GetError()); err != nil {
		return res, err
	}
	res.ProviderRef = p.GetProviderRef()
	res.NextAction = NextActionFromProto(p.GetNextAction())
	res.ExpiresAt = TimeFromProto(p.GetExpiresAt())
	st, err := StatusFromProto(p.GetStatus())
	res.Status = st
	return res, err
}

func ChargeResultToProto(r connector.ChargeResult) *connectorpb.ChargeResponse {
	return &connectorpb.ChargeResponse{
		Raw: RawExchangeToProto(r.Raw), ProviderRef: valid(r.ProviderRef), Status: StatusToProto(r.Status),
		AmountPaid: MoneyToProto(r.AmountPaid), PaidAt: TimeToProto(r.PaidAt), NextAction: NextActionToProto(r.NextAction),
	}
}

func ChargeResultFromProto(p *connectorpb.ChargeResponse) (connector.ChargeResult, error) {
	if p == nil {
		return connector.ChargeResult{}, errNilResponse
	}
	res := connector.ChargeResult{Raw: RawExchangeFromProto(p.GetRaw())}
	if err := ErrorFromProto(p.GetError()); err != nil {
		return res, err
	}
	res.ProviderRef = p.GetProviderRef()
	res.AmountPaid = MoneyFromProto(p.GetAmountPaid())
	res.PaidAt = TimeFromProto(p.GetPaidAt())
	res.NextAction = NextActionFromProto(p.GetNextAction())
	st, err := StatusFromProto(p.GetStatus())
	res.Status = st
	return res, err
}

func PaymentStatusToProto(s connector.PaymentStatus) *connectorpb.QueryPaymentResponse {
	return &connectorpb.QueryPaymentResponse{
		Raw: RawExchangeToProto(s.Raw), Found: s.Found, Status: StatusToProto(s.Status),
		ProviderRef: valid(s.ProviderRef), AmountPaid: MoneyToProto(s.AmountPaid), PaidAt: TimeToProto(s.PaidAt),
		Card: CardToProto(s.Card), ProviderCode: valid(s.ProviderCode), ProviderMessage: valid(s.ProviderMessage),
	}
}

// PaymentStatusFromProto: found=false thì không xét status (đối tác không biết giao dịch).
func PaymentStatusFromProto(p *connectorpb.QueryPaymentResponse) (connector.PaymentStatus, error) {
	if p == nil {
		return connector.PaymentStatus{}, errNilResponse
	}
	res := connector.PaymentStatus{Raw: RawExchangeFromProto(p.GetRaw())}
	if err := ErrorFromProto(p.GetError()); err != nil {
		return res, err
	}
	if !p.GetFound() {
		return res, nil
	}
	res.Found = true
	res.ProviderRef = p.GetProviderRef()
	res.AmountPaid = MoneyFromProto(p.GetAmountPaid())
	res.PaidAt = TimeFromProto(p.GetPaidAt())
	res.Card = CardFromProto(p.GetCard())
	res.ProviderCode, res.ProviderMessage = p.GetProviderCode(), p.GetProviderMessage()
	st, err := StatusFromProto(p.GetStatus())
	res.Status = st
	return res, err
}

func CancelResultToProto(r connector.CancelResult) *connectorpb.CancelPaymentResponse {
	return &connectorpb.CancelPaymentResponse{Raw: RawExchangeToProto(r.Raw), Status: StatusToProto(r.Status)}
}

func CancelResultFromProto(p *connectorpb.CancelPaymentResponse) (connector.CancelResult, error) {
	if p == nil {
		return connector.CancelResult{}, errNilResponse
	}
	res := connector.CancelResult{Raw: RawExchangeFromProto(p.GetRaw())}
	if err := ErrorFromProto(p.GetError()); err != nil {
		return res, err
	}
	st, err := StatusFromProto(p.GetStatus())
	res.Status = st
	return res, err
}

func RefundResultToProto(r connector.RefundResult) *connectorpb.CreateRefundResponse {
	return &connectorpb.CreateRefundResponse{
		Raw: RawExchangeToProto(r.Raw), ProviderRef: valid(r.ProviderRef), Status: StatusToProto(r.Status),
	}
}

func RefundResultFromProto(p *connectorpb.CreateRefundResponse) (connector.RefundResult, error) {
	if p == nil {
		return connector.RefundResult{}, errNilResponse
	}
	res := connector.RefundResult{Raw: RawExchangeFromProto(p.GetRaw())}
	if err := ErrorFromProto(p.GetError()); err != nil {
		return res, err
	}
	res.ProviderRef = p.GetProviderRef()
	st, err := StatusFromProto(p.GetStatus())
	res.Status = st
	return res, err
}

func RefundStatusToProto(s connector.RefundStatus) *connectorpb.QueryRefundResponse {
	return &connectorpb.QueryRefundResponse{
		Raw: RawExchangeToProto(s.Raw), Found: s.Found, Status: StatusToProto(s.Status),
		ProviderRef: valid(s.ProviderRef), ProviderCode: valid(s.ProviderCode), ProviderMessage: valid(s.ProviderMessage),
	}
}

// RefundStatusFromProto: found=false thì không xét status.
func RefundStatusFromProto(p *connectorpb.QueryRefundResponse) (connector.RefundStatus, error) {
	if p == nil {
		return connector.RefundStatus{}, errNilResponse
	}
	res := connector.RefundStatus{Raw: RawExchangeFromProto(p.GetRaw())}
	if err := ErrorFromProto(p.GetError()); err != nil {
		return res, err
	}
	if !p.GetFound() {
		return res, nil
	}
	res.Found = true
	res.ProviderRef, res.ProviderCode, res.ProviderMessage = p.GetProviderRef(), p.GetProviderCode(), p.GetProviderMessage()
	st, err := StatusFromProto(p.GetStatus())
	res.Status = st
	return res, err
}

// ProviderEventToProto dựng message sự kiện; ParseWebhookResponse/ParseRedirectResponse chứa nó ở trường event.
func ProviderEventToProto(e connector.ProviderEvent) *connectorpb.ProviderEvent {
	return &connectorpb.ProviderEvent{
		EventId: eventIDToProto(e.EventID), Kind: valid(string(e.Kind)),
		TransactionCode: valid(e.TransactionCode), RefundCode: valid(e.RefundCode), ProviderRef: valid(e.ProviderRef),
		Status: StatusToProto(e.Status), AmountPaid: MoneyToProto(e.AmountPaid), PaidAt: TimeToProto(e.PaidAt),
		Card: CardToProto(e.Card), ProviderCode: valid(e.ProviderCode), ProviderMessage: valid(e.ProviderMessage),
		Raw: e.Raw,
	}
}

// ProviderEventFromProto: event nil hoặc status không hiểu được là lỗi bọc ErrUnknownOutcome.
func ProviderEventFromProto(p *connectorpb.ProviderEvent) (connector.ProviderEvent, error) {
	if p == nil {
		return connector.ProviderEvent{}, errNilResponse
	}
	st, err := StatusFromProto(p.GetStatus())
	ev := connector.ProviderEvent{
		EventID: p.GetEventId(), Kind: connector.EventKind(p.GetKind()),
		TransactionCode: p.GetTransactionCode(), RefundCode: p.GetRefundCode(), ProviderRef: p.GetProviderRef(),
		Status: st, AmountPaid: MoneyFromProto(p.GetAmountPaid()), PaidAt: TimeFromProto(p.GetPaidAt()),
		Card: CardFromProto(p.GetCard()), ProviderCode: p.GetProviderCode(), ProviderMessage: p.GetProviderMessage(),
	}
	if len(p.GetRaw()) > 0 {
		ev.Raw = p.GetRaw()
	}
	return ev, err
}
