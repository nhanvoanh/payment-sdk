package connectorrpc

// Cho test ngoài gói tiêm đồng hồ và khoảng kiểm.
var (
	ServerTLSWithClock = serverTLS
	ClientTLSWithClock = clientTLS
)

// SetAfterReady cài hook chạy ngay sau waitReady, trước lần đọc lại cờ cách ly (để dựng đua tất định).
func SetAfterReady(c *Client, f func()) { c.afterReady = f }

// Quarantine cách ly c như khi phát hiện sai danh tính.
func Quarantine(c *Client) { c.quarantine() }
