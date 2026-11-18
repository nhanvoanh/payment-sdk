package redact_test

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"bitbucket.org/hasaki-tech/payment-connector-sdk/redact"
)

const shortAddr = "12 Lê Lợi\nThôn Đông, Xã Tân Phú\nHuyện Củ Chi"

// Dòng địa chỉ gồm thành phần ngắn, không chữ số: dòng ≥ 12 ký tự là giá trị trọn; trường gợi ý địa chỉ nhận một
// dãy thành phần liền nhau; khoá có số đuôi (address1, addr2, address_line1, name1) vẫn là khoá gợi ý.
func TestAddress_ShortComponentLines(t *testing.T) {
	r := redact.NewPII(redact.Customer("Lê Văn An", "", "", shortAddr))
	cases := []struct{ in, want string }{
		{`{"address":"Thôn Đông, Xã Tân Phú"}`, `{"address":"[REDACTED]"}`},
		{`<address>Thôn Đông, Xã Tân Phú</address>`, `<address>[REDACTED]</address>`},
		{`giao tới Thôn Đông - Xã Tân Phú.`, `giao tới [REDACTED].`},
		{`ship to THON DONG,XA TAN PHU`, `ship to [REDACTED]`},
		{`{"address1":"Thon Dong","addr2":"Xa Tan Phu"}`, `{"address1":"[REDACTED]","addr2":"[REDACTED]"}`},
		{`{"address_line1":"Xã Tân Phú, Huyện Củ Chi"}`, `{"address_line1":"[REDACTED]"}`},
		{`{"name1":"LE VAN","name2":"AN"}`, `{"name1":"[REDACTED]","name2":"[REDACTED]"}`},
		{`{"note":"Xã Tân Phú"}`, `{"note":"Xã Tân Phú"}`}, // thành phần ngắn, không số, ở trường thường: không che riêng
	}
	for _, c := range cases {
		if got := r.String(c.in); got != c.want {
			t.Errorf("%s\n   ra  %s\n  muốn %s", c.in, got, c.want)
		}
	}
}

// JSON lồng trong form-urlencoded: %5Cn là xuống dòng, %5Cu… là chữ; dấu nối đã mã hoá (+-+, %20-%20, %2C+) khớp địa chỉ trọn.
func TestEncoded_JSONInsideForm(t *testing.T) {
	const addr = "5 Nguyễn Thị Minh Khai\nPhường Bến Nghé\nQuận 1"
	r := redact.NewPII(redact.Customer("Lê Văn An", "", "", addr))
	for _, in := range []string{
		"data=" + url.QueryEscape(`{"address":"5 Nguyễn Thị Minh Khai\nPhường Bến Nghé\nQuận 1"}`),
		"data=" + url.QueryEscape(`{"address":"5 Nguyễn Thị Minh Khai\nPhường Bến Nghé"}`),
		"a=" + strings.ReplaceAll(url.QueryEscape("5 Nguyễn Thị Minh Khai - Phường Bến Nghé - Quận 1"), "+", "+"),
		"a=" + url.PathEscape("5 Nguyễn Thị Minh Khai - Phường Bến Nghé, Quận 1"),
		"a=5+Nguy%E1%BB%85n+Th%E1%BB%8B+Minh+Khai%2C+Ph%C6%B0%E1%BB%9Dng+B%E1%BA%BFn+Ngh%C3%A9",
	} {
		got := r.String(in)
		dec, _ := url.QueryUnescape(got)
		low := strings.ToLower(redact.Fold(dec))
		for _, leak := range []string{"minh khai", "ben nghe", "phuong"} {
			if strings.Contains(low, leak) {
				t.Errorf("%s\n  ra %s: còn %q", in, got, leak)
			}
		}
	}
}

// "%" đứng ngay trước giá trị có hai ký tự đầu là hex không làm lọt giá trị; "+" trước điện thoại được che cùng.
func TestEncoded_PercentBeforeValueAndPhonePlus(t *testing.T) {
	r := redact.NewPII(redact.Customer("", "abc@d.ef", "0987654321", ""))
	cases := map[string]string{
		"giảm 50%0987654321": "giảm 50%[REDACTED]",
		"x=%abc@d.ef":        "x=%[REDACTED]",
		"(+84) 987 654 321":  "([REDACTED]",
		"tel=%2B84987654321": "tel=[REDACTED]",
		`"+84987654321"`:     `"[REDACTED]"`,
	}
	for in, want := range cases {
		if got := r.String(in); got != want {
			t.Errorf("%q → %q, muốn %q", in, got, want)
		}
	}
}

// "_" chỉ là dấu nối của chữ trong GIÁ TRỊ: khoá/tên tham số có "_" không bị che.
func TestWords_UnderscoreOnlyInValues(t *testing.T) {
	r := redact.NewPII(redact.Customer("Test True Order", "", "", ""))
	cases := map[string]string{
		`{"order_test":1,"test_order" : 2}`: `{"order_test":1,"test_order" : 2}`,
		`order_test=1&x=TEST_ORDER`:         `order_test=1&x=[REDACTED]`,
		`<order_test>1</order_test>`:        `<order_test>1</order_test>`,
		`{"memo":"test_order"}`:             `{"memo":"[REDACTED]"}`,
	}
	for in, want := range cases {
		if got := r.String(in); got != want {
			t.Errorf("%s → %s, muốn %s", in, got, want)
		}
	}
}

// Thân bệnh lý 1 MiB che xong dưới 500 ms (không -race): gộp khoảng O(n log n), không quét lại giá trị ở khoá rỗng.
func TestPathologicalBodiesFast(t *testing.T) {
	if raceEnabled || testing.Short() {
		t.Skip("đo thời gian không chạy dưới -race / -short")
	}
	r := redact.NewPII(redact.Customer("Lê Văn An", "an@example.com", "0987654321", shortAddr))
	const mib = 1 << 20
	rep := func(unit string) string { return strings.Repeat(unit, mib/len(unit)+1)[:mib] }
	for name, body := range map[string]string{
		"tên lặp":     rep(`"name":"Lê",`),
		"địa chỉ lặp": rep("Thôn Đông, Xã Tân Phú; "),
		"====":        rep("="),
		"%%%%":        rep("%"),
		`\\\\`:        rep(`\`),
		"chữ số":      rep("0123456789"),
		"LE LE LE":    rep("LE "),
		"%HH":         rep("%41"),
		"a=a=":        rep("a="),
		"<<<<":        rep("<a>"),
		"::::":        rep(`"":`),
	} {
		began := time.Now()
		_ = r.String(body)
		if d := time.Since(began); d > 500*time.Millisecond {
			t.Errorf("%s: %v (> 500 ms)", name, d)
		}
	}
}
