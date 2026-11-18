package connector

// Operation là một năng lực tuỳ chọn của connector, ứng với đúng một interface.
type Operation string

const (
	OpQueryPayment Operation = "query_payment"
	OpWebhook      Operation = "webhook"
	OpRedirect     Operation = "redirect"
	OpCharge       Operation = "charge"
	OpCancel       Operation = "cancel"
	OpRefund       Operation = "refund"
	OpQueryRefund  Operation = "query_refund"
)

// AllOperations trả mọi Operation theo thứ tự cố định (thứ tự Describe khai). Mỗi lần gọi là một bản sao,
// người gọi sửa thoải mái. Thêm hằng Operation thì thêm vào đây (có test).
func AllOperations() []Operation {
	return []Operation{OpQueryPayment, OpWebhook, OpRedirect, OpCharge, OpCancel, OpRefund, OpQueryRefund}
}

// CapabilityReporter cho phép connector thu hẹp năng lực so với các interface nó cài.
type CapabilityReporter interface{ Supports(op Operation) bool }

// Supports: CapabilityReporter có thì quyết định, không thì dựa vào type assertion.
func Supports(c Connector, op Operation) bool {
	if r, ok := c.(CapabilityReporter); ok {
		return r.Supports(op)
	}
	switch op {
	case OpQueryPayment:
		_, ok := c.(PaymentQuerier)
		return ok
	case OpWebhook:
		_, ok := c.(WebhookReceiver)
		return ok
	case OpRedirect:
		_, ok := c.(Redirecter)
		return ok
	case OpCharge:
		_, ok := c.(Charger)
		return ok
	case OpCancel:
		_, ok := c.(Canceler)
		return ok
	case OpRefund:
		_, ok := c.(Refunder)
		return ok
	case OpQueryRefund:
		_, ok := c.(RefundQuerier)
		return ok
	}
	return false
}

// as trả interface T chỉ khi connector cài T và Supports(op) đúng.
func as[T any](c Connector, op Operation) (T, bool) {
	var zero T
	if !Supports(c, op) {
		return zero, false
	}
	v, ok := c.(T)
	if !ok {
		return zero, false
	}
	return v, true
}

func AsPaymentQuerier(c Connector) (PaymentQuerier, bool) {
	return as[PaymentQuerier](c, OpQueryPayment)
}
func AsWebhookReceiver(c Connector) (WebhookReceiver, bool) {
	return as[WebhookReceiver](c, OpWebhook)
}
func AsRedirecter(c Connector) (Redirecter, bool) { return as[Redirecter](c, OpRedirect) }
func AsCharger(c Connector) (Charger, bool)       { return as[Charger](c, OpCharge) }
func AsCanceler(c Connector) (Canceler, bool)     { return as[Canceler](c, OpCancel) }
func AsRefunder(c Connector) (Refunder, bool)     { return as[Refunder](c, OpRefund) }
func AsRefundQuerier(c Connector) (RefundQuerier, bool) {
	return as[RefundQuerier](c, OpQueryRefund)
}
