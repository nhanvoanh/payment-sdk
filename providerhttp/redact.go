package providerhttp

import "bitbucket.org/hasaki-tech/payment-connector-sdk/redact"

// Redact che dữ liệu của khách trong một thân (request, response hay câu lỗi) trước khi connector đặt nó
// vào connector.RawExchange: mọi dạng (thô, JSON-escape, URL-encode, không phân biệt hoa thường) của từng
// giá trị trong values (tên, email, điện thoại, địa chỉ... của khách trong lời gọi) thành "[REDACTED]", rồi
// che số thẻ và mã bảo mật thẻ. Giá trị ngắn hơn 4 ký tự bị bỏ qua. Cùng thuật toán với core (gói redact).
func Redact(body string, values ...string) string { return redact.String(body, values...) }
