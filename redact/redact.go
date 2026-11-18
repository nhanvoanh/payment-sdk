// Package redact che dữ liệu cá nhân của khách và dữ liệu thẻ trong văn bản sắp được lưu vết hay ghi log.
// Connector (qua providerhttp.Redact) và core dùng CHUNG thuật toán này.
//
// Hai lớp:
//   - Values: thay mọi lần xuất hiện của các giá trị ĐÃ BIẾT (tên, email, điện thoại, địa chỉ...) ở các dạng
//     thô, JSON-escape (kể cả \uXXXX cho ký tự ngoài ASCII và "\/"), URL-encode (QueryEscape, PathEscape);
//     so không phân biệt hoa thường. Giá trị nhiều dòng được che cả nguyên vẹn lẫn từng dòng riêng. Giá trị có dạng số điện thoại được so thêm ở dạng chỉ còn chữ số và
//     các dạng đầu 0 / 84 / +84 suy ra được.
//   - Cards: không cần giá trị biết trước: dãy 13–19 chữ số liền nhau qua kiểm Luhn giữ 6 số đầu + 4 số
//     cuối; giá trị của trường cvv/cvc/cvn/card_cvv/security_code (JSON, form, XML) thành Mask.
//
// Giới hạn: không bắt được giá trị bị định dạng khác đi (khoảng trắng khác, chuẩn hoá Unicode NFD, viết tắt,
// mã hoá hai lần, base64), số điện thoại có cách viết khác cách suy ra ở trên, số thẻ có dấu cách hay gạch
// giữa các nhóm, và giá trị bị cắt đôi ở mép thân đã cắt.
package redact

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// Mask là chuỗi thay cho giá trị bị che.
const Mask = "[REDACTED]"

// MinValueRunes: giá trị ngắn hơn (sau khi cắt khoảng trắng) bị bỏ qua để không phá nát thân.
const MinValueRunes = 4

// String che cả hai lớp: giá trị đã biết rồi dữ liệu thẻ.
func String(s string, values ...string) string {
	return Cards(Values(s, values...))
}

// Redactor là bộ che đã dựng sẵn cho một tập giá trị: dùng khi che nhiều văn bản với cùng giá trị (dựng
// biểu thức một lần). Giá trị zero và nil không che giá trị nào, chỉ che dữ liệu thẻ.
type Redactor struct{ re *regexp.Regexp }

// New dựng Redactor cho values (cùng luật như Values).
func New(values ...string) *Redactor { return &Redactor{re: matcher(values)} }

// String như String của gói với các giá trị của r.
func (r *Redactor) String(s string) string {
	if r != nil && r.re != nil && s != "" {
		s = r.re.ReplaceAllLiteralString(s, Mask)
	}
	return Cards(s)
}

// Values thay mọi dạng của mỗi giá trị trong values bằng Mask. Byte không phải UTF-8 ngoài chỗ khớp giữ nguyên.
func Values(s string, values ...string) string {
	if s == "" {
		return s
	}
	re := matcher(values)
	if re == nil {
		return s
	}
	return re.ReplaceAllLiteralString(s, Mask)
}

// Contains báo s có còn chứa một dạng nào của các giá trị (cùng cách so với Values).
func Contains(s string, values ...string) bool {
	if s == "" {
		return false
	}
	re := matcher(values)
	return re != nil && re.MatchString(s)
}

// matcher dựng biểu thức khớp mọi dạng của mọi giá trị đủ dài; nil khi không có giá trị nào.
func matcher(values []string) *regexp.Regexp {
	set := map[string]bool{}
	for _, v := range withLines(values) {
		v = strings.TrimSpace(v)
		if utf8.RuneCountInString(v) < MinValueRunes {
			continue
		}
		for _, b := range bases(v) {
			for _, f := range forms(b) {
				set[f] = true
			}
		}
	}
	if len(set) == 0 {
		return nil
	}
	list := make([]string, 0, len(set))
	for f := range set {
		list = append(list, f)
	}
	// Dài trước: khi nhiều dạng cùng bắt đầu ở một chỗ, dạng dài nhất được thay (luật leftmost-first).
	sort.Slice(list, func(i, j int) bool {
		if len(list[i]) != len(list[j]) {
			return len(list[i]) > len(list[j])
		}
		return list[i] < list[j]
	})
	for i, f := range list {
		list[i] = regexp.QuoteMeta(f)
	}
	return regexp.MustCompile("(?i)(?:" + strings.Join(list, "|") + ")")
}

// withLines thêm từng dòng của giá trị nhiều dòng (địa chỉ) làm giá trị riêng: đối tác hay tách địa chỉ thành
// nhiều trường hoặc nối các dòng bằng dấu phẩy, khi đó giá trị nguyên vẹn không còn xuất hiện.
func withLines(values []string) []string {
	out := values
	for _, v := range values {
		if !strings.ContainsAny(v, "\r\n") {
			continue
		}
		if len(out) == len(values) {
			out = append([]string(nil), values...)
		}
		out = append(out, strings.FieldsFunc(v, func(r rune) bool { return r == '\r' || r == '\n' })...)
	}
	return out
}

// bases là giá trị cùng các cách viết tương đương của nó (hiện chỉ có số điện thoại).
func bases(v string) []string {
	out := []string{v}
	d, ok := phoneDigits(v)
	if !ok {
		return out
	}
	out = append(out, d)
	switch {
	case strings.HasPrefix(d, "84") && len(d) >= 10:
		out = append(out, "+"+d, "0"+d[2:])
	case strings.HasPrefix(d, "0") && len(d) >= 9:
		out = append(out, "84"+d[1:], "+84"+d[1:])
	}
	return out
}

// phoneDigits: v chỉ gồm chữ số và + - . ( ) khoảng trắng, có 8–15 chữ số → các chữ số của nó.
func phoneDigits(v string) (string, bool) {
	var b strings.Builder
	for _, r := range v {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case strings.ContainsRune("+-.() ", r):
		default:
			return "", false
		}
	}
	d := b.String()
	return d, len(d) >= 8 && len(d) <= 15
}

// forms là các dạng mã hoá của một giá trị có thể nằm trong thân request/response hay câu lỗi.
func forms(v string) []string {
	std, _ := json.Marshal(v)
	var noHTML bytes.Buffer
	enc := json.NewEncoder(&noHTML)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	plain := unquote(noHTML.Bytes())
	out := []string{v, unquote(std), plain, asciiJSON(plain), url.QueryEscape(v), url.PathEscape(v)}
	for _, f := range out[1:4] {
		if strings.Contains(f, "/") {
			out = append(out, strings.ReplaceAll(f, "/", `\/`))
		}
	}
	return out
}

func unquote(b []byte) string {
	s := strings.TrimSpace(string(b))
	return strings.TrimSuffix(strings.TrimPrefix(s, `"`), `"`)
}

// asciiJSON đổi mọi ký tự ngoài ASCII của chuỗi JSON đã escape thành \uXXXX (cặp thay thế cho ký tự ngoài BMP).
func asciiJSON(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r < utf8.RuneSelf:
			b.WriteRune(r)
		case r > 0xFFFF:
			r -= 0x10000
			fmt.Fprintf(&b, `\u%04x\u%04x`, 0xD800+(r>>10), 0xDC00+(r&0x3FF))
		default:
			fmt.Fprintf(&b, `\u%04x`, r)
		}
	}
	return b.String()
}

// Trường mã bảo mật của thẻ: tên đúng nguyên (không phân biệt hoa thường), không khớp tên dài hơn chứa nó.
const securityFields = `cvv|cvc|cvn|card_cvv|security_code`

var (
	jsonSecurity = regexp.MustCompile(`(?i)("(?:` + securityFields + `)"\s*:\s*)("(?:[^"\\]|\\.)*"|-?[0-9]+)`)
	formSecurity = regexp.MustCompile(`(?i)(^|[&?;\s])((?:` + securityFields + `)=)[^&;\s]*`)
	xmlSecurity  = regexp.MustCompile(`(?i)(<(?:` + securityFields + `)>)[^<]*(</(?:` + securityFields + `)>)`)
)

// Cards che số thẻ (dãy 13–19 chữ số liền nhau, không dính chữ số khác, qua Luhn) và giá trị mã bảo mật.
func Cards(s string) string {
	s = maskPANs(s)
	s = jsonSecurity.ReplaceAllString(s, `${1}"`+Mask+`"`)
	s = formSecurity.ReplaceAllString(s, `${1}${2}`+Mask)
	return xmlSecurity.ReplaceAllString(s, `${1}`+Mask+`${2}`)
}

func maskPANs(s string) string {
	var b []byte
	last := 0
	for i := 0; i < len(s); {
		if !isDigit(s[i]) {
			i++
			continue
		}
		j := i
		for j < len(s) && isDigit(s[j]) {
			j++
		}
		if n := j - i; n >= 13 && n <= 19 && luhn(s[i:j]) {
			if b == nil {
				b = make([]byte, 0, len(s))
			}
			b = append(b, s[last:i+6]...)
			b = append(b, strings.Repeat("*", n-10)...)
			b = append(b, s[j-4:j]...)
			last = j
		}
		i = j
	}
	if b == nil {
		return s
	}
	return string(append(b, s[last:]...))
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func luhn(d string) bool {
	sum := 0
	for i := len(d) - 1; i >= 0; i-- {
		n := int(d[i] - '0')
		if (len(d)-i)%2 == 0 {
			if n *= 2; n > 9 {
				n -= 9
			}
		}
		sum += n
	}
	return sum%10 == 0
}
