package providerhttp

import (
	"github.com/nhanvoanh/payment-sdk/paykit"
	"github.com/nhanvoanh/payment-sdk/redact"
)

// Hai lớp che, theo thứ tự áp dụng:
//  1. Config.Redact (hàm của client, không biết khách nào): Do áp nó lên thân request/response TRƯỚC khi đặt
//     vào Response.Raw / Error.Raw. Dùng cho thứ cố định như token, chữ ký.
//  2. RedactCustomer / RedactValues (theo từng lời gọi): connector áp lên Raw SAU khi Do trả về, với khách của
//     lời gọi, trước khi đặt vào connector.RawExchange (luật LOG-1).

// RedactCustomer che dữ liệu của khách c trong body theo luật của gói redact (cùng luật core dùng và LOG-1 dò): tên
// đầy đủ, email, điện thoại, địa chỉ ở mọi dạng (thô, JSON-escape, URL-encode, khác hoa thường, bỏ dấu); thành phần
// địa chỉ; chữ của tên (chữ có dấu đứng riêng; chữ bỏ dấu khi liền chữ khác của tên hoặc là cả giá trị của trường
// gợi ý người như "last", "holder"); rồi số thẻ và mã bảo mật thẻ. Che bằng nó thì qua LOG-1. Che nhiều thân của
// cùng khách thì dựng redact.NewPII(redact.Customer(...)) một lần rồi gọi String cho từng thân.
func RedactCustomer(body string, c paykit.Customer) string {
	return redact.NewPII(redact.Customer(c.Name, c.Email, c.Phone, c.Address)).String(body)
}

// RedactValues như RedactCustomer với các giá trị trọn cho sẵn (so chuỗi con; không tách chữ). Dùng khi
// connector chỉ biết giá trị từ phản hồi của đối tác, hoặc đã tự tách họ/tên: truyền cả các phần đã tách.
func RedactValues(body string, values ...string) string { return redact.String(body, values...) }

// Redact là tên cũ của RedactValues.
//
// Deprecated: dùng RedactValues (giá trị cho sẵn) hoặc RedactCustomer (cả khách); tên này dễ nhầm với Config.Redact.
func Redact(body string, values ...string) string { return RedactValues(body, values...) }
