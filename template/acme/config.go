package acme

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"
)

const defaultTimeout = 10 * time.Second

// Config là cấu hình của connector. Giá trị zero của Timeout và MaxConns dùng mặc định.
type Config struct {
	BaseURL    string // địa chỉ API của acme, ví dụ https://api.acme.example
	MerchantID string
	SecretKey  string        // vừa để lấy token vừa để ký webhook
	Timeout    time.Duration // hạn tổng cho một lời gọi tới acme; mặc định 10 giây
	MaxConns   int           // số kết nối tối đa tới acme; mặc định của providerhttp
	// Location là múi giờ của ngày trong mã đơn (yymmdd_<mã giao dịch>); nil là UTC. Đổi sau khi đã chạy thật
	// thì truy vấn các giao dịch cũ dựng sai mã: chỉ đặt một lần.
	Location *time.Location
}

// ConfigFromEnv đọc ACME_BASE_URL, ACME_MERCHANT_ID, ACME_SECRET_KEY (bắt buộc), ACME_TIMEOUT
// (tuỳ chọn, ví dụ "8s") và ACME_TIMEZONE (tuỳ chọn, tên IANA như "Asia/Ho_Chi_Minh"; mặc định UTC; image cần
// dữ liệu múi giờ — distroless/static có sẵn). Thiếu hay sai thì lỗi nêu tên biến, không bao giờ nêu giá trị (có thể là secret).
func ConfigFromEnv(getenv func(string) string) (Config, error) {
	// Cắt khoảng trắng (xuống dòng thừa khi dán secret từ file là lỗi quen thuộc) trước khi kiểm.
	env := func(k string) string { return strings.TrimSpace(getenv(k)) }
	cfg := Config{BaseURL: env("ACME_BASE_URL"), MerchantID: env("ACME_MERCHANT_ID"), SecretKey: env("ACME_SECRET_KEY")}
	var missing []string
	for _, v := range []struct{ name, val string }{
		{"ACME_BASE_URL", cfg.BaseURL}, {"ACME_MERCHANT_ID", cfg.MerchantID}, {"ACME_SECRET_KEY", cfg.SecretKey},
	} {
		if v.val == "" {
			missing = append(missing, v.name)
		}
	}
	if len(missing) > 0 {
		return Config{}, fmt.Errorf("acme: thiếu biến môi trường %s", strings.Join(missing, ", "))
	}
	if s := env("ACME_TIMEOUT"); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil || d <= 0 {
			return Config{}, errors.New("acme: ACME_TIMEOUT phải là khoảng thời gian dương, ví dụ 8s")
		}
		cfg.Timeout = d
	}
	if s := env("ACME_TIMEZONE"); s != "" {
		loc, err := time.LoadLocation(s)
		if err != nil {
			return Config{}, errors.New("acme: ACME_TIMEZONE phải là tên múi giờ IANA, ví dụ Asia/Ho_Chi_Minh")
		}
		cfg.Location = loc
	}
	return cfg, nil
}

// validate kiểm cấu hình và trả URL gốc đã chuẩn hoá. Secret đi qua đường truyền này nên chỉ cho
// https; http chỉ được phép tới máy local để chạy thử với đối tác giả.
func (c Config) validate() (*url.URL, error) {
	if c.MerchantID == "" || c.SecretKey == "" {
		return nil, errors.New("acme: MerchantID và SecretKey là bắt buộc")
	}
	u, err := url.Parse(c.BaseURL)
	if err != nil || u.Host == "" {
		return nil, errors.New("acme: BaseURL không hợp lệ")
	}
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && isLoopback(u.Hostname()):
	default:
		return nil, errors.New("acme: BaseURL phải là https (http chỉ cho localhost)")
	}
	return u, nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
