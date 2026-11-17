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
}

// ConfigFromEnv đọc ACME_BASE_URL, ACME_MERCHANT_ID, ACME_SECRET_KEY (bắt buộc) và ACME_TIMEOUT
// (tuỳ chọn, ví dụ "8s"). Thiếu hay sai thì lỗi nêu tên biến, không bao giờ nêu giá trị (có thể là secret).
func ConfigFromEnv(getenv func(string) string) (Config, error) {
	cfg := Config{
		BaseURL: getenv("ACME_BASE_URL"), MerchantID: getenv("ACME_MERCHANT_ID"), SecretKey: getenv("ACME_SECRET_KEY"),
	}
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
	if s := getenv("ACME_TIMEOUT"); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil || d <= 0 {
			return Config{}, errors.New("acme: ACME_TIMEOUT phải là khoảng thời gian dương, ví dụ 8s")
		}
		cfg.Timeout = d
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
