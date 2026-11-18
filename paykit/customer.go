package paykit

// Customer là thông tin khách gửi sang đối tác. Mọi trường trừ ID là dữ liệu cá nhân: không được vào log hay
// RawExchange (luật LOG-1).
type Customer struct {
	ID, Name, Email, Phone string
	// Address là địa chỉ khách, một chuỗi tự do có thể nhiều dòng (\n), tối đa 500 ký tự, đã cắt khoảng trắng
	// hai đầu; rỗng là không có. Đối tác cần nó để chống gian lận; connector không cần thì bỏ qua.
	Address string
}

// CardMasked chỉ giữ thông tin thẻ đã che; không bao giờ chứa số thẻ đầy đủ.
type CardMasked struct {
	Brand, Last4, Holder string
}
