package redact_test

import (
	"sort"
	"strings"
	"testing"

	"github.com/nhanvoanh/payment-sdk/redact"
)

// designKind: loại mà theo thiết kế KHÔNG được che (chữ yếu của tên đứng riêng; thành phần địa chỉ ngắn đứng riêng
// ngoài trường gợi ý địa chỉ). Ở thân văn bản không có khoá nên chữ bỏ dấu tách riêng và thành phần ngắn cũng vậy.
func designKind(kind string) bool {
	enc, k, _ := strings.Cut(kind, "/")
	switch k {
	case "weak_standalone":
		return true
	case "name_split_folded", "address_component_field", "address_line_short":
		return enc == "text"
	}
	return false
}

// Phép thử "phải che" thường trú: vài nghìn thân ngẫu nhiên (hạt giống cố định; JSON, \u, form, XML, văn bản, JSON lồng
// trong JSON, JSON lồng trong form), mọi giá trị khách thuộc loại phải che đều không còn ở dạng đã mã hoá trong đầu ra.
func TestDifferential_MustMask(t *testing.T) {
	leaks := map[string]int{}
	design := map[string]int{}
	var sample []string
	for _, gb := range genBodies(20261120, 4000) {
		c := gb.Customer
		out := redact.NewPII(redact.Customer(c.Name, c.Email, c.Phone, c.Address)).String(gb.Body)
		for _, s := range gb.Secrets {
			if !strings.Contains(out, s.Encoded) {
				continue
			}
			if designKind(s.Kind) {
				design[s.Kind]++
				continue
			}
			leaks[s.Kind]++
			if len(sample) < 5 {
				sample = append(sample, s.Kind+": "+s.Encoded+"\n    thân: "+gb.Body+"\n    ra:   "+out)
			}
		}
	}
	if len(leaks) > 0 {
		var ks []string
		for k, n := range leaks {
			ks = append(ks, k+"="+itoa(n))
		}
		sort.Strings(ks)
		t.Errorf("lọt: %s\n%s", strings.Join(ks, " "), strings.Join(sample, "\n"))
	}
	if testing.Verbose() {
		t.Logf("không che theo thiết kế: %v", design)
	}
}

func itoa(n int) string {
	var b []byte
	for n >= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(append([]byte{byte('0' + n)}, b...))
}
