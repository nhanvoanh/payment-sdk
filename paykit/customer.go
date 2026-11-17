package paykit

// Customer là thông tin khách gửi sang đối tác.
type Customer struct {
	ID, Name, Email, Phone string
}

// CardMasked chỉ giữ thông tin thẻ đã che; không bao giờ chứa số thẻ đầy đủ.
type CardMasked struct {
	Brand, Last4, Holder string
}
