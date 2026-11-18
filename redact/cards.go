package redact

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Trường mã bảo mật của thẻ: tên đúng nguyên (không phân biệt hoa thường), không khớp tên dài hơn chứa nó.
const securityFields = `cvv|cvc|cvn|card_cvv|security_code`

var (
	jsonSecurity = regexp.MustCompile(`(?i)("(?:` + securityFields + `)"\s*:\s*)("(?:[^"\\]|\\.)*"|-?[0-9]+)`)
	formSecurity = regexp.MustCompile(`(?i)(^|[&?;\s])((?:` + securityFields + `)=)[^&;\s]*`)
	xmlSecurity  = regexp.MustCompile(`(?i)(<(?:` + securityFields + `)>)[^<]*(</(?:` + securityFields + `)>)`)
)

// Cards che số thẻ và giá trị mã bảo mật (không cần giá trị biết trước). Luật số thẻ: 13–19 chữ số liền nhau hoặc
// chia nhóm bởi MỘT dấu cách hay gạch, không dính chữ/số/"_" ở hai đầu, qua Luhn, và khớp dải đầu số × độ dài:
// Visa 4 (13/16/19), Mastercard 51–55 và 2221–2720 (16), Amex 34/37 (15), JCB 3528–3589 (16), UnionPay 62 (16–19),
// Discover 6011/65 (16), thẻ nội địa 9704 (16/19). Che giữ 6 số đầu và 4 số cuối, dấu phân cách giữ nguyên.
func Cards(s string) string { return cards(s, nil) }

func cards(s string, keep []string) string {
	s = maskPANs(s, keep)
	if !containsSecurityField(s) {
		return s
	}
	s = jsonSecurity.ReplaceAllString(s, `${1}"`+Mask+`"`)
	s = formSecurity.ReplaceAllString(s, `${1}${2}`+Mask)
	return xmlSecurity.ReplaceAllString(s, `${1}`+Mask+`${2}`)
}

// containsSecurityField lọc nhanh trước khi chạy biểu thức: có "cvv", "cvc", "cvn" hay "security_code" (mọi hoa thường).
func containsSecurityField(s string) bool {
	for i := 0; i+3 <= len(s); i++ {
		if c := s[i] | 0x20; c != 'c' && c != 's' {
			continue
		}
		if a, b := s[i+1]|0x20, s[i+2]|0x20; s[i]|0x20 == 'c' && a == 'v' && (b == 'v' || b == 'c' || b == 'n') {
			return true
		}
		if i+13 <= len(s) && strings.EqualFold(s[i:i+13], "security_code") {
			return true
		}
	}
	return false
}

func maskPANs(s string, keep []string) string {
	var out []byte
	var kept [][2]int
	keepDone := false
	digits := make([]byte, 0, 24)
	for i := 0; i < len(s); {
		if !isDigit(s[i]) {
			i++
			continue
		}
		j := i
		digits = digits[:0]
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
		before := i == 0 || !isWordRuneAt(s, i, true)
		after := j == len(s) || !isWordRuneAt(s, j, false)
		if before && after && cardShape(digits) && luhn(digits) {
			if !keepDone {
				kept, keepDone = keepRanges(s, keep), true
			}
			if !overlaps(kept, i, j) {
				if out == nil {
					out = []byte(s)
				}
				k, n := 0, len(digits)
				for p := i; p < j; p++ {
					if isDigit(s[p]) {
						if k >= 6 && k < n-4 {
							out[p] = '*'
						}
						k++
					}
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

func keepRanges(s string, keep []string) [][2]int {
	var out [][2]int
	for _, k := range keep {
		for p := 0; p <= len(s)-len(k); {
			x := strings.Index(s[p:], k)
			if x < 0 {
				break
			}
			out = append(out, [2]int{p + x, p + x + len(k)})
			p += x + 1
		}
	}
	return out
}

func overlaps(rs [][2]int, i, j int) bool {
	for _, r := range rs {
		if r[0] < j && i < r[1] {
			return true
		}
	}
	return false
}

// cardShape: độ dài và đầu số khớp một mạng thẻ phổ biến.
func cardShape(d []byte) bool {
	n := len(d)
	if n < 13 || n > 19 {
		return false
	}
	p2 := int(d[0]-'0')*10 + int(d[1]-'0')
	p4 := p2*100 + int(d[2]-'0')*10 + int(d[3]-'0')
	switch {
	case d[0] == '4':
		return n == 13 || n == 16 || n == 19
	case p2 >= 51 && p2 <= 55, p4 >= 2221 && p4 <= 2720:
		return n == 16
	case p2 == 34 || p2 == 37:
		return n == 15
	case p4 >= 3528 && p4 <= 3589:
		return n == 16
	case p2 == 62:
		return n >= 16
	case p4 == 6011 || p2 == 65:
		return n == 16
	case p4 == 9704:
		return n == 16 || n == 19
	}
	return false
}

// isWordRuneAt: ký tự ngay trước (before) hoặc tại vị trí i là chữ, số hay "_".
func isWordRuneAt(s string, i int, before bool) bool {
	var r rune
	if before {
		r, _ = utf8.DecodeLastRuneInString(s[:i])
	} else {
		r, _ = utf8.DecodeRuneInString(s[i:])
	}
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
}

func luhn(d []byte) bool {
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
