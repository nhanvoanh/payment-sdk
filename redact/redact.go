// Package redact che dữ liệu cá nhân của khách và dữ liệu thẻ trong văn bản sắp được lưu vết hay ghi log.
// Connector (qua providerhttp.RedactCustomer/RedactValues) và core dùng CHUNG thuật toán này. Chỉ dùng thư viện chuẩn.
//
// Cách làm: thân được CHUẨN HOÁ một lần (giải mã \uXXXX kể cả lồng hai lần, \n \t \/ \", %HH, "+" thành dấu cách;
// bỏ dấu tiếng Việt; chữ thường; dấu cách, xuống dòng, dấu phẩy, " - " gộp thành một dấu cách), kèm bảng ánh xạ
// từng byte chuẩn hoá về byte gốc. Mọi so khớp chạy trên bản chuẩn hoá (giá trị cũng được chuẩn hoá cùng cách), nên
// một mẫu bắt được mọi dạng mã hoá, hoa thường và có/không dấu. Các khoảng khớp được gộp (hợp) rồi thay trên thân gốc.
//
// Luật (PII):
//   - Giá trị trọn — tên đầy đủ (≥ 2 chữ), email, địa chỉ nguyên vẹn (các dòng nối kiểu nào cũng được), giá trị khác
//     ≥ 4 ký tự: so CHUỖI CON.
//   - Thành phần địa chỉ (tách theo dòng, "," và " - "): che riêng, trọn từ, khi dài ≥ 12 ký tự hoặc có chữ số.
//   - Điện thoại (giá trị chỉ gồm chữ số và + - . ( ) dấu cách, 8–15 chữ số): theo nhóm chữ số, dấu phân cách
//     ". ( ) -" và dấu cách xen giữa, đầu 0 / 84 / +84 / 0084 / (+84) / +84(0); không bao giờ cắt giữa một nhóm số.
//   - Chữ của tên: chữ MẠNH (có dấu, ≥ 2 ký tự) bị che khi đứng thành từ riêng ở dạng CÓ DẤU. Chữ YẾU (dạng bỏ dấu
//     của chữ mạnh, chữ vốn không dấu) chỉ bị che khi (a1) đứng liền một chữ khác của tên (cách bởi dấu cách, "+",
//     "%20", ",", "-", ".", "_"), hoặc (a2) là toàn bộ giá trị của một trường có tên gợi ý người (JSON "k":"v",
//     form k=v, XML <k>v</k>) và mọi chữ của giá trị là chữ của tên khách. Trường có tên gợi ý địa chỉ mà giá trị là
//     một thành phần địa chỉ của khách cũng bị che.
//   - Thẻ (Cards): 13–19 chữ số (liền hoặc nhóm cách bởi MỘT dấu cách/gạch), không dính chữ/số/"_", qua Luhn, và
//     khớp dải đầu số × độ dài của các mạng thẻ phổ biến; mã đã biết của lời gọi (PII.Keep) không bao giờ là thẻ.
//     Giá trị của trường cvv/cvc/cvn/card_cvv/security_code (JSON, form, XML) thành Mask.
//
// Giới hạn: xem README của conformance (mục LOG-1) và báo cáo Task 6.
package redact

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Mask là chuỗi thay cho giá trị bị che.
const Mask = "[REDACTED]"

// MinValueRunes: giá trị trọn ngắn hơn (sau khi cắt khoảng trắng) bị bỏ qua để không phá nát thân.
const MinValueRunes = 4

// MinWordRunes: chữ của tên ngắn hơn bị bỏ qua.
const MinWordRunes = 2

// MinLineRunes: thành phần địa chỉ không có chữ số chỉ được che riêng khi dài ít nhất chừng này ("Hà Nội" thì không).
const MinLineRunes = 12

// PII là dữ liệu cá nhân cần che.
type PII struct {
	Names     []string // tên người: giá trị trọn (khi ≥ 2 chữ) và từng chữ (mạnh/yếu)
	Addresses []string // địa chỉ: giá trị trọn và các thành phần
	Values    []string // giá trị trọn khác (email, điện thoại...); giá trị nhiều dòng được coi như địa chỉ
	Keep      []string // mã đã biết của lời gọi (mã giao dịch, mã đối tác, khoá idempotency...): không bao giờ là thẻ
}

// Customer dựng PII của một khách.
func Customer(name, email, phone, address string) PII {
	return PII{Names: []string{name}, Values: []string{email, phone}, Addresses: []string{address}}
}

// String che mọi lớp với values là giá trị trọn.
func String(s string, values ...string) string { return New(values...).String(s) }

// Values chỉ che giá trị trọn (không che thẻ). Byte không phải UTF-8 ngoài chỗ khớp giữ nguyên.
func Values(s string, values ...string) string { return New(values...).values(s) }

// Contains báo s còn chứa một dạng nào của values.
func Contains(s string, values ...string) bool { return New(values...).Contains(s) }

type wordInfo struct{ strong string } // strong: dạng có dấu (chữ thường) khi chữ là chữ mạnh, rỗng nếu yếu

// Redactor là bộ che đã dựng sẵn; dựng một lần cho mỗi lệnh rồi dùng cho mọi văn bản của lệnh đó. nil chỉ che thẻ.
// An toàn khi dùng đồng thời; không bao giờ panic vì đầu vào.
type Redactor struct {
	full   []string            // chuỗi con
	comps  []string            // thành phần địa chỉ, trọn từ
	addr   map[string]bool     // thành phần địa chỉ (cho trường gợi ý địa chỉ)
	words  map[string]wordInfo // chữ của tên (đã chuẩn hoá, bỏ dấu)
	phones map[string]bool     // dãy chữ số của điện thoại ở mọi đầu số
	maxPh  int
	keep   []string
}

// New dựng Redactor với values là giá trị trọn.
func New(values ...string) *Redactor { return NewPII(PII{Values: values}) }

// NewPII dựng Redactor cho p.
func NewPII(p PII) *Redactor {
	r := &Redactor{}
	seen := map[string]bool{}
	addFull := func(v string) {
		if utf8.RuneCountInString(v) < MinValueRunes {
			return
		}
		n := normString(v, true)
		if n != "" && !seen[n] {
			seen[n] = true
			r.full = append(r.full, n)
		}
	}
	addAddress := func(a string) {
		addFull(a)
		for _, c := range components(a) {
			n := normString(c, true)
			if utf8.RuneCountInString(n) < MinWordRunes {
				continue
			}
			if r.addr == nil {
				r.addr = map[string]bool{}
			}
			r.addr[n] = true
			if utf8.RuneCountInString(c) >= MinValueRunes && (utf8.RuneCountInString(c) >= MinLineRunes || hasDigit(c)) {
				r.comps = append(r.comps, n)
			}
		}
	}
	for _, v := range p.Values {
		v = strings.TrimSpace(v)
		switch d, ok := phoneDigits(v); {
		case ok:
			r.addPhone(d)
		case strings.ContainsAny(v, "\r\n"):
			addAddress(v)
		default:
			addFull(v)
		}
	}
	for _, a := range p.Addresses {
		addAddress(strings.TrimSpace(a))
	}
	for _, name := range p.Names {
		f := strings.Fields(name)
		if len(f) > 1 {
			addFull(strings.Join(f, " "))
		}
		for _, w := range f {
			fw := normString(w, true)
			if utf8.RuneCountInString(fw) < MinWordRunes || strings.ContainsRune(fw, ' ') {
				continue
			}
			if r.words == nil {
				r.words = map[string]wordInfo{}
			}
			info := r.words[fw]
			if lw := normString(w, false); lw != fw && info.strong == "" {
				info.strong = lw
			}
			r.words[fw] = info
		}
	}
	for _, k := range p.Keep {
		if len(k) >= 6 {
			r.keep = append(r.keep, k)
		}
	}
	return r
}

// WithKeep trả bản sao của r có thêm các mã không bao giờ bị coi là số thẻ (r nil: chỉ có mã).
func (r *Redactor) WithKeep(codes ...string) *Redactor {
	out := &Redactor{}
	if r != nil {
		*out = *r
		out.keep = append([]string(nil), r.keep...)
	}
	for _, k := range codes {
		if len(k) >= 6 {
			out.keep = append(out.keep, k)
		}
	}
	return out
}

// String che dữ liệu khách của r rồi che thẻ.
func (r *Redactor) String(s string) string {
	var keep []string
	if r != nil {
		keep = r.keep
	}
	return cards(r.values(s), keep)
}

// Contains báo s còn chứa một dạng nào của dữ liệu khách của r (không xét thẻ).
func (r *Redactor) Contains(s string) bool {
	if r.empty() || s == "" {
		return false
	}
	return len(r.spans(s)) > 0
}

func (r *Redactor) empty() bool {
	return r == nil || (len(r.full) == 0 && len(r.comps) == 0 && len(r.words) == 0 && len(r.phones) == 0 && len(r.addr) == 0)
}

func (r *Redactor) values(s string) string {
	if r.empty() || s == "" {
		return s
	}
	sp := r.spans(s)
	if len(sp) == 0 {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	last := 0
	for _, x := range sp {
		b.WriteString(s[last:x[0]])
		b.WriteString(Mask)
		last = x[1]
	}
	b.WriteString(s[last:])
	return b.String()
}

// spans trả các khoảng (byte của s) cần che, đã gộp.
func (r *Redactor) spans(s string) [][2]int {
	n := normalize(s, true)
	t := string(n.b)
	var out [][2]int
	add := func(i, j int) {
		if j > i {
			out = append(out, [2]int{int(n.pos[i]), int(n.pos[j])})
		}
	}
	for _, p := range r.full {
		for k := 0; k <= len(t)-len(p); {
			x := strings.Index(t[k:], p)
			if x < 0 {
				break
			}
			add(k+x, k+x+len(p))
			k += x + 1
		}
	}
	for _, p := range r.comps {
		for k := 0; k <= len(t)-len(p); {
			x := strings.Index(t[k:], p)
			if x < 0 {
				break
			}
			if i, j := k+x, k+x+len(p); (i == 0 || !isWordByte(t[i-1])) && (j == len(t) || !isWordByte(t[j])) {
				add(i, j)
			}
			k += x + 1
		}
	}
	if len(r.phones) > 0 {
		r.phoneSpans(t, add)
	}
	if len(r.words) > 0 {
		r.wordSpans(s, n, t, add)
	}
	if len(r.words) > 0 || len(r.addr) > 0 {
		r.fieldSpans(s, n, t, add)
	}
	return merge(out)
}

func merge(out [][2]int) [][2]int {
	if len(out) < 2 {
		return out
	}
	// Sắp xếp chèn: số khoảng thường rất nhỏ.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j][0] < out[j-1][0]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	m := out[:1]
	for _, x := range out[1:] {
		if l := &m[len(m)-1]; x[0] <= l[1] {
			l[1] = max(l[1], x[1])
			continue
		}
		m = append(m, x)
	}
	return m
}

// ---- Chữ của tên ----

type token struct{ i, j int }

// wordSpans: chữ mạnh đứng riêng (dạng có dấu) và chuỗi ≥ 2 chữ của tên đứng liền nhau (luật a1).
func (r *Redactor) wordSpans(s string, n ntext, t string, add func(i, j int)) {
	var run []token
	flush := func() {
		if len(run) >= 2 {
			add(run[0].i, run[len(run)-1].j)
		}
		run = run[:0]
	}
	prevEnd := -1
	for i := 0; i < len(t); {
		if !isWordByte(t[i]) {
			i++
			continue
		}
		j := i
		for j < len(t) && isWordByte(t[j]) {
			j++
		}
		info, isName := r.words[t[i:j]]
		switch {
		case !isName:
			flush()
		default:
			if info.strong != "" && normString(s[n.pos[i]:n.pos[j]], false) == info.strong {
				add(i, j)
			}
			if len(run) > 0 && !joinable(t[prevEnd:i]) {
				flush()
			}
			run = append(run, token{i, j})
		}
		prevEnd, i = j, j
	}
	flush()
}

// joinable: khoảng giữa hai chữ chỉ gồm dấu cách, "-", ".", "_" (đã chuẩn hoá: "+", "%20", "," thành dấu cách), tối đa 3.
func joinable(gap string) bool {
	if gap == "" || len(gap) > 3 {
		return false
	}
	for i := 0; i < len(gap); i++ {
		switch gap[i] {
		case ' ', '-', '.', '_':
		default:
			return false
		}
	}
	return true
}

// ---- Trường gợi ý người / địa chỉ (luật a2) ----

var personKeys = setOf("name", "fullname", "full", "first", "last", "middle", "given", "family", "surname", "holder",
	"cardholder", "account", "accountname", "ho", "ten", "dem", "hoten", "customer", "payer", "buyer", "receiver",
	"recipient", "beneficiary", "contact", "owner", "billing", "shipping", "user", "username")

var addressKeys = setOf("address", "addr", "street", "ward", "district", "city", "province", "phuong", "quan", "huyen",
	"tinh", "diachi")

func setOf(ws ...string) map[string]bool {
	m := make(map[string]bool, len(ws))
	for _, w := range ws {
		m[w] = true
	}
	return m
}

const maxKey, maxFieldValue = 64, 256

// fieldSpans tìm các trường JSON "k":"v", form k=v và XML <k>v</k> trên bản chuẩn hoá.
func (r *Redactor) fieldSpans(s string, n ntext, t string, add func(i, j int)) {
	for i := 0; i < len(t); i++ {
		switch t[i] {
		case ':':
			j := i - 1
			for j >= 0 && t[j] == ' ' {
				j--
			}
			if j < 1 || t[j] != '"' {
				continue
			}
			ks := strings.LastIndexByte(t[max(0, j-maxKey):j], '"')
			if ks < 0 {
				continue
			}
			ks += max(0, j-maxKey) + 1
			m := i + 1
			for m < len(t) && t[m] == ' ' {
				m++
			}
			if m >= len(t) || t[m] != '"' {
				continue
			}
			ve := strings.IndexByte(t[m+1:min(len(t), m+1+maxFieldValue)], '"')
			if ve < 0 {
				continue
			}
			r.field(s, n, t, ks, j, m+1, m+1+ve, add)
		case '=':
			ks := i
			for ks > 0 && i-ks < maxKey && !strings.ContainsRune("&?; \"'<>", rune(t[ks-1])) {
				ks--
			}
			ve := i + 1
			for ve < len(t) && ve-i <= maxFieldValue && !strings.ContainsRune("&;\"<", rune(t[ve])) {
				ve++
			}
			r.field(s, n, t, ks, i, i+1, ve, add)
		case '<':
			if i+1 >= len(t) || t[i+1] == '/' {
				continue
			}
			ke := strings.IndexByte(t[i+1:min(len(t), i+1+maxKey)], '>')
			if ke < 0 {
				continue
			}
			ke += i + 1
			vs := ke + 1
			ve := strings.IndexByte(t[vs:min(len(t), vs+maxFieldValue)], '<')
			if ve < 0 || vs+ve+1 >= len(t) || t[vs+ve+1] != '/' {
				continue
			}
			r.field(s, n, t, i+1, ke, vs, vs+ve, add)
		}
	}
}

// field che giá trị [vs,ve) khi khoá [ks,ke) gợi ý người (mọi chữ của giá trị là chữ của tên) hoặc địa chỉ (giá trị
// là một thành phần địa chỉ của khách).
func (r *Redactor) field(s string, n ntext, t string, ks, ke, vs, ve int, add func(i, j int)) {
	for vs < ve && t[vs] == ' ' {
		vs++
	}
	for ve > vs && t[ve-1] == ' ' {
		ve--
	}
	if ve <= vs || ke <= ks {
		return
	}
	person, address := keyKind(s[n.pos[ks]:n.pos[ke]])
	v := t[vs:ve]
	switch {
	case person && len(r.words) > 0 && allNameWords(v, r.words):
		add(vs, ve)
	case address && r.addr[v]:
		add(vs, ve)
	}
}

func allNameWords(v string, words map[string]wordInfo) bool {
	found := false
	for _, w := range strings.FieldsFunc(v, func(c rune) bool { return c == ' ' || c == '-' || c == '.' || c == '_' }) {
		if _, ok := words[w]; !ok {
			return false
		}
		found = true
	}
	return found
}

// keyKind tách khoá theo "_", "-", ".", camelCase (trên khoá gốc) và so với danh sách khoá gợi ý người/địa chỉ.
func keyKind(key string) (person, address bool) {
	var whole strings.Builder
	check := func(w string) {
		w = strings.ToLower(w)
		person = person || personKeys[w]
		address = address || addressKeys[w]
	}
	start := 0
	runes := []rune(key)
	for i, c := range runes {
		if c < utf8.RuneSelf && (unicode.IsLetter(c) || unicode.IsDigit(c)) {
			whole.WriteRune(unicode.ToLower(c))
		}
		switch {
		case !(unicode.IsLetter(c) || unicode.IsDigit(c)):
			if i > start {
				check(string(runes[start:i]))
			}
			start = i + 1
		case unicode.IsUpper(c) && i > start && unicode.IsLower(runes[i-1]):
			check(string(runes[start:i]))
			start = i
		}
	}
	if len(runes) > start {
		check(string(runes[start:]))
	}
	check(whole.String())
	return person, address
}

// ---- Điện thoại ----

func (r *Redactor) addPhone(d string) {
	if r.phones == nil {
		r.phones = map[string]bool{}
	}
	add := func(x string) {
		r.phones[x] = true
		r.maxPh = max(r.maxPh, len(x))
	}
	nat := ""
	switch {
	case strings.HasPrefix(d, "84") && len(d) >= 10:
		nat = d[2:]
	case strings.HasPrefix(d, "0") && len(d) >= 9:
		nat = d[1:]
	default:
		add(d)
		return
	}
	nat = strings.TrimPrefix(nat, "0")
	for _, p := range []string{"", "0", "84", "840", "0084", "00840"} {
		add(p + nat)
	}
}

// phoneSpans: ghép các nhóm chữ số liền nhau (cách bởi tối đa 4 ký tự trong ". ( ) -" và dấu cách), bắt đầu và kết
// thúc ở ranh giới nhóm, rồi so với các dãy của điện thoại.
func (r *Redactor) phoneSpans(t string, add func(i, j int)) {
	type group struct{ i, j int }
	var groups []group
	for i := 0; i < len(t); {
		if !isDigit(t[i]) {
			i++
			continue
		}
		j := i
		for j < len(t) && isDigit(t[j]) {
			j++
		}
		groups = append(groups, group{i, j})
		i = j
	}
	var buf []byte
	for a := range groups {
		buf = buf[:0]
		for b := a; b < len(groups); b++ {
			if b > a && !phoneGap(t[groups[b-1].j:groups[b].i]) {
				break
			}
			buf = append(buf, t[groups[b].i:groups[b].j]...)
			if len(buf) > r.maxPh {
				break
			}
			if r.phones[string(buf)] {
				add(groups[a].i, groups[b].j)
			}
		}
	}
}

func phoneGap(g string) bool {
	if g == "" || len(g) > 4 {
		return false
	}
	for i := 0; i < len(g); i++ {
		if !strings.ContainsRune(" .()-", rune(g[i])) {
			return false
		}
	}
	return true
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

// ---- Địa chỉ ----

// components tách địa chỉ theo dòng, rồi mỗi dòng theo "," và " - ".
func components(a string) []string {
	var out []string
	for _, line := range strings.FieldsFunc(a, func(c rune) bool { return c == '\n' || c == '\r' }) {
		for _, part := range strings.Split(line, ",") {
			for _, c := range strings.Split(part, " - ") {
				if c = strings.TrimSpace(c); c != "" {
					out = append(out, c)
				}
			}
		}
	}
	return out
}

func hasDigit(s string) bool { return strings.ContainsAny(s, "0123456789") }

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// isWordByte (trên bản chuẩn hoá): chữ/số ASCII hoặc byte của ký tự ngoài ASCII.
func isWordByte(c byte) bool {
	return c >= utf8.RuneSelf || isDigit(c) || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}
