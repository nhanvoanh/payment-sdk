package paykit

type NextActionKind string

const (
	NextActionNone     NextActionKind = "none"
	NextActionRedirect NextActionKind = "redirect"
	NextActionShowQR   NextActionKind = "show_qr"
)

// NextAction cho client biết bước kế tiếp sau khi tạo thanh toán.
type NextAction struct {
	Kind       NextActionKind
	URL        string
	QRData     string
	FormFields map[string]string
}
