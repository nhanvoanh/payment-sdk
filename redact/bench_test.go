package redact_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/nhanvoanh/payment-sdk/redact"
)

var benchPII = redact.Customer("Nguyễn Thị Minh Khai", "nguyen.khai+shop@example.com", "0987654321",
	"12 Đường Lê Lợi\nPhường Bến Nghé, Quận 1\nThành phố Hồ Chí Minh")

// benchBody là thân JSON điển hình của đối tác (n byte), có nhắc lại khách vài lần và một số thẻ.
func benchBody(n int) string {
	var b strings.Builder
	b.WriteString(`{"customer":{"name":"Nguyễn Thị Minh Khai","holder":"NGUYEN THI MINH KHAI","email":"nguyen.khai+shop@example.com",` +
		`"phone":"+84 987 654 321","address":"12 Đường Lê Lợi\nPhường Bến Nghé"},"pan":"4111111111111111","items":[`)
	for i := 0; b.Len() < n-64; i++ {
		fmt.Fprintf(&b, `{"sku":"SKU-%06d","name":"Sản phẩm thử nghiệm số %d","qty":%d,"price":%d,"note":"giao hàng tiêu chuẩn"},`, i, i, i%5+1, 10000+i*37)
	}
	b.WriteString(`{}]}`)
	return b.String()
}

func BenchmarkNewPII(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = redact.NewPII(benchPII)
	}
}

func benchString(b *testing.B, n int) {
	body := benchBody(n)
	r := redact.NewPII(benchPII)
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = r.String(body)
	}
}

func BenchmarkString4KiB(b *testing.B)  { benchString(b, 4<<10) }
func BenchmarkString64KiB(b *testing.B) { benchString(b, 64<<10) }
