package conformance

import (
	"context"
	"errors"
	"net/url"
	"time"
	"unicode"
	"unicode/utf8"

	"bitbucket.org/hasaki-tech/payment-connector-sdk/connector"
	"bitbucket.org/hasaki-tech/payment-connector-sdk/paykit"
)

func (e *env) createReq(tx string) connector.CreatePaymentRequest {
	return connector.CreatePaymentRequest{
		TransactionCode: tx, IdempotencyKey: "idem-" + tx, Money: e.h.Amount, Method: e.h.Method,
		Customer:  paykit.Customer{ID: "C1", Name: "Nguyen Van A", Email: "a@example.com", Phone: "0900000000"},
		ReturnURL: "https://shop.example/return/" + tx, CancelURL: "https://shop.example/cancel/" + tx,
		ClientIP: "203.0.113.10",
	}
}

// create gọi CreatePayment với ctx cho trước; ok sai khi panic hay quá limit (đã thành finding).
func (e *env) create(ctx context.Context, limit time.Duration, req connector.CreatePaymentRequest) (res connector.CreatePaymentResult, ok bool, err error) {
	ok = e.call("CreatePayment", limit, func() { res, err = e.c.CreatePayment(ctx, req) })
	return res, ok, err
}

// createNormal là bước chuẩn bị: tạo một giao dịch ở ModeNormal, trả ProviderRef (rỗng nếu không tạo
// được). Không ghi finding: lỗi của Create đã thuộc về PAY-1, không báo trùng ở luật khác.
func (e *env) createNormal(tx string) string {
	e.p.SetMode(ModeNormal)
	ctx, cancel := normalCtx()
	defer cancel()
	var res connector.CreatePaymentResult
	var err error
	if guard(callLimit+time.Second, func() { res, err = e.c.CreatePayment(ctx, e.createReq(tx)) }) != nil || err != nil {
		return ""
	}
	return res.ProviderRef
}

var validStatuses = map[connector.Status]bool{
	connector.StatusPending: true, connector.StatusSucceeded: true, connector.StatusFailed: true,
	connector.StatusCanceled: true, connector.StatusOnHold: true,
}

func checkPAY1(e *env) {
	e.p.SetMode(ModeNormal)
	ctx, cancel := normalCtx()
	defer cancel()
	res, ok, err := e.create(ctx, callLimit+time.Second, e.createReq(e.tx()))
	if !ok {
		return
	}
	if err != nil {
		e.fail("CreatePayment ở ModeNormal trả lỗi %v; cần thành công; khách không thanh toán được qua đối tác này", err)
		return
	}
	if !validStatuses[res.Status] {
		e.fail("CreatePayment trả Status %q; cần một trong pending, succeeded, failed, canceled, on_hold; core không biết giao dịch đã thu tiền hay chưa", res.Status)
	}
	na := res.NextAction
	switch na.Kind {
	case paykit.NextActionRedirect:
		u, perr := url.Parse(na.URL)
		if perr != nil || !u.IsAbs() || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			e.fail("NextAction redirect có URL %q; cần URL tuyệt đối http(s); khách không tới được trang thanh toán", na.URL)
		}
	case paykit.NextActionShowQR:
		if na.QRData == "" {
			e.fail("NextAction show_qr có QRData rỗng; cần dữ liệu QR; khách không có mã để quét trả tiền")
		}
	case paykit.NextActionNone, "":
	default:
		e.fail("NextAction.Kind %q không thuộc none, redirect, show_qr; client của core không biết dẫn khách đi đâu", na.Kind)
	}
}

func checkPAY2(e *env) {
	e.p.SetMode(ModeNormal)
	ctx, cancel := normalCtx()
	defer cancel()
	res, ok, err := e.create(ctx, callLimit+time.Second, e.createReq(e.tx()))
	if !ok {
		return
	}
	if err != nil {
		e.skipf("CreatePayment ở ModeNormal lỗi nên không có ProviderRef để kiểm (xem PAY-1)")
		return
	}
	ref := res.ProviderRef
	switch {
	case !utf8.ValidString(ref):
		e.fail("ProviderRef %q không phải UTF-8 hợp lệ; cần UTF-8; core thay byte hỏng nên gửi lại đối tác một mã khác ở lệnh truy vấn/hoàn tiền", ref)
	case containsControl(ref):
		e.fail("ProviderRef %q có ký tự điều khiển; cần không có; core bỏ ký tự đó nên gửi lại đối tác một mã khác ở lệnh truy vấn/hoàn tiền", ref)
	case utf8.RuneCountInString(ref) > 255:
		e.fail("ProviderRef dài %d ký tự; cần tối đa 255; core cắt và gửi bản bị cắt cho đối tác ở các lệnh truy vấn/hoàn sau, đối tác không nhận ra giao dịch", utf8.RuneCountInString(ref))
	}
}

func containsControl(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

// expectUnavailable: đối tác từ chối kết nối thì lỗi phải khớp ErrProviderUnavailable.
func (e *env) expectUnavailable(what string, fn func(ctx context.Context) error) {
	e.p.SetMode(ModeDown)
	ctx, cancel := normalCtx()
	defer cancel()
	var err error
	if !e.call(what, callLimit+time.Second, func() { err = fn(ctx) }) {
		return
	}
	if !errors.Is(err, connector.ErrProviderUnavailable) {
		e.fail("%s khi đối tác từ chối kết nối trả %v; cần lỗi khớp ErrProviderUnavailable; core không chốt thất bại được, giao dịch treo chờ truy vấn và khách không tạo lại thanh toán ngay được", what, errText(err))
	}
}

// expectHang: đối tác treo, ctx hạn 300 ms: phải trả về kịp, có lỗi, và không được nói chắc là chưa xử lý hay bị từ chối.
func (e *env) expectHang(what string, fn func(ctx context.Context) error) {
	e.p.SetMode(ModeHang)
	ctx, cancel := context.WithTimeout(context.Background(), hangCtx)
	defer cancel()
	var err error
	if !e.call(what+" khi đối tác treo (ctx hạn 300ms)", hangCtx+hangSlack, func() { err = fn(ctx) }) {
		return
	}
	switch {
	case err == nil:
		e.fail("%s khi đối tác treo trả err nil; cần lỗi không rõ kết quả (ErrUnknownOutcome); core sẽ coi lời gọi thành công dù không biết đối tác đã xử lý chưa", what)
	case errors.Is(err, connector.ErrProviderUnavailable):
		e.fail("%s khi hết hạn chờ đối tác trả %v (khớp ErrProviderUnavailable); cần ErrUnknownOutcome; core sẽ chốt thất bại trong khi đối tác có thể đã trừ tiền, khách trả tiền hai lần", what, err)
	case errors.Is(err, connector.ErrProviderRejected):
		e.fail("%s khi hết hạn chờ đối tác trả %v (khớp ErrProviderRejected); cần ErrUnknownOutcome; core sẽ chốt thất bại trong khi đối tác có thể đã trừ tiền, khách trả tiền hai lần", what, err)
	}
}

// expectRejected: đối tác từ chối rõ ràng thì lỗi phải khớp ErrProviderRejected.
func (e *env) expectRejected(what string, fn func(ctx context.Context) error) {
	e.p.SetMode(ModeReject)
	ctx, cancel := normalCtx()
	defer cancel()
	var err error
	if !e.call(what, callLimit+time.Second, func() { err = fn(ctx) }) {
		return
	}
	if !errors.Is(err, connector.ErrProviderRejected) {
		e.fail("%s khi đối tác từ chối rõ ràng trả %v; cần lỗi khớp ErrProviderRejected (nên dùng RejectedError); core không chốt thất bại được, giao dịch treo và khách phải chờ", what, errText(err))
	}
}

func errText(err error) string {
	if err == nil {
		return "err nil"
	}
	return "lỗi «" + err.Error() + "»"
}

func (e *env) createFn(tx string) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		_, err := e.c.CreatePayment(ctx, e.createReq(tx))
		return err
	}
}

func checkPAY3(e *env) { e.expectUnavailable("CreatePayment", e.createFn(e.tx())) }
func checkPAY4(e *env) { e.expectHang("CreatePayment", e.createFn(e.tx())) }
func checkPAY5(e *env) { e.expectRejected("CreatePayment", e.createFn(e.tx())) }

func checkPAY6(e *env) {
	e.p.SetMode(ModeGarbage)
	ctx, cancel := normalCtx()
	defer cancel()
	res, ok, err := e.create(ctx, callLimit+time.Second, e.createReq(e.tx()))
	if !ok {
		return
	}
	switch {
	case err == nil:
		e.fail("CreatePayment khi đối tác trả thân không đọc được trả err nil (Status %q); cần ErrUnknownOutcome; core tin một kết quả mà connector không hiểu", res.Status)
	case errors.Is(err, connector.ErrProviderUnavailable), errors.Is(err, connector.ErrProviderRejected):
		e.fail("CreatePayment khi đối tác trả thân không đọc được trả %v; cần ErrUnknownOutcome, không được khớp ErrProviderUnavailable hay ErrProviderRejected; core sẽ chốt thất bại trong khi đối tác có thể đã nhận tiền, khách trả tiền hai lần", err)
	}
}

// checkPAY7: core gửi lại cùng khoá idempotency khi không rõ kết quả; connector phải chịu được.
func checkPAY7(e *env) {
	e.p.SetMode(ModeNormal)
	req := e.createReq(e.tx())
	for i := 0; i < 2; i++ {
		ctx, cancel := normalCtx()
		_, ok, _ := e.create(ctx, callLimit+time.Second, req)
		cancel()
		if !ok {
			return
		}
	}
}

func (e *env) querier() connector.PaymentQuerier { q, _ := e.c.(connector.PaymentQuerier); return q }

func (e *env) query(req connector.QueryPaymentRequest) (st connector.PaymentStatus, ok bool, err error) {
	ctx, cancel := normalCtx()
	defer cancel()
	ok = e.call("QueryPayment", callLimit+time.Second, func() { st, err = e.querier().QueryPayment(ctx, req) })
	return st, ok, err
}

// checkQRY1: core truy vấn cả giao dịch chưa từng tới được đối tác; "không thấy" phải là Found false.
func checkQRY1(e *env) {
	e.p.SetMode(ModeNormal)
	st, ok, err := e.query(connector.QueryPaymentRequest{TransactionCode: e.tx()})
	if !ok {
		return
	}
	if err != nil {
		e.fail("QueryPayment cho giao dịch đối tác không biết trả %v; cần Found false và err nil; core không bao giờ đóng được giao dịch chưa tới đối tác, khách không tạo lại thanh toán được", err)
		return
	}
	if st.Found {
		e.fail("QueryPayment cho giao dịch đối tác không biết trả Found true (Status %q); cần Found false; core có thể ghi nhận một thanh toán không có thật", st.Status)
	}
}

func checkQRY2(e *env) {
	tx := e.tx()
	ref := e.createNormal(tx)
	e.p.SetMode(ModeNormal)
	e.p.SetPaymentStatus(tx, connector.StatusSucceeded, e.h.Amount)
	st, ok, err := e.query(connector.QueryPaymentRequest{TransactionCode: tx, ProviderRef: ref})
	if !ok {
		return
	}
	if err != nil {
		e.fail("QueryPayment cho giao dịch đã thanh toán trả %v; cần Found true, Status succeeded; khách đã trả tiền mà đơn không được xác nhận", err)
		return
	}
	if !st.Found || st.Status != connector.StatusSucceeded {
		e.fail("QueryPayment cho giao dịch đã thanh toán trả Found %v, Status %q; cần Found true, Status succeeded; khách đã trả tiền mà đơn không được xác nhận", st.Found, st.Status)
	}
	if st.AmountPaid.Amount != 0 && st.AmountPaid.Amount != e.h.Amount.Amount {
		e.fail("QueryPayment trả AmountPaid %d trong khi đối tác báo %d; cần đúng số đối tác báo hoặc 0; core sẽ đưa giao dịch vào lệch tiền", st.AmountPaid.Amount, e.h.Amount.Amount)
	}
	if c := st.AmountPaid.Currency; c != "" && !isCurrency(c) {
		e.fail("QueryPayment trả AmountPaid.Currency %q; cần rỗng hoặc ba chữ A–Z; core đổi thành XXX và đưa giao dịch vào lệch tiền", c)
	}
}

func isCurrency(c string) bool {
	_, err := paykit.NewMoney(0, c)
	return err == nil
}

func checkQRY3(e *env) {
	tx := e.tx()
	ref := e.createNormal(tx)
	e.expectHang("QueryPayment", func(ctx context.Context) error {
		_, err := e.querier().QueryPayment(ctx, connector.QueryPaymentRequest{TransactionCode: tx, ProviderRef: ref})
		return err
	})
}

// checkCXL1: huỷ một giao dịch khách đã trả tiền mà báo canceled thì core hoàn đơn trong khi tiền đã bị trừ.
func checkCXL1(e *env) {
	tx := e.tx()
	ref := e.createNormal(tx)
	e.p.SetMode(ModeNormal)
	e.p.SetPaymentStatus(tx, connector.StatusSucceeded, e.h.Amount)
	cn, _ := e.c.(connector.Canceler)
	ctx, cancel := normalCtx()
	defer cancel()
	var res connector.CancelResult
	var err error
	if !e.call("CancelPayment", callLimit+time.Second, func() {
		res, err = cn.CancelPayment(ctx, connector.CancelRequest{TransactionCode: tx, ProviderRef: ref, IdempotencyKey: "cancel-" + tx})
	}) {
		return
	}
	if err == nil && res.Status == connector.StatusCanceled {
		e.fail("CancelPayment cho giao dịch đối tác báo đã thanh toán trả Status canceled, err nil; cần trạng thái khác canceled hoặc lỗi; core huỷ đơn trong khi khách đã bị trừ tiền")
	}
}

// checkCTX1: core huỷ ctx khi tắt máy hay khi client bỏ đi; connector phải buông ngay.
func checkCTX1(e *env) {
	e.p.SetMode(ModeHang)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tx := e.tx()
	calls := []struct {
		op connector.Operation
		fn func()
	}{
		{"", func() { _, _ = e.c.CreatePayment(ctx, e.createReq(tx)) }},
		{connector.OpCharge, func() {
			r := e.createReq(tx)
			_, _ = e.c.(connector.Charger).Charge(ctx, connector.ChargeRequest{TransactionCode: tx, IdempotencyKey: r.IdempotencyKey,
				Money: r.Money, Method: r.Method, Customer: r.Customer, ReturnURL: r.ReturnURL, CancelURL: r.CancelURL, ClientIP: r.ClientIP})
		}},
		{connector.OpQueryPayment, func() {
			_, _ = e.c.(connector.PaymentQuerier).QueryPayment(ctx, connector.QueryPaymentRequest{TransactionCode: tx})
		}},
		{connector.OpCancel, func() {
			_, _ = e.c.(connector.Canceler).CancelPayment(ctx, connector.CancelRequest{TransactionCode: tx, IdempotencyKey: "cancel-" + tx})
		}},
		{connector.OpRefund, func() { _, _ = e.c.(connector.Refunder).CreateRefund(ctx, e.refundReq(tx, "", "R"+tx)) }},
		{connector.OpQueryRefund, func() {
			_, _ = e.c.(connector.RefundQuerier).QueryRefund(ctx, connector.QueryRefundRequest{RefundCode: "R" + tx, TransactionCode: tx})
		}},
	}
	for _, c := range calls {
		if c.op != "" && !e.ops[c.op] {
			continue
		}
		name := "CreatePayment"
		if c.op != "" {
			name = string(c.op)
		}
		if err := guard(cancelLimit, c.fn); err != nil {
			e.fail("%s với ctx đã huỷ (đối tác treo): %v; cần trả về trong %v và không panic; core không dừng được lời gọi khi tắt máy hay khi khách bỏ đi, lời gọi treo giữ chân tài nguyên của core", name, err, cancelLimit)
		}
	}
}
