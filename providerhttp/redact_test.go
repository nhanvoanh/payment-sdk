package providerhttp_test

import (
	"strings"
	"testing"

	"bitbucket.org/hasaki-tech/payment-connector-sdk/providerhttp"
	"bitbucket.org/hasaki-tech/payment-connector-sdk/redact"
)

func TestRedact_SameAlgorithmAsRedactPackage(t *testing.T) {
	body := `{"customer":{"name":"Nguyễn Canary","email":"canary@example.com"},"pan":"4111111111111111","q":"canary%40example.com"}`
	got := providerhttp.Redact(body, "Nguyễn Canary", "canary@example.com")
	if got != redact.String(body, "Nguyễn Canary", "canary@example.com") {
		t.Fatalf("Redact phải dùng đúng thuật toán của gói redact: %q", got)
	}
	if strings.Contains(strings.ToLower(got), "canary") || strings.Contains(got, "4111111111111111") {
		t.Fatalf("còn dữ liệu khách: %q", got)
	}
}
