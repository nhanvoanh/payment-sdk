package conformance

import (
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"
)

// leaks nhận ra mã ở dạng thô và mọi dạng mã hoá đảo ngược được mà CHG-2 hứa: base64 (Std/URL, có/không đệm, ở mọi
// độ lệch trong một khối base64 lớn hơn) và hex (thường/hoa). Mã chọn sao cho các dạng khác nhau thật.
func TestLeaks_EncodedForms(t *testing.T) {
	code := "P~?>~?>~Z" + "1" // 10 byte: có đệm; byte 0x7E/0x3F/0x3E cho '+' '/' ở Std và '-' '_' ở URL
	std, url := base64.StdEncoding.EncodeToString([]byte(code)), base64.URLEncoding.EncodeToString([]byte(code))
	if std == url || !strings.HasSuffix(std, "=") {
		t.Fatal("mã thử phải cho Std khác URL và có đệm")
	}
	forms := map[string]string{
		"thô":                  code,
		"base64 Std":           std,
		"base64 Std không đệm": base64.RawStdEncoding.EncodeToString([]byte(code)),
		"base64 URL":           url,
		"base64 URL không đệm": base64.RawURLEncoding.EncodeToString([]byte(code)),
		"hex thường":           hex.EncodeToString([]byte(code)),
		"hex hoa":              strings.ToUpper(hex.EncodeToString([]byte(code))),
	}
	for k := 0; k < 3; k++ {
		blob := strings.Repeat("a", k) + `{"code":"` + code + `"}`
		forms["base64 Std lệch "+string(rune('0'+k))] = base64.StdEncoding.EncodeToString([]byte(blob))
		forms["base64 URL lệch "+string(rune('0'+k))] = base64.RawURLEncoding.EncodeToString([]byte(blob))
	}
	for name, f := range forms {
		if !leaks("x="+f+"&y=1", code) {
			t.Errorf("%s: không nhận ra", name)
		}
	}
	// Văn bản khác hẳn mã thì không bị báo. (Chuỗi chung gần hết các byte với mã thì có thể bị báo: mảnh base64 chỉ
	// phủ các nhóm 3 byte trọn của mã — chấp nhận được, vì khi đó gần như cả mã đã lộ.)
	for _, clean := range []string{"", "x=1", base64.StdEncoding.EncodeToString([]byte("Q0123456789")), hex.EncodeToString([]byte("other-code"))} {
		if leaks(clean, code) {
			t.Error("báo lộ trên văn bản không có mã")
		}
	}
}
