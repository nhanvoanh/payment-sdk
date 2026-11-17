package paykit

// Method là phương thức thanh toán mà khách chọn.
type Method string

const (
	MethodVisa       Method = "visa"
	MethodMastercard Method = "mastercard"
	MethodATMCard    Method = "atm_card"
	MethodQR         Method = "qr"
	MethodWallet     Method = "wallet"
	MethodPOS        Method = "pos"
	MethodBalance    Method = "balance"
)
