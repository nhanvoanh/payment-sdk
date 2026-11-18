package providerhttp_test

import (
	"strings"
	"testing"

	"bitbucket.org/hasaki-tech/payment-connector-sdk/paykit"
	"bitbucket.org/hasaki-tech/payment-connector-sdk/providerhttp"
	"bitbucket.org/hasaki-tech/payment-connector-sdk/redact"
)

func TestRedact_SameAlgorithmAsRedactPackage(t *testing.T) {
	body := `{"customer":{"name":"Nguyễn Canary","email":"canary@example.com"},"pan":"4111111111111111","q":"canary%40example.com"}`
	got := providerhttp.RedactValues(body, "Nguyễn Canary", "canary@example.com")
	if got != redact.String(body, "Nguyễn Canary", "canary@example.com") {
		t.Fatalf("RedactValues phải dùng đúng thuật toán của gói redact: %q", got)
	}
	if strings.Contains(strings.ToLower(got), "canary") || strings.Contains(got, "4111111111111111") {
		t.Fatalf("còn dữ liệu khách: %q", got)
	}
}

// RedactCustomer che cả họ/tên đã tách và tên bỏ dấu.
func TestRedactCustomer_SplitAndFoldedName(t *testing.T) {
	c := paykit.Customer{Name: "Đặng Canaryname", Email: "canary@example.com", Phone: "0987654321", Address: "12 Đường Canarystreet"}
	got := providerhttp.RedactCustomer(`{"first":"Canaryname","last":"Đặng","holder":"DANG CANARYNAME","tel":"0987 654 321"}`, c)
	if strings.Contains(strings.ToLower(got), "canary") || strings.Contains(got, "Đặng") || strings.Contains(got, "654") {
		t.Fatalf("còn dữ liệu khách: %q", got)
	}
}
