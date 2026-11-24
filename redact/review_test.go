package redact_test

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/nhanvoanh/payment-sdk/redact"
)

// Tên/địa chỉ bỏ dấu (tên chủ thẻ ASCII hoa ở cổng thanh toán Việt Nam) vẫn bị che như giá trị trọn.
func TestValues_DiacriticFolded(t *testing.T) {
	cases := []struct{ body, value, leak string }{
		{`{"holder":"NGUYEN VAN AN"}`, "Nguyễn Văn An", "NGUYEN"},
		{`{"holder":"DANG CANARYNAME"}`, "Đặng Canaryname", "CANARYNAME"},
		{`desc=Thanh toan cho Le Thi Hoa`, "Lê Thị Hoa", "Hoa"},
		{`{"addr":"12 Duong Canarystreet, Phuong Ben Nghe"}`, "12 Đường Canarystreet\nPhường Bến Nghé", "Canarystreet"},
		{`{"line2":"phuong ben nghe"}`, "12 Đường Canarystreet\nPhường Bến Nghé", "ben nghe"},
	}
	for _, c := range cases {
		got := redact.String(c.body, c.value)
		if strings.Contains(got, c.leak) {
			t.Errorf("String(%q, %q) = %q: còn %q", c.body, c.value, got, c.leak)
		}
	}
}

// Chữ tách từ tên chỉ bị che khi khớp TRỌN TỪ; giá trị trọn vẫn so chuỗi con.
func TestPII_WordsWholeWordBothDirections(t *testing.T) {
	leak := []struct {
		name, body string
	}{
		{"Lê Văn An", `{"first_name":"Văn An","last_name":"Lê"}`},
		{"Đặng Canaryname", `{"first":"Canaryname","last":"Đặng"}`},
		{"Lê Văn An", `{"last_name":"Lê"}`},
		{"Lê Văn An", `first_name=V%C4%83n+An&last_name=L%C3%AA`},
	}
	for _, c := range leak {
		p := redact.Customer(c.name, "", "", "")
		got := redact.NewPII(p).String(c.body)
		for _, w := range strings.Fields(c.name) {
			if strings.Contains(got, `"`+w+`"`) || strings.Contains(got, " "+w+`"`) || strings.Contains(got, `"`+w+" ") {
				t.Errorf("%q: còn chữ %q trong %q", c.name, w, got)
			}
		}
		if got == c.body {
			t.Errorf("%q: không che gì trong %q", c.name, c.body)
		}
	}
	keep := []struct{ name, body string }{
		{"Thành Long", `{"description":"Thanh toan don hang","amount_long":1}`},
		{"Test True", `{"test_mode":true,"paid":true,"x":null}`},
		{"Order Null", `{"order_code":"A1","note":null}`},
		{"Long An", `{"amount_long":5,"plan":"annual","Anh":"x"}`},
	}
	for _, c := range keep {
		got := redact.NewPII(redact.Customer(c.name, "", "", "")).String(c.body)
		if got != c.body {
			t.Errorf("%q che thừa: %q → %q", c.name, c.body, got)
		}
	}
	// Chữ đứng riêng trong chuỗi thì vẫn bị che.
	if got := redact.NewPII(redact.Customer("Test True", "", "", "")).String(`{"holder":"Test"}`); got != `{"holder":"[REDACTED]"}` {
		t.Errorf("chữ đứng riêng không bị che: %q", got)
	}
	if !redact.NewPII(redact.Customer("Đặng Canaryname", "", "", "")).Contains(`{"last":"Đặng"}`) {
		t.Error("Contains phải thấy chữ tách từ tên")
	}
}

// Giá trị không phải UTF-8 không làm panic; dạng byte gốc vẫn bị che.
func TestValues_InvalidUTF8ValueNoPanic(t *testing.T) {
	got := redact.String("x Canary\xffname y", "Canary\xffname")
	if got != "x "+redact.Mask+" y" {
		t.Errorf("got %q", got)
	}
	if redact.Contains("x Canary\xffname y", "\xff\xfe\xfd\xfc") {
		t.Error("không có giá trị đó")
	}
	_ = redact.NewPII(redact.PII{Names: []string{"\xff\xfe Ab\xff"}}).String("abc")
}

// Điện thoại có dấu phân cách trong thân; không cắt giữa một dãy số dài hơn.
func TestValues_PhoneSeparatorsAndDigitBoundary(t *testing.T) {
	for _, body := range []string{"0987 654 321", "0987-654-321", "(+84) 987 654 321", "+84(0)987654321",
		"0987.654.321", "84987654321", "+84 98 765 43 21", "tel=%2B84987654321", "0987+654+321", "FT0987654321X"} {
		got := redact.String("<"+body+">", "0987654321")
		if strings.Contains(got, "654") || strings.Contains(got, "321") {
			t.Errorf("%q → %q: còn số điện thoại", body, got)
		}
	}
	for _, body := range []string{`{"amount":10987654321}`, `ref 09876543210`, `id=8409876543219`} {
		if got := redact.String(body, "0987654321"); got != body {
			t.Errorf("%q bị che nhầm: %q", body, got)
		}
	}
}

// Hai giá trị chồng lấn không cùng điểm bắt đầu: che theo hợp các khoảng khớp.
func TestValues_OverlappingValuesUnion(t *testing.T) {
	if got := redact.Values("<abcd efgh ijkl>", "abcd efgh", "efgh ijkl"); got != "<"+redact.Mask+">" {
		t.Errorf("got %q", got)
	}
}

// \u hoa/thường lẫn lộn và JSON lồng hai lần.
func TestValues_EscapeCaseAndDoubleJSON(t *testing.T) {
	for _, body := range []string{`"Đặng Canaryname"`, `"\\u0110\\u1eb7ng Canaryname"`, `"a\\/b Canaryname"`} {
		got := redact.Values(body, "Đặng Canaryname", "a/b Canaryname")
		if strings.Contains(strings.ToLower(got), "canary") {
			t.Errorf("%q → %q", body, got)
		}
	}
}

// Dòng địa chỉ ngắn (< 12 ký tự) không bị che riêng; chỉ che trong giá trị trọn, kể cả khi \n bị thay.
func TestValues_ShortAddressLines(t *testing.T) {
	const a = "12 Đường Canarystreet\nQuận 1\nHà Nội"
	// Thành phần không có chữ số và < 12 ký tự ("Hà Nội") không bị che riêng ở trường thường; có chữ số ("Quận 1")
	// thì bị che; ở trường gợi ý địa chỉ (city) thì thành phần nào cũng bị che.
	if got := redact.Values(`{"note":"Hà Nội","memo":"Quận 1"}`, a); got != `{"note":"Hà Nội","memo":"[REDACTED]"}` {
		t.Errorf("thành phần ngắn: %q", got)
	}
	if got := redact.Values(`{"city":"Ha Noi"}`, a); got != `{"city":"[REDACTED]"}` {
		t.Errorf("trường địa chỉ: %q", got)
	}
	for _, body := range []string{"12 Đường Canarystreet Quận 1 Hà Nội", "12 Đường Canarystreet, Quận 1, Hà Nội",
		`12 Đường Canarystreet\nQuận 1\nHà Nội`, "12+%C4%90%C6%B0%E1%BB%9Dng+Canarystreet%0AQu%E1%BA%ADn+1%0AH%C3%A0+N%E1%BB%99i"} {
		if got := redact.Values(body, a); got != redact.Mask {
			t.Errorf("%q → %q", body, got)
		}
	}
}

// Số thẻ: nhóm có dấu cách/gạch; không coi là thẻ khi dính chữ/số khác hoặc đầu số ngoài dải thẻ.
func TestCards_GroupsAndFalsePositives(t *testing.T) {
	cases := map[string]string{
		`pan 4111 1111 1111 1111 x`:    `pan 4111 11** **** 1111 x`,
		`pan 4111-1111-1111-1111`:      `pan 4111-11**-****-1111`,
		`nd 9704000000000018`:          `nd 970400******0018`,
		`ref FT4111111111111111`:       `ref FT4111111111111111`,
		`ref 4111111111111111A`:        `ref 4111111111111111A`,
		`ref 1111111111111117`:         `ref 1111111111111117`,
		`ref 4111 1111 1111 1111 2222`: `ref 4111 1111 1111 1111 2222`,
	}
	for in, want := range cases {
		if got := redact.Cards(in); got != want {
			t.Errorf("Cards(%q) = %q, muốn %q", in, got, want)
		}
	}
}

// Tỉ lệ che nhầm trên mã tham chiếu 15–16 số ngẫu nhiên: Luhn (~1/10) × dải đầu số theo độ dài (≈ 2% với 15 số, ≈ 23% với 16 số) ≈ 1,3%.
func TestCards_FalsePositiveRateOnRandomRefs(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	hit := 0
	const n = 10000
	for i := 0; i < n; i++ {
		d := 15 + i%2
		var b strings.Builder
		for j := 0; j < d; j++ {
			fmt.Fprintf(&b, "%d", rng.Intn(10))
		}
		if redact.Cards("ref "+b.String()) != "ref "+b.String() {
			hit++
		}
	}
	if hit > n*3/100 {
		t.Errorf("che nhầm %d/%d mã ngẫu nhiên, muốn ≤ 3%%", hit, n)
	}
}
