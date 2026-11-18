// Package redact che dữ liệu cá nhân của khách và dữ liệu thẻ trong văn bản sắp được lưu vết hay ghi log.
// Connector (qua providerhttp.RedactValues/RedactCustomer) và core dùng CHUNG thuật toán này. Chỉ dùng thư viện chuẩn.
//
// Hai lớp:
//   - Giá trị ĐÃ BIẾT (PII):
//   - Values (tên đầy đủ, email, điện thoại, địa chỉ) so CHUỖI CON, không phân biệt hoa thường, ở các dạng
//     thô, JSON-escape (kể cả \uXXXX hoa/thường cho ký tự ngoài ASCII, "\/", và JSON lồng hai lần),
//     URL-encode (QueryEscape, PathEscape), và dạng BỎ DẤU tiếng Việt (Đ→D, ă/â/ơ/ư và mọi dấu thanh).
//     Giá trị nhiều dòng (địa chỉ) so cả nguyên vẹn, nối bằng dấu cách hay ", ", và từng dòng ≥ 12 ký tự.
//     Giá trị dạng số điện thoại so theo chữ số, cho phép dấu phân cách xen giữa và các đầu 0 / 84 / +84 /
//     (+84) / +84(0), không bao giờ cắt giữa một dãy số dài hơn.
//   - Words (chữ tách từ tên) chỉ so TRỌN TỪ (hai bên không phải chữ, số hay "_"; chữ đứng trần sau ":"
//     như true/false/null của JSON không tính), không bỏ dấu, chữ ≥ 2 ký tự.
//   - Cards: không cần giá trị biết trước: 13–19 chữ số (liền nhau hoặc nhóm cách nhau bởi một dấu cách/gạch),
//     không dính chữ hay số khác, qua Luhn, đầu số 2–6 hoặc 9704 → giữ 6 số đầu + 4 số cuối; giá trị của trường
//     cvv/cvc/cvn/card_cvv/security_code (JSON, form, XML) thành Mask.
//
// Các khoảng khớp được gộp (hợp) rồi mới thay, nên hai giá trị chồng lấn không để lọt phần nào.
//
// Giới hạn: không bắt được chuẩn hoá Unicode NFD, viết tắt, base64, mã hoá ba lần trở lên, chữ của tên đã bỏ
// dấu khi đứng tách riêng, điện thoại nằm trong dãy chữ-số dài hơn chỉ khi có số kề, và giá trị bị cắt đôi ở
// mép thân đã cắt.
package redact

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Mask là chuỗi thay cho giá trị bị che.
const Mask = "[REDACTED]"

// MinValueRunes: giá trị trọn ngắn hơn (sau khi cắt khoảng trắng) bị bỏ qua để không phá nát thân.
const MinValueRunes = 4

// MinWordRunes: chữ tách từ tên ngắn hơn bị bỏ qua.
const MinWordRunes = 2

// MinLineRunes: dòng của giá trị nhiều dòng chỉ được che riêng khi dài ít nhất chừng này; dòng ngắn hơn ("Quận 1",
// "Hà Nội") chỉ bị che như một phần của giá trị trọn, để không che thừa mọi thân có các chữ đó.
const MinLineRunes = 12

// PII là dữ liệu cá nhân cần che: Values so chuỗi con, Words so trọn từ.
type PII struct {
	Values []string
	Words  []string
}

// Customer dựng PII của một khách: tên, email, điện thoại, địa chỉ là giá trị trọn; từng chữ của tên (khi tên
// có nhiều chữ) là Words, vì đối tác hay tách họ và tên.
func Customer(name, email, phone, address string) PII {
	p := PII{Values: []string{name, email, phone, address}}
	if f := strings.Fields(name); len(f) > 1 {
		p.Words = f
	}
	return p
}

// String che cả hai lớp với values là giá trị trọn.
func String(s string, values ...string) string { return New(values...).String(s) }

// Values chỉ che giá trị trọn (không che thẻ). Byte không phải UTF-8 ngoài chỗ khớp giữ nguyên.
func Values(s string, values ...string) string { return New(values...).values(s) }

// Contains báo s còn chứa một dạng nào của values (cùng cách so với Values).
func Contains(s string, values ...string) bool { return New(values...).Contains(s) }

// Redactor là bộ che đã dựng sẵn cho một PII (dựng biểu thức một lần). nil không che giá trị nào, chỉ che thẻ.
// Không bao giờ panic vì đầu vào.
type Redactor struct {
	full  *regexp.Regexp // giá trị trọn, chuỗi con
	words *regexp.Regexp // chữ của tên, trọn từ
	phone *regexp.Regexp // số điện thoại, không kề chữ số
	lit   []string       // dạng byte gốc (giá trị không phải UTF-8, hoặc khi biểu thức không dựng được), so đúng byte
}

// New dựng Redactor với values là giá trị trọn.
func New(values ...string) *Redactor { return NewPII(PII{Values: values}) }

// NewPII dựng Redactor cho p.
func NewPII(p PII) *Redactor {
	r := &Redactor{}
	full, words := map[string]bool{}, map[string]bool{}
	var phones []string
	for _, v := range p.Values {
		v = r.valid(strings.TrimSpace(v))
		if d, ok := phoneDigits(v); ok {
			phones = append(phones, d)
			continue
		}
		for _, b := range wholeForms(v) {
			if utf8.RuneCountInString(b) < MinValueRunes {
				continue
			}
			addForms(full, b)
			if f := fold(b); f != b {
				addForms(full, f)
			}
		}
	}
	for _, w := range p.Words {
		if w = r.valid(strings.TrimSpace(w)); utf8.RuneCountInString(w) >= MinWordRunes {
			addForms(words, w)
		}
	}
	r.full = r.compile(full)
	r.words = r.compile(words)
	if len(phones) > 0 {
		var alts []string
		for _, d := range phones {
			alts = append(alts, phonePattern(d))
		}
		r.phone, _ = regexp.Compile("(?:" + strings.Join(alts, "|") + ")")
	}
	return r
}

// valid: giá trị không phải UTF-8 được giữ dạng byte gốc để so đúng byte, và đổi thành UTF-8 hợp lệ cho biểu thức.
func (r *Redactor) valid(v string) string {
	if utf8.ValidString(v) {
		return v
	}
	if len(v) >= MinValueRunes {
		r.lit = append(r.lit, v)
	}
	return strings.ToValidUTF8(v, "�")
}

// compile dựng một biểu thức không phân biệt hoa thường cho tập dạng, dài trước (luật leftmost-first). Không
// dựng được (không xảy ra với chuỗi đã QuoteMeta) thì các dạng thành so đúng byte, không bao giờ panic.
func (r *Redactor) compile(set map[string]bool) *regexp.Regexp {
	if len(set) == 0 {
		return nil
	}
	list := make([]string, 0, len(set))
	for f := range set {
		list = append(list, f)
	}
	sort.Slice(list, func(i, j int) bool {
		if len(list[i]) != len(list[j]) {
			return len(list[i]) > len(list[j])
		}
		return list[i] < list[j]
	})
	quoted := make([]string, len(list))
	for i, f := range list {
		quoted[i] = regexp.QuoteMeta(f)
	}
	re, err := regexp.Compile("(?i)(?:" + strings.Join(quoted, "|") + ")")
	if err != nil {
		r.lit = append(r.lit, list...)
		return nil
	}
	return re
}

// String che giá trị của r rồi che thẻ.
func (r *Redactor) String(s string) string { return Cards(r.values(s)) }

// Contains báo s còn chứa một dạng nào của PII của r.
func (r *Redactor) Contains(s string) bool { return len(r.spans(s, true)) > 0 }

func (r *Redactor) values(s string) string {
	sp := r.spans(s, false)
	if len(sp) == 0 {
		return s
	}
	var b strings.Builder
	last := 0
	for _, x := range sp {
		b.WriteString(s[last:x[0]])
		b.WriteString(Mask)
		last = x[1]
	}
	b.WriteString(s[last:])
	return b.String()
}

// spans trả các khoảng cần che, đã gộp (hợp các khoảng chồng lấn hoặc kề nhau); first: dừng ở khoảng đầu tiên.
func (r *Redactor) spans(s string, first bool) [][2]int {
	if r == nil || s == "" {
		return nil
	}
	var out [][2]int
	add := func(i, j int) bool { out = append(out, [2]int{i, j}); return first }
	if scan(s, r.full, nil, add) || scan(s, r.words, wordBoundary, add) || scan(s, r.phone, digitBoundary, add) {
		return out
	}
	for _, l := range r.lit {
		for i := 0; i <= len(s)-len(l); {
			k := strings.Index(s[i:], l)
			if k < 0 {
				break
			}
			if add(i+k, i+k+len(l)) {
				return out
			}
			i += k + 1
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
	merged := out[:0]
	for _, x := range out {
		if n := len(merged); n > 0 && x[0] <= merged[n-1][1] {
			merged[n-1][1] = max(merged[n-1][1], x[1])
			continue
		}
		merged = append(merged, x)
	}
	return merged
}

// scan tìm mọi khớp của re (kể cả khớp chồng lấn: lần tìm sau bắt đầu ngay sau điểm đầu của khớp trước), giữ khớp
// qua ok; add trả true để dừng. Trả true khi đã dừng.
func scan(s string, re *regexp.Regexp, ok func(s string, i, j int) bool, add func(i, j int) bool) bool {
	if re == nil {
		return false
	}
	for pos := 0; pos < len(s); {
		loc := re.FindStringIndex(s[pos:])
		if loc == nil {
			return false
		}
		i, j := pos+loc[0], pos+loc[1]
		if j > i && (ok == nil || ok(s, i, j)) && add(i, j) {
			return true
		}
		_, size := utf8.DecodeRuneInString(s[i:])
		pos = i + max(size, 1)
	}
	return false
}

// wholeForms: giá trị một dòng là chính nó; giá trị nhiều dòng là bản nối các dòng bằng \n, \r\n, dấu cách,
// ", " và "," cùng từng dòng đủ dài (MinLineRunes).
func wholeForms(v string) []string {
	if !strings.ContainsAny(v, "\r\n") {
		return []string{v}
	}
	var lines []string
	for _, l := range strings.FieldsFunc(v, func(r rune) bool { return r == '\r' || r == '\n' }) {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	out := []string{v}
	for _, sep := range []string{"\n", "\r\n", " ", ", ", ","} {
		out = append(out, strings.Join(lines, sep))
	}
	for _, l := range lines {
		if utf8.RuneCountInString(l) >= MinLineRunes {
			out = append(out, l)
		}
	}
	return out
}

// isWordRune: chữ, số Unicode và "_" (để amount_long, test_mode, order_code không bị coi là chứa từ riêng).
func isWordRune(r rune) bool { return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) }

// wordBoundary: trước và sau khớp không phải ký tự của từ. Ký tự ASCII không phải chữ/số được mã hoá (%20,  )
// cũng là ranh giới. Chữ đứng trần sau ":" hay "," (true/false/null của JSON) không tính là từ của khách.
func wordBoundary(s string, i, j int) bool {
	if i > 0 {
		prev, _ := utf8.DecodeLastRuneInString(s[:i])
		if isWordRune(prev) && !encodedSeparatorBefore(s, i) {
			return false
		}
	}
	if j < len(s) {
		next, _ := utf8.DecodeRuneInString(s[j:])
		if isWordRune(next) {
			return false
		}
	}
	switch strings.ToLower(s[i:j]) {
	case "true", "false", "null":
		k := i
		for k > 0 && (s[k-1] == ' ' || s[k-1] == '\t') {
			k--
		}
		if k == 0 || s[k-1] != '"' {
			return false
		}
	}
	return true
}

// encodedSeparatorBefore: ngay trước i là %HH hoặc \uXXXX mã hoá một ký tự không phải chữ/số/"_".
func encodedSeparatorBefore(s string, i int) bool {
	if i >= 3 && s[i-3] == '%' {
		var b int
		if _, err := fmt.Sscanf(s[i-2:i], "%02x", &b); err == nil && b < utf8.RuneSelf && !isWordRune(rune(b)) {
			return true
		}
	}
	if i >= 6 && s[i-6] == '\\' && (s[i-5] == 'u' || s[i-5] == 'U') {
		var c int
		if _, err := fmt.Sscanf(s[i-4:i], "%04x", &c); err == nil && !isWordRune(rune(c)) {
			return true
		}
	}
	return false
}

// digitBoundary: không có chữ số ASCII kề hai bên (không cắt giữa một dãy số dài hơn).
func digitBoundary(s string, i, j int) bool {
	return (i == 0 || !isDigit(s[i-1])) && (j == len(s) || !isDigit(s[j]))
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

const (
	phoneSep  = `(?:[\s.()\-]|\+|%20)*`
	phonePlus = `(?:\+|%2[bB]|\\u002[bB]|00)`
)

// phonePattern: các chữ số quốc gia (bỏ 84 hoặc 0 đầu) với dấu phân cách xen giữa, sau một đầu tuỳ chọn
// 0 / 84 / +84 / (+84) / +84(0); số không theo dạng Việt Nam chỉ có "+" tuỳ chọn ở đầu.
func phonePattern(d string) string {
	n, prefix := d, `(?:`+phonePlus+`[\s(]*)?`
	switch {
	case strings.HasPrefix(d, "84") && len(d) >= 10:
		n = d[2:]
	case strings.HasPrefix(d, "0") && len(d) >= 9:
		n = d[1:]
	default:
		return prefix + interleave(n)
	}
	n = strings.TrimPrefix(n, "0")
	prefix = `(?:(?:` + phonePlus + `?[\s(]*84[\s.()\-]*0?` + `|0)` + phoneSep + `)?`
	return prefix + interleave(n)
}

func interleave(digits string) string {
	parts := strings.Split(digits, "")
	return strings.Join(parts, phoneSep)
}

// addForms thêm mọi dạng mã hoá của v có thể nằm trong thân request/response hay câu lỗi.
func addForms(set map[string]bool, v string) {
	std, _ := json.Marshal(v)
	var noHTML bytes.Buffer
	enc := json.NewEncoder(&noHTML)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	plain := unquote(noHTML.Bytes())
	jsonForms := []string{unquote(std), plain, asciiJSON(plain)}
	for _, f := range jsonForms[:3] {
		if strings.Contains(f, "/") {
			jsonForms = append(jsonForms, strings.ReplaceAll(f, "/", `\/`))
		}
	}
	for _, f := range jsonForms {
		set[f] = true
		if strings.Contains(f, `\`) {
			set[strings.ReplaceAll(f, `\`, `\\`)] = true // JSON lồng hai lần
		}
	}
	set[v] = true
	set[url.QueryEscape(v)] = true
	set[url.PathEscape(v)] = true
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

// vietFold: chữ có dấu tiếng Việt (dựng sẵn) → chữ không dấu.
var vietFold = func() map[rune]rune {
	m := map[rune]rune{'đ': 'd', 'Đ': 'D'}
	for base, letters := range map[rune]string{
		'a': "àáạảãâầấậẩẫăằắặẳẵ", 'e': "èéẹẻẽêềếệểễ", 'i': "ìíịỉĩ",
		'o': "òóọỏõôồốộổỗơờớợởỡ", 'u': "ùúụủũưừứựửữ", 'y': "ỳýỵỷỹ",
	} {
		for _, r := range letters {
			m[r] = base
			m[unicode.ToUpper(r)] = unicode.ToUpper(base)
		}
	}
	return m
}()

// Fold bỏ dấu tiếng Việt (Đ→D, ă/â/ê/ô/ơ/ư và mọi dấu thanh; cả dấu kết hợp U+0300–U+036F đứng rời).
func Fold(s string) string { return fold(s) }

func fold(s string) string {
	return strings.Map(func(r rune) rune {
		if f, ok := vietFold[r]; ok {
			return f
		}
		if r >= 0x300 && r <= 0x36F {
			return -1
		}
		return r
	}, s)
}

// Trường mã bảo mật của thẻ: tên đúng nguyên (không phân biệt hoa thường), không khớp tên dài hơn chứa nó.
const securityFields = `cvv|cvc|cvn|card_cvv|security_code`

var (
	jsonSecurity = regexp.MustCompile(`(?i)("(?:` + securityFields + `)"\s*:\s*)("(?:[^"\\]|\\.)*"|-?[0-9]+)`)
	formSecurity = regexp.MustCompile(`(?i)(^|[&?;\s])((?:` + securityFields + `)=)[^&;\s]*`)
	xmlSecurity  = regexp.MustCompile(`(?i)(<(?:` + securityFields + `)>)[^<]*(</(?:` + securityFields + `)>)`)
)

// Cards che số thẻ và giá trị mã bảo mật. Luật số thẻ: 13–19 chữ số liền nhau hoặc chia nhóm bởi MỘT dấu cách
// hay gạch, không dính chữ/số/"_" ở hai đầu (không nằm trong một dãy chữ-số dài hơn), qua Luhn, và đầu số 2–6
// (Visa, Mastercard, Amex, JCB, Discover, UnionPay...) hoặc 9704 (thẻ nội địa). Che giữ 6 số đầu và 4 số cuối,
// dấu phân cách giữ nguyên.
func Cards(s string) string {
	s = maskPANs(s)
	s = jsonSecurity.ReplaceAllString(s, `${1}"`+Mask+`"`)
	s = formSecurity.ReplaceAllString(s, `${1}${2}`+Mask)
	return xmlSecurity.ReplaceAllString(s, `${1}`+Mask+`${2}`)
}

func maskPANs(s string) string {
	var out []byte
	for i := 0; i < len(s); {
		if !isDigit(s[i]) {
			i++
			continue
		}
		// Một dãy: chữ số, và dấu cách/gạch đơn lẻ nằm giữa hai chữ số.
		j, digits := i, make([]byte, 0, 24)
		for j < len(s) {
			if isDigit(s[j]) {
				digits = append(digits, s[j])
				j++
				continue
			}
			if (s[j] == ' ' || s[j] == '-') && j+1 < len(s) && isDigit(s[j+1]) {
				j++
				continue
			}
			break
		}
		before := i == 0 || !isWordByte(s, i, true)
		after := j == len(s) || !isWordByte(s, j, false)
		if n := len(digits); before && after && n >= 13 && n <= 19 && cardPrefix(digits) && luhn(string(digits)) {
			if out == nil {
				out = []byte(s)
			}
			k := 0
			for p := i; p < j; p++ {
				if isDigit(s[p]) {
					if k >= 6 && k < n-4 {
						out[p] = '*'
					}
					k++
				}
			}
		}
		i = j
	}
	if out == nil {
		return s
	}
	return string(out)
}

// isWordByte: ký tự ngay trước (before) hoặc tại vị trí i là chữ, số hay "_".
func isWordByte(s string, i int, before bool) bool {
	var r rune
	if before {
		r, _ = utf8.DecodeLastRuneInString(s[:i])
	} else {
		r, _ = utf8.DecodeRuneInString(s[i:])
	}
	return isWordRune(r)
}

func cardPrefix(d []byte) bool {
	return (d[0] >= '2' && d[0] <= '6') || bytes.HasPrefix(d, []byte("9704"))
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
