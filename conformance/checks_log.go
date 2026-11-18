package conformance

import (
	"time"

	"bitbucket.org/hasaki-tech/payment-connector-sdk/connector"
	"bitbucket.org/hasaki-tech/payment-connector-sdk/paykit"
	"bitbucket.org/hasaki-tech/payment-connector-sdk/redact"
)

// logCanary là khách mang chuỗi canh của LOG-1: tên có ký tự ngoài ASCII (để thử dạng \uXXXX), email có "+"
// (để thử URL-encode), điện thoại đầu 0 (để thử dạng 84/+84). Địa chỉ thêm ở đây khi request có trường địa chỉ.
var logCanary = paykit.Customer{ID: "C-LOG1", Name: "Đặng Canaryname", Email: "canary.mail+log1@example.com", Phone: "0987654321"}

func canaryValues() []string { return []string{logCanary.Name, logCanary.Email, logCanary.Phone} }

// expectNoCustomerData: RawExchange và câu lỗi không còn dạng nào của chuỗi canh (cùng thuật toán với core).
func (e *env) expectNoCustomerData(what string, raw connector.RawExchange, err error) {
	parts := []struct{ name, text string }{{"RawExchange.RequestBody", raw.RequestBody}, {"RawExchange.ResponseBody", raw.ResponseBody}}
	if err != nil {
		parts = append(parts, struct{ name, text string }{"câu lỗi", err.Error()})
	}
	for _, p := range parts {
		if redact.Contains(p.text, canaryValues()...) {
			e.fail("%s trả %s còn dữ liệu của khách (tên, email hay điện thoại; thô, JSON-escape, URL-encode hay khác hoa thường); cần che trước khi trả, ví dụ providerhttp.Redact(body, tên, email, điện thoại); vết lời gọi đối tác và log của core sẽ lưu dữ liệu cá nhân của khách", what, p.name)
		}
	}
}

// checkLOG1: mọi thao tác mang dữ liệu khách (tạo, charge) và truy vấn ngay sau đó (đối tác có thể nhắc lại
// khách) không được trả dữ liệu khách trong RawExchange. Hoàn tiền không mang dữ liệu khách nên không kiểm.
func checkLOG1(e *env) {
	e.p.SetMode(ModeNormal)
	tx := e.tx()
	req := e.createReq(tx)
	req.Customer = logCanary
	ctx, cancel := normalCtx()
	defer cancel()
	res, ok, err := e.create(ctx, callLimit+time.Second, req)
	if !ok {
		return
	}
	e.expectNoCustomerData("CreatePayment", res.Raw, err)
	if e.ops[connector.OpCharge] {
		ch, _ := e.c.(connector.Charger)
		creq := e.chargeReq(e.tx())
		creq.Customer = logCanary
		var cres connector.ChargeResult
		var cerr error
		if e.call("Charge", callLimit+time.Second, func() { cres, cerr = ch.Charge(ctx, creq) }) {
			e.expectNoCustomerData("Charge", cres.Raw, cerr)
		}
	}
	if err == nil && e.ops[connector.OpQueryPayment] {
		e.p.SetPaymentStatus(tx, connector.StatusSucceeded, e.h.Amount)
		st, ok, qerr := e.query(connector.QueryPaymentRequest{TransactionCode: tx, ProviderRef: res.ProviderRef})
		if ok {
			e.expectNoCustomerData("QueryPayment", st.Raw, qerr)
		}
	}
}
