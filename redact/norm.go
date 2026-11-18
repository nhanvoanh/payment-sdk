package redact

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// ntext là văn bản đã chuẩn hoá: b[i] đến từ byte gốc pos[i]; pos có thêm phần tử cuối = len(gốc), nên khoảng
// chuẩn hoá [i, j) ứng với khoảng gốc [pos[i], pos[j]).
type ntext struct {
	b   []byte
	pos []int32
}

func normString(s string, fold bool) string {
	return strings.TrimSpace(string(normalize(s, fold).b))
}

// normalize giải mã \uXXXX (một hoặc hai dấu "\", kể cả cặp thay thế), \n \r \t \/ \" và %HH (nhiều byte UTF-8);
// "+" , dấu cách, xuống dòng, "," và " - " thành MỘT dấu cách; chữ thường; fold: bỏ dấu tiếng Việt và dấu kết hợp.
// Byte không phải UTF-8 được giữ nguyên. Không bao giờ panic.
func normalize(s string, fold bool) ntext {
	n := ntext{b: make([]byte, 0, len(s)), pos: make([]int32, 0, len(s)+1)}
	for i := 0; i < len(s); {
		c := s[i]
		switch c {
		case '\\':
			if r, end, ok := backslash(s, i); ok {
				n.rune(r, i, fold)
				i = end
				continue
			}
		case '%':
			if end, ok := n.percent(s, i, fold); ok {
				i = end
				continue
			}
		case '-':
			if len(n.b) > 0 && n.b[len(n.b)-1] == ' ' && i+1 < len(s) && s[i+1] == ' ' {
				n.sep(i)
				i++
				continue
			}
		}
		if c < utf8.RuneSelf {
			n.ascii(c, i)
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			n.raw(c, i)
			i++
			continue
		}
		n.rune(r, i, fold)
		i += size
	}
	n.pos = append(n.pos, int32(len(s)))
	return n
}

func (n *ntext) raw(c byte, at int) {
	n.b = append(n.b, c)
	n.pos = append(n.pos, int32(at))
}

func (n *ntext) sep(at int) {
	if len(n.b) > 0 && n.b[len(n.b)-1] == ' ' {
		return
	}
	n.raw(' ', at)
}

func (n *ntext) ascii(c byte, at int) {
	switch {
	case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',' || c == '+':
		n.sep(at)
	case c >= 'A' && c <= 'Z':
		n.raw(c+'a'-'A', at)
	default:
		n.raw(c, at)
	}
}

func (n *ntext) rune(r rune, at int, fold bool) {
	if r < utf8.RuneSelf {
		n.ascii(byte(r), at)
		return
	}
	if unicode.IsSpace(r) {
		n.sep(at)
		return
	}
	if fold {
		if f, ok := vietFold[r]; ok {
			n.ascii(byte(f), at)
			return
		}
		if r >= 0x300 && r <= 0x36F {
			return
		}
	}
	r = unicode.ToLower(r)
	var buf [utf8.UTFMax]byte
	for _, c := range buf[:utf8.EncodeRune(buf[:], r)] {
		n.raw(c, at)
	}
}

// percent giải mã một chuỗi %HH liền nhau như các byte UTF-8; mỗi ký tự trỏ về "%" của byte đầu của nó.
func (n *ntext) percent(s string, i int, fold bool) (int, bool) {
	var buf [64]byte
	var offs [64]int
	k, j := 0, i
	for k < len(buf) && j+2 < len(s) && s[j] == '%' && isHex(s[j+1]) && isHex(s[j+2]) {
		buf[k], offs[k] = unhex(s[j+1])<<4|unhex(s[j+2]), j
		k++
		j += 3
	}
	if k == 0 {
		return i, false
	}
	for p := 0; p < k; {
		r, size := utf8.DecodeRune(buf[p:k])
		switch {
		case r == utf8.RuneError && size <= 1:
			n.raw(buf[p], offs[p])
			size = 1
		default:
			n.rune(r, offs[p], fold)
		}
		p += size
	}
	return j, true
}

// backslash giải mã một chuỗi thoát JSON bắt đầu ở i (một hoặc hai dấu "\" — JSON lồng hai lần).
func backslash(s string, i int) (rune, int, bool) {
	k := 1
	if i+1 < len(s) && s[i+1] == '\\' {
		k = 2
	}
	j := i + k
	if j >= len(s) {
		return 0, 0, false
	}
	switch s[j] {
	case 'u', 'U':
		r, ok := hex4(s, j+1)
		if !ok {
			return 0, 0, false
		}
		end := j + 5
		if r >= 0xD800 && r <= 0xDBFF && end+k+5 <= len(s) && strings.Repeat(`\`, k) == s[end:end+k] && (s[end+k] == 'u' || s[end+k] == 'U') {
			if lo, ok := hex4(s, end+k+1); ok && lo >= 0xDC00 && lo <= 0xDFFF {
				return 0x10000 + (r-0xD800)<<10 + (lo - 0xDC00), end + k + 5, true
			}
		}
		return r, end, true
	case 'n', 'r', 't':
		return ' ', j + 1, true
	case '/':
		return '/', j + 1, true
	case '"':
		return '"', j + 1, true
	}
	if k == 2 {
		return '\\', i + 2, true
	}
	return 0, 0, false
}

func hex4(s string, i int) (rune, bool) {
	if i+4 > len(s) {
		return 0, false
	}
	var r rune
	for _, c := range []byte(s[i : i+4]) {
		if !isHex(c) {
			return 0, false
		}
		r = r<<4 | rune(unhex(c))
	}
	return r, true
}

func isHex(c byte) bool { return isDigit(c) || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F') }

func unhex(c byte) byte {
	switch {
	case isDigit(c):
		return c - '0'
	case c >= 'a':
		return c - 'a' + 10
	}
	return c - 'A' + 10
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
func Fold(s string) string {
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
