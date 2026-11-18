package redact_test

import (
	"strings"
	"testing"

	"bitbucket.org/hasaki-tech/payment-connector-sdk/redact"
)

// Bảng hai chiều của luật chữ mạnh/yếu (review vòng 2): đầu vào → đầu ra chính xác.
func TestPII_StrongWeakWordsTable(t *testing.T) {
	const addr = "5 Nguyễn Thị Minh Khai\nPhường Bến Nghé\nQuận 1"
	cases := []struct{ name, in, want string }{
		// PHẢI che.
		{"Lê Văn An", `{"last":"LE","first":"VAN AN"}`, `{"last":"[REDACTED]","first":"[REDACTED]"}`},
		{"Lê Văn An", `{"ho":"LE","ten":"AN","dem":"VAN"}`, `{"ho":"[REDACTED]","ten":"[REDACTED]","dem":"[REDACTED]"}`},
		{"Lê Văn An", `first=VAN+AN&last=LE`, `first=[REDACTED]&last=[REDACTED]`},
		{"Lê Văn An", `{"firstName":"Văn An","lastName":"Lê"}`, `{"firstName":"[REDACTED]","lastName":"[REDACTED]"}`},
		{"Lê Văn An", `<CardHolder>LE VAN AN</CardHolder>`, `<CardHolder>[REDACTED]</CardHolder>`},
		{"Lê Văn An", `{"note":"giao cho Lê"}`, `{"note":"giao cho [REDACTED]"}`},
		{"Lê Văn An", `{"memo":"VAN AN da thanh toan"}`, `{"memo":"[REDACTED] da thanh toan"}`},
		{"Huỳnh Đoàn Canaryname", `{"middle":"DOAN"}`, `{"middle":"[REDACTED]"}`},
		{"Huỳnh Đoàn Canaryname", `{"last":"HUYNH"}`, `{"last":"[REDACTED]"}`},
		{"Huỳnh Đoàn Canaryname", `{"x":"Đoàn"}`, `{"x":"[REDACTED]"}`},
		{"", `{"ward":"Phuong Ben Nghe"}`, `{"ward":"[REDACTED]"}`},
		{"", `giao tới 5 Nguyễn Thị Minh Khai - Phường Bến Nghé - Quận 1.`, `giao tới [REDACTED].`},
		{"", `{"street":"5 NGUYEN THI MINH KHAI"}`, `{"street":"[REDACTED]"}`},
		// KHÔNG được che.
		{"Lê Văn An", `An error occurred`, `An error occurred`},
		{"Lê Văn An", `{"msg":"an toan","status":"AN"}`, `{"msg":"an toan","status":"AN"}`},
		{"Lê Văn An", `{"lang":"le","van":1}`, `{"lang":"le","van":1}`},
		{"Trần Thanh An", `Giao dich thanh cong`, `Giao dich thanh cong`},
		{"Trần Thanh An", `{"description":"Thanh toan don hang"}`, `{"description":"Thanh toan don hang"}`},
		{"Thành Long", `{"amount_long":1,"desc":"Thanh toan"}`, `{"amount_long":1,"desc":"Thanh toan"}`},
		{"Test True Order", `mode=test&x=1`, `mode=test&x=1`},
		{"Test True Order", `{"test_mode":true,"paid":true,"Order":{"code":"A"}}`, `{"test_mode":true,"paid":true,"Order":{"code":"A"}}`},
		{"", `{"city":"Hà Nội","note":"Bến Nghé"}`, `{"city":"Hà Nội","note":"Bến Nghé"}`},
	}
	for _, c := range cases {
		got := redact.NewPII(redact.Customer(c.name, "", "", addr)).String(c.in)
		if got != c.want {
			t.Errorf("khách %q: %s\n   ra  %s\n  muốn %s", c.name, c.in, got, c.want)
		}
	}
}

// Mã đã biết của lời gọi không bao giờ bị coi là số thẻ.
func TestRedactor_KeepCodes(t *testing.T) {
	r := redact.NewPII(redact.PII{Keep: []string{"4111111111111111"}})
	if got := r.String(`{"ref":"4111111111111111"}`); got != `{"ref":"4111111111111111"}` {
		t.Errorf("mã đã biết bị che: %s", got)
	}
	if got := r.WithKeep("5555555555554444").String("a 5555555555554444 b 4012888888881881"); got != "a 5555555555554444 b 401288******1881" {
		t.Errorf("WithKeep: %s", got)
	}
	if got := (*redact.Redactor)(nil).WithKeep("4111111111111111").String("4111111111111111"); got != "4111111111111111" {
		t.Errorf("nil.WithKeep: %s", got)
	}
}

func FuzzString(f *testing.F) {
	for _, s := range []string{`{"name":"Lê Văn An","x":"Đ"}`, "%C4%90%E1", `\\u00`, "+84 (0) 987-654-321",
		"4111 1111 1111 1111", "<ho>LE</ho>", "a=b=c&", "\xff\xfe", `"k":"`, "\\uD83D\\uDE00"} {
		f.Add(s)
	}
	r := redact.NewPII(redact.Customer("Lê Văn An", "an@example.com", "0987654321", "12 Lê Lợi\nQuận 1"))
	f.Fuzz(func(t *testing.T, s string) {
		out := r.String(s)
		_ = r.Contains(s)
		_ = redact.NewPII(redact.Customer(s, s, s, s)).String(s)
		if strings.Contains(s, "an@example.com") && strings.Contains(out, "an@example.com") {
			t.Fatalf("email còn: %q", out)
		}
	})
}
