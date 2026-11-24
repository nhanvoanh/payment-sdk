package redact_test

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/nhanvoanh/payment-sdk/redact"
)

const (
	name  = "Trần Thị Canaryname"
	email = "Canary.Mail@Example.com"
	phone = "0912 345 678"
	addr  = "12 Đường Canary, Quận 1"
)

func jsonEsc(s string) string {
	b, _ := json.Marshal(s)
	return string(b[1 : len(b)-1])
}

// asciiJSON là dạng JSON mọi ký tự ngoài ASCII thành \uXXXX (như json_encode mặc định của nhiều ngôn ngữ).
func asciiJSON(s string, upper bool) string {
	var b strings.Builder
	for _, r := range s {
		if r < 0x80 {
			b.WriteRune(r)
			continue
		}
		var hex string
		if r > 0xFFFF {
			r -= 0x10000
			hex = u4(0xD800+(r>>10)) + u4(0xDC00+(r&0x3FF))
		} else {
			hex = u4(r)
		}
		if upper {
			hex = strings.ReplaceAll(strings.ToUpper(hex), `\U`, `\u`)
		}
		b.WriteString(hex)
	}
	return b.String()
}

func u4(r rune) string {
	const digits = "0123456789abcdef"
	return `\u` + string([]byte{digits[r>>12&0xF], digits[r>>8&0xF], digits[r>>4&0xF], digits[r&0xF]})
}

func TestValues_AllFormsOfAllValues(t *testing.T) {
	vals := []string{name, email, phone, addr}
	var parts []string
	for _, v := range vals {
		parts = append(parts, v, jsonEsc(v), asciiJSON(v, false), asciiJSON(v, true), url.QueryEscape(v), url.PathEscape(v))
	}
	parts = append(parts, strings.ToLower(email), strings.ToUpper(email), url.QueryEscape(strings.ToLower(email)))
	body := "<" + strings.Join(parts, "|") + ">"
	got := redact.Values(body, vals...)
	for _, v := range vals {
		for _, form := range []string{v, jsonEsc(v), asciiJSON(v, false), url.QueryEscape(v), url.PathEscape(v)} {
			if strings.Contains(got, form) {
				t.Errorf("còn %q trong %q", form, got)
			}
		}
	}
	if strings.Contains(strings.ToLower(got), "canary") {
		t.Errorf("còn chuỗi canh: %q", got)
	}
	if !strings.HasPrefix(got, "<"+redact.Mask) || !strings.HasSuffix(got, redact.Mask+">") {
		t.Errorf("phần ngoài giá trị phải giữ nguyên: %q", got)
	}
	if redact.Contains(got, vals...) {
		t.Error("Contains phải sai sau khi che")
	}
	if !redact.Contains(body, email) {
		t.Error("Contains phải đúng trước khi che")
	}
}

func TestValues_PhoneVariants(t *testing.T) {
	for _, in := range []string{"0912345678", "+84912345678", "84912345678", "+84 912 345 678", "0912-345-678"} {
		for _, body := range []string{
			`{"phone":"0912345678"}`, `{"phone":"+84912345678"}`, `{"phone":"84912345678"}`,
			`phone=%2B84912345678`, `{"phone":"+84912345678"}`,
		} {
			got := redact.Values(body, in)
			if strings.Contains(got, "912345678") {
				t.Errorf("giá trị %q: còn số điện thoại trong %q", in, got)
			}
		}
	}
}

func TestValues_ShortAndEmptyValuesIgnored(t *testing.T) {
	body := `{"a":"An","b":"Văn","c":""}`
	if got := redact.Values(body, "An", " Văn ", "", "   "); got != body {
		t.Errorf("giá trị ngắn hơn 4 ký tự không được che: %q", got)
	}
}

func TestValues_TrimsValue(t *testing.T) {
	if got := redact.Values("x Canaryname y", "  Canaryname \n"); got != "x "+redact.Mask+" y" {
		t.Errorf("got %q", got)
	}
}

func TestValues_KeepsInvalidUTF8Bytes(t *testing.T) {
	body := "\xff\xfe Canaryname \xc3"
	got := redact.Values(body, "Canaryname")
	if got != "\xff\xfe "+redact.Mask+" \xc3" {
		t.Errorf("got %q", got)
	}
}

func TestCards_LuhnNumbersMasked(t *testing.T) {
	cases := map[string]string{
		`{"pan":"4111111111111111"}`:     `{"pan":"411111******1111"}`,
		`card=5555555555554444&x=1`:      `card=555555******4444&x=1`,
		`n 378282246310005 e`:            `n 378282*****0005 e`,
		`ref 4111111111111112 not luhn`:  `ref 4111111111111112 not luhn`,
		`id 123456789012 short`:          `id 123456789012 short`,
		`long 41111111111111111111111 x`: `long 41111111111111111111111 x`,
	}
	for in, want := range cases {
		if got := redact.Cards(in); got != want {
			t.Errorf("Cards(%q) = %q, muốn %q", in, got, want)
		}
	}
}

func TestCards_SecurityCodeFields(t *testing.T) {
	cases := map[string]string{
		`{"cvv":"123","a":1}`:       `{"cvv":"[REDACTED]","a":1}`,
		`{"CVC" : 456}`:             `{"CVC" : "[REDACTED]"}`,
		`{"card_cvv":"7890"}`:       `{"card_cvv":"[REDACTED]"}`,
		`{"Security_Code":"12\"3"}`: `{"Security_Code":"[REDACTED]"}`,
		`a=1&cvn=999&b=2`:           `a=1&cvn=[REDACTED]&b=2`,
		`cvv=111`:                   `cvv=[REDACTED]`,
		`<cvv>321</cvv>`:            `<cvv>[REDACTED]</cvv>`,
		`{"xcvv":"123","cvvx":"1"}`: `{"xcvv":"123","cvvx":"1"}`,
		`a=1&mycvv=999`:             `a=1&mycvv=999`,
	}
	for in, want := range cases {
		if got := redact.Cards(in); got != want {
			t.Errorf("Cards(%q) = %q, muốn %q", in, got, want)
		}
	}
}

func TestString_BothLayers(t *testing.T) {
	got := redact.String(`{"name":"Canaryname","pan":"4111111111111111","cvv":"123"}`, "Canaryname")
	want := `{"name":"[REDACTED]","pan":"411111******1111","cvv":"[REDACTED]"}`
	if got != want {
		t.Errorf("got %q", got)
	}
}

func TestRedactor_SameAsString(t *testing.T) {
	r := redact.New(name, email)
	for _, in := range []string{"", "x " + name + " 4111111111111111", url.QueryEscape(email)} {
		if got, want := r.String(in), redact.String(in, name, email); got != want {
			t.Errorf("Redactor.String(%q) = %q, muốn %q", in, got, want)
		}
	}
	var nilR *redact.Redactor
	if got := nilR.String(`{"cvv":"1"}`); got != `{"cvv":"[REDACTED]"}` {
		t.Errorf("nil Redactor vẫn che thẻ: %q", got)
	}
}

// Địa chỉ nhiều dòng: che cả giá trị nguyên vẹn (mọi dạng, kể cả \n đã JSON-escape hay URL-encode) lẫn từng
// dòng riêng, vì đối tác thường tách địa chỉ thành nhiều trường hoặc nối các dòng bằng dấu phẩy.
func TestValues_MultiLineValue(t *testing.T) {
	const multi = "12 Đường Canarystreet\r\nPhường Bến Nghé\n  Quận 1  "
	for name, body := range map[string]string{
		"thô":           "addr=" + multi + ";",
		"JSON":          `{"address":"` + jsonEsc(multi) + `"}`,
		"URL-encode":    "address=" + url.QueryEscape(multi),
		"dòng tách":     `{"line1":"12 Đường Canarystreet","line2":"` + asciiJSON("Phường Bến Nghé", false) + `"}`,
		"nối bằng phẩy": "12 Đường Canarystreet, Phường Bến Nghé, Quận 1",
	} {
		got := redact.Values(body, multi)
		for _, leak := range []string{"Canarystreet", "Bến Nghé", `Bến Nghé`} {
			if strings.Contains(got, leak) {
				t.Errorf("%s: còn %q sau khi che: %s", name, leak, got)
			}
		}
	}
	// Thành phần ngắn hơn MinValueRunes (như "Q1") không bị che riêng để không phá nát thân; chỉ bị che khi là toàn
	// bộ giá trị của một trường gợi ý địa chỉ.
	if got := redact.Values(`{"note":"Q1"}`, "12 Đường Canarystreet\nQ1"); got != `{"note":"Q1"}` {
		t.Errorf("dòng ngắn bị che: %s", got)
	}
	if got := redact.Values(`{"district":"Q1"}`, "12 Đường Canarystreet\nQ1"); got != `{"district":"[REDACTED]"}` {
		t.Errorf("trường địa chỉ không bị che: %s", got)
	}
}
