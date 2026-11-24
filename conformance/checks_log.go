package conformance

import (
	"context"
	"time"

	"github.com/nhanvoanh/payment-sdk/connector"
	"github.com/nhanvoanh/payment-sdk/paykit"
	"github.com/nhanvoanh/payment-sdk/redact"
)

// logCanary là khách mang chuỗi canh của LOG-1: tên có dấu (để thử \uXXXX và dạng bỏ dấu "HUYNH DOAN CANARYNAME" của
// tên chủ thẻ; họ "Huỳnh" và tên đệm "Đoàn" vì dạng bỏ dấu của chúng không trùng từ thông dụng, "Đoàn" để thử Đ→D ở
// mức chữ), email có "+" (để thử URL-encode), điện thoại đầu 0 (để thử 84/+84 và dấu phân cách), địa chỉ nhiều
// dòng (để thử \n đã escape và từng thành phần bị tách ra trường riêng).
var logCanary = paykit.Customer{ID: "C-LOG1", Name: "Huỳnh Đoàn Canaryname", Email: "canary.mail+log1@example.com", Phone: "0987654321",
	Address: "12 Đường Canarystreet\nPhường Bến Nghé, Quận 1"}

// canary dò bằng ĐÚNG luật của providerhttp.RedactCustomer: connector che theo lời khuyên của LOG-1 thì qua LOG-1.
var canary = redact.NewPII(redact.Customer(logCanary.Name, logCanary.Email, logCanary.Phone, logCanary.Address))

const leakAdvice = "cần che trước khi trả, ví dụ providerhttp.RedactCustomer(body, khách) (hoặc RedactValues với các giá trị đọc từ phản hồi); vết lời gọi đối tác, bảng provider_events và log của core sẽ lưu dữ liệu cá nhân của khách"

// expectNoCustomerData: RawExchange và câu lỗi không còn dạng nào của chuỗi canh.
func (e *env) expectNoCustomerData(what string, raw connector.RawExchange, err error) {
	parts := []struct{ name, text string }{{"RawExchange.RequestBody", raw.RequestBody}, {"RawExchange.ResponseBody", raw.ResponseBody}}
	if err != nil {
		parts = append(parts, struct{ name, text string }{"câu lỗi", err.Error()})
	}
	for _, p := range parts {
		if canary.Contains(p.text) {
			e.fail("%s trả %s còn dữ liệu của khách (tên, một chữ của tên, email, điện thoại hay địa chỉ, kể cả một dòng của địa chỉ; thô, JSON-escape, URL-encode, bỏ dấu hay khác hoa thường); %s", what, p.name, leakAdvice)
		}
	}
}

// checkLOG1: mọi thao tác không được trả dữ liệu khách trong RawExchange (hay câu lỗi): tạo và charge mang khách;
// truy vấn, huỷ, hoàn, truy vấn hoàn chạy trên giao dịch đã tạo với khách đó (đối tác có thể nhắc lại khách trong
// phản hồi); ProviderEvent.Raw của webhook của giao dịch đó (được phép rỗng) cũng phải đã che. Cuối cùng tạo và
// charge mang khách khi đối tác từ chối hay trả rác: Raw ở nhánh lỗi (nên có, để tranh chấp) cũng phải đã che.
func checkLOG1(e *env) {
	e.p.SetMode(ModeNormal)
	tx := e.tx()
	req := e.createReq(tx)
	req.Customer = logCanary
	ctx, cancel := normalCtx()
	defer cancel()
	res, ok, err := e.create(ctx, callLimit+time.Second, req)
	if !ok {
		return // tạo treo thì các lượt lỗi cũng treo: luật sẽ quá hạn của chính nó
	}
	defer e.logOnError()
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
	if err != nil {
		return // các bước sau cần giao dịch đã tạo; lỗi của Create thuộc PAY-1
	}
	ref := res.ProviderRef
	e.p.SetPaymentStatus(tx, connector.StatusSucceeded, e.h.Amount)
	if e.ops[connector.OpQueryPayment] {
		st, ok, qerr := e.query(connector.QueryPaymentRequest{TransactionCode: tx, ProviderRef: ref, CreatedAt: txCreatedAt})
		if ok {
			e.expectNoCustomerData("QueryPayment", st.Raw, qerr)
		}
	}
	if e.ops[connector.OpWebhook] {
		if ev, ok, _ := e.parse(e.signed(tx)); ok && canary.Contains(string(ev.Raw)) {
			e.fail("ParseWebhook trả ProviderEvent.Raw còn dữ liệu của khách; trả Raw đã che hoặc rỗng; %s", leakAdvice)
		}
	}
	e.logCall(connector.OpRefund, "CreateRefund", func(ctx context.Context) (connector.RawExchange, error) {
		rf, _ := e.c.(connector.Refunder)
		r, err := rf.CreateRefund(ctx, e.refundReq(tx, ref, "R"+tx+"L"))
		return r.Raw, err
	})
	e.logCall(connector.OpQueryRefund, "QueryRefund", func(ctx context.Context) (connector.RawExchange, error) {
		q, _ := e.c.(connector.RefundQuerier)
		r, err := q.QueryRefund(ctx, connector.QueryRefundRequest{RefundCode: "R" + tx + "L", TransactionCode: tx,
			CreatedAt: refundCreatedAt, TransactionCreatedAt: txCreatedAt})
		return r.Raw, err
	})
	e.logCall(connector.OpCancel, "CancelPayment", func(ctx context.Context) (connector.RawExchange, error) {
		c, _ := e.c.(connector.Canceler)
		r, err := c.CancelPayment(ctx, connector.CancelRequest{TransactionCode: tx, ProviderRef: ref,
			IdempotencyKey: "idem-cxl-" + tx, CreatedAt: txCreatedAt})
		return r.Raw, err
	})
}

// logCall chạy một thao tác đã khai trên giao dịch mang khách chuỗi canh rồi kiểm Raw và câu lỗi.
func (e *env) logCall(op connector.Operation, what string, fn func(ctx context.Context) (connector.RawExchange, error)) {
	if op != "" && !e.ops[op] { // op rỗng: thao tác bắt buộc (tạo)
		return
	}
	ctx, cancel := normalCtx()
	defer cancel()
	var raw connector.RawExchange
	var err error
	if e.call(what, callLimit+time.Second, func() { raw, err = fn(ctx) }) {
		e.expectNoCustomerData(what, raw, err)
	}
}

// logOnError: tạo (và charge nếu có) mang khách chuỗi canh ở ModeReject và ModeGarbage; Raw trả kèm lỗi và câu lỗi
// không được còn dữ liệu khách. Request đã mang khách nên vết của nhánh lỗi lộ khách nếu connector quên che.
func (e *env) logOnError() {
	defer e.p.SetMode(ModeNormal)
	for _, m := range []Mode{ModeReject, ModeGarbage} {
		situation := " khi đối tác từ chối"
		if m == ModeGarbage {
			situation = " khi đối tác trả thân không đọc được"
		}
		e.p.SetMode(m)
		req := e.createReq(e.tx())
		req.Customer = logCanary
		e.logCall("", "CreatePayment"+situation, func(ctx context.Context) (connector.RawExchange, error) {
			r, err := e.c.CreatePayment(ctx, req)
			return r.Raw, err
		})
		creq := e.chargeReq(e.tx())
		creq.Customer = logCanary
		e.logCall(connector.OpCharge, "Charge"+situation, func(ctx context.Context) (connector.RawExchange, error) {
			ch, _ := e.c.(connector.Charger)
			r, err := ch.Charge(ctx, creq)
			return r.Raw, err
		})
	}
}
