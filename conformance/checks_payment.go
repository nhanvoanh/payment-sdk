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

// createNormal là bước chuẩn bị: tạo một giao dịch ở ModeNormal, trả ProviderRef. Không tạo được thì
// luật vào Skipped (lỗi của Create thuộc về PAY-1, không báo trùng ở luật khác) và trả ok sai.
func (e *env) createNormal(tx string) (string, bool) {
	e.p.SetMode(ModeNormal)
	ctx, cancel := normalCtx()
	defer cancel()
	var res connector.CreatePaymentResult
	var err error
	if g := guard(callLimit+time.Second, func() { res, err = e.c.CreatePayment(ctx, e.createReq(tx)) }); g != nil || err != nil {
		reason := errText(err)
		if g != nil {
			reason = g.Error()
		}
		e.skipf("bước chuẩn bị CreatePayment ở ModeNormal hỏng (%s), xem PAY-1", reason)
		return "", false
	}
	return res.ProviderRef, true
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

// isUnknownForCore: core giữ giao dịch ở trạng thái chờ truy vấn, không chốt thất bại.
func isUnknownForCore(err error) bool { return connector.Classify(err) == connector.CallUnknown }

func outcomeText(err error) string {
	switch connector.Classify(err) {
	case connector.CallOK:
		return "thành công"
	case connector.CallRejected:
		return "bị từ chối chắc chắn"
	case connector.CallUnavailable:
		return "chắc chắn chưa xử lý"
	}
	return "chưa rõ kết quả"
}

// modeCall đặt đối tác vào mode m rồi gọi fn. Với ModeHang: gọi thẳng thì ctx hạn 300 ms; qua gRPC thì
// ctx phía proxy hạn 300 ms + 2 s còn server bọc ctx của handler bằng hạn 300 ms, để connector luôn trả
// lời (đúng hay sai) trước khi proxy hết hạn và câu trả lời sai luôn tới được proxy.
func (e *env) modeCall(m Mode, what string, fn func(ctx context.Context) error) (bool, error) {
	e.p.SetMode(m)
	ctxLimit, limit := callLimit, callLimit+time.Second
	if m == ModeHang {
		what += " khi đối tác treo (ctx hạn 300ms)"
		ctxLimit, limit = hangCtx, hangCtx+hangSlack
		if e.srv != nil {
			ctxLimit, limit = hangCtx+hangSlack, hangCtx+hangSlack+time.Second
			e.srv.setHandlerDeadline(hangCtx)
			defer e.srv.setHandlerDeadline(0)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), ctxLimit)
	defer cancel()
	var err error
	ok := e.call(what, limit, func() { err = fn(ctx) })
	return ok, err
}

// expectUnavailable: đối tác từ chối kết nối thì core phải hiểu là "chắc chắn chưa xử lý".
func (e *env) expectUnavailable(what string, fn func(ctx context.Context) error) {
	ok, err := e.modeCall(ModeDown, what, fn)
	if ok && (!errors.Is(err, connector.ErrProviderUnavailable) || connector.Classify(err) != connector.CallUnavailable) {
		e.fail("%s khi đối tác từ chối kết nối trả %v (core hiểu là %s); cần lỗi khớp ErrProviderUnavailable và không khớp ErrUnknownOutcome hay ErrProviderRejected; core không chốt thất bại được, giao dịch treo chờ truy vấn và khách không tạo lại thanh toán ngay được", what, errText(err), outcomeText(err))
	}
}

// expectRejected: đối tác từ chối rõ ràng thì core phải hiểu là "bị từ chối".
func (e *env) expectRejected(what string, fn func(ctx context.Context) error) {
	ok, err := e.modeCall(ModeReject, what, fn)
	if ok && connector.Classify(err) != connector.CallRejected {
		e.fail("%s khi đối tác từ chối rõ ràng trả %v (core hiểu là %s); cần lỗi khớp ErrProviderRejected (nên dùng RejectedError) và không khớp ErrUnknownOutcome; core không chốt thất bại được, giao dịch treo và khách phải chờ", what, errText(err), outcomeText(err))
	}
}

// expectUnknown: đối tác treo (ModeHang) hay trả thân không đọc được (ModeGarbage) thì không ai biết đối
// tác đã xử lý chưa: lỗi phải là "chưa rõ" theo đúng thứ tự của core.
func (e *env) expectUnknown(m Mode, what string, fn func(ctx context.Context) error) {
	ok, err := e.modeCall(m, what, fn)
	if !ok || isUnknownForCore(err) {
		return
	}
	situation := "khi hết hạn chờ đối tác"
	if m == ModeGarbage {
		situation = "khi đối tác trả thân không đọc được"
	}
	if err == nil {
		e.fail("%s %s trả err nil; cần lỗi chưa rõ kết quả (ErrUnknownOutcome); core tin một kết quả mà connector không biết", what, situation)
		return
	}
	e.fail("%s %s trả %v (core hiểu là %s); cần ErrUnknownOutcome, không được khớp ErrProviderUnavailable, ErrNotSupported hay ErrProviderRejected; core sẽ chốt thất bại trong khi đối tác có thể đã trừ tiền, khách trả tiền hai lần", what, situation, err, outcomeText(err))
}

// expectError: với truy vấn, core coi mọi lỗi như nhau (thử lại sau); điều nguy hiểm duy nhất là err nil,
// vì khi đó core tin Found/Status mà connector đọc từ một câu trả lời không có hoặc không hiểu được.
func (e *env) expectError(m Mode, what, consequence string, fn func(ctx context.Context) error) {
	ok, err := e.modeCall(m, what, fn)
	if !ok || err != nil {
		return
	}
	situation := "khi hết hạn chờ đối tác"
	if m == ModeGarbage {
		situation = "khi đối tác trả thân không đọc được"
	}
	e.fail("%s %s trả err nil; cần một lỗi bất kỳ; %s", what, situation, consequence)
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
func checkPAY4(e *env) { e.expectUnknown(ModeHang, "CreatePayment", e.createFn(e.tx())) }
func checkPAY5(e *env) { e.expectRejected("CreatePayment", e.createFn(e.tx())) }
func checkPAY6(e *env) { e.expectUnknown(ModeGarbage, "CreatePayment", e.createFn(e.tx())) }

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
	ref, ok := e.createNormal(tx)
	if !ok {
		return
	}
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
	ref, ok := e.createNormal(tx)
	if !ok {
		return
	}
	e.expectUnknown(ModeHang, "QueryPayment", func(ctx context.Context) error {
		_, err := e.querier().QueryPayment(ctx, connector.QueryPaymentRequest{TransactionCode: tx, ProviderRef: ref})
		return err
	})
}

// checkCXL1: huỷ một giao dịch khách đã trả tiền mà báo canceled thì core hoàn đơn trong khi tiền đã bị trừ.
func checkCXL1(e *env) {
	tx := e.tx()
	ref, ok := e.createNormal(tx)
	if !ok {
		return
	}
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
			_, _ = e.c.(connector.Charger).Charge(ctx, e.chargeReq(tx))
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

// checkCXL2: không biết đối tác đã huỷ chưa thì không được nói "đã huỷ"; báo "chắc chắn" thì core chốt sai.
func checkCXL2(e *env) {
	tx := e.tx()
	ref, ok := e.createNormal(tx)
	if !ok {
		return
	}
	cn, _ := e.c.(connector.Canceler)
	for _, m := range []Mode{ModeHang, ModeGarbage} {
		var res connector.CancelResult
		ok, err := e.modeCall(m, "CancelPayment", func(ctx context.Context) error {
			var err error
			res, err = cn.CancelPayment(ctx, connector.CancelRequest{TransactionCode: tx, ProviderRef: ref, IdempotencyKey: "cancel-" + tx})
			return err
		})
		if !ok {
			continue
		}
		situation := "khi hết hạn chờ đối tác"
		if m == ModeGarbage {
			situation = "khi đối tác trả thân không đọc được"
		}
		e.expectCancelUnknown(situation, res, err)
	}
	e.cancelWithoutRefOnHang(tx)
}

// expectCancelUnknown: lời huỷ đã tới đối tác mà không nghe được trả lời hiểu được; câu trả lời hợp lệ duy nhất
// là lỗi core hiểu là "chưa rõ". err nil kèm trạng thái bất kỳ là một câu trả lời bịa: canceled thì core huỷ
// đơn, trạng thái khác thì core hiểu là đối tác từ chối huỷ (như RejectedError) và quyết theo truy vấn lại.
func (e *env) expectCancelUnknown(situation string, res connector.CancelResult, err error) {
	switch {
	case err == nil && res.Status == connector.StatusCanceled:
		e.fail("CancelPayment %s trả Status canceled, err nil; cần ErrUnknownOutcome; core huỷ đơn trong khi đối tác có thể vẫn thu tiền của khách", situation)
	case err == nil:
		e.fail("CancelPayment %s trả Status %q, err nil; cần ErrUnknownOutcome; core hiểu là đối tác nói rõ không huỷ (như RejectedError) và quyết theo một câu trả lời không có", situation, res.Status)
	case !isUnknownForCore(err):
		e.fail("CancelPayment %s trả %v (core hiểu là %s); cần ErrUnknownOutcome; core tin chắc một kết quả huỷ mà không ai biết, đơn và tiền của khách lệch nhau", situation, err, outcomeText(err))
	}
}

// cancelWithoutRefOnHang: lượt thêm của CXL-2 — huỷ thiếu ProviderRef khi đối tác treo (connector có thể huỷ
// theo mã giao dịch). Bộ đếm request của đối tác giả phân biệt tất định: không request nào tới đối tác thì câu
// trả lời là quyết định tại chỗ (từ chối, không hỗ trợ, hay trạng thái khác canceled đều được); có request tới
// thì như lượt có mã: chỉ "chưa rõ" hợp lệ.
func (e *env) cancelWithoutRefOnHang(tx string) {
	counter, ok := e.p.(RequestCounter)
	if !ok {
		e.skipf("lượt huỷ thiếu ProviderRef khi đối tác treo không kiểm được: Partner cần cài conformance.RequestCounter để biết connector có gọi đối tác hay không")
		return
	}
	cn, _ := e.c.(connector.Canceler)
	var before int
	var res connector.CancelResult
	ok, err := e.modeCall(ModeHang, "CancelPayment thiếu ProviderRef", func(ctx context.Context) error {
		before = counter.BusinessRequests() // chụp ngay trước lời huỷ đang xét, sau khi đã đặt mode
		var err error
		res, err = cn.CancelPayment(ctx, connector.CancelRequest{TransactionCode: tx, IdempotencyKey: "cancel-noref-" + tx})
		return err
	})
	if !ok {
		return
	}
	const situation = "thiếu ProviderRef khi hết hạn chờ đối tác"
	if err == nil && res.Status == connector.StatusCanceled {
		e.fail("CancelPayment %s trả Status canceled, err nil; cần ErrUnknownOutcome (đã gọi đối tác) hoặc từ chối tại chỗ; core huỷ đơn trong khi đối tác có thể vẫn thu tiền của khách", situation)
		return
	}
	if isUnknownForCore(err) || !reachedPartner(counter, before) {
		return
	}
	e.expectCancelUnknown(situation+" (đã gửi request tới đối tác)", res, err)
}

// reachedPartner: đối tác giả đã nhận request nghiệp vụ nào sau mốc before. Connector có thể đã bỏ request
// đang bay (timeout nội bộ) trước khi đối tác giả kịp đếm, nên chờ tối đa settleWait cho nó tới.
func reachedPartner(c RequestCounter, before int) bool {
	deadline := time.Now().Add(settleWait)
	for {
		if c.BusinessRequests() > before {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// checkCXL4: đối tác báo đã thu khi huỷ. Hợp lệ chỉ khi err nil kèm succeeded/on_hold ("đã thu") hoặc lỗi core
// hiểu là "chưa rõ". err nil kèm trạng thái khác được core xếp chung với RejectedError ("bị từ chối"), nên vi phạm
// như nó; canceled kèm err nil là vi phạm của CXL-1, không báo hai lần.
func checkCXL4(e *env) {
	tx := e.tx()
	ref, ok := e.createNormal(tx)
	if !ok {
		return
	}
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
	const need = "cần err nil kèm Status succeeded hoặc on_hold (hoặc ErrUnknownOutcome nếu không phân biệt được \"đã thu\"); core hiểu câu trả lời này là bị từ chối và quyết theo lần truy vấn lại, truy vấn của đối tác còn chậm thì giao dịch khách đã trả bị xử lý như chưa trả"
	switch {
	case err == nil && (res.Status == connector.StatusSucceeded || res.Status == connector.StatusOnHold || res.Status == connector.StatusCanceled):
		// canceled thuộc CXL-1.
	case err == nil:
		e.fail("CancelPayment cho giao dịch đối tác báo đã thanh toán trả Status %q, err nil; %s", res.Status, need)
	case !isUnknownForCore(err):
		e.fail("CancelPayment cho giao dịch đối tác báo đã thanh toán trả %v (core hiểu là %s); %s", errText(err), outcomeText(err), need)
	}
}

// checkCXL3: core huỷ cả giao dịch mà Create chưa nghe được trả lời (không có ProviderRef, đối tác có thể không
// biết). Connector phải trả lời dứt khoát; "chưa rõ" hay "không gọi được" thì core không bao giờ cho hết hạn.
func checkCXL3(e *env) {
	tx := e.tx()
	e.p.SetMode(ModeNormal)
	cn, _ := e.c.(connector.Canceler)
	ctx, cancel := normalCtx()
	defer cancel()
	var err error
	if !e.call("CancelPayment", callLimit+time.Second, func() {
		_, err = cn.CancelPayment(ctx, connector.CancelRequest{TransactionCode: tx, IdempotencyKey: "cancel-" + tx})
	}) {
		return
	}
	// Cùng thứ tự với cách core hiểu một câu trả lời huỷ: chưa rõ trước, rồi không hỗ trợ, rồi từ chối.
	switch {
	case err == nil:
	case connector.Classify(err) == connector.CallUnknown:
		e.fail("CancelPayment cho giao dịch đối tác không biết, ProviderRef rỗng, trả %v (core hiểu là chưa rõ kết quả); cần câu trả lời dứt khoát: trạng thái (err nil), RejectedError (ví dụ mã cannot_cancel) hoặc ErrNotSupported; core không bao giờ cho giao dịch quá hạn hết hạn, đơn của khách kẹt tới khi có người chốt tay", errText(err))
	case errors.Is(err, connector.ErrNotSupported), connector.Classify(err) == connector.CallRejected:
	default:
		e.fail("CancelPayment cho giao dịch đối tác không biết, ProviderRef rỗng, trả %v (core hiểu là đối tác không trả lời được); cần câu trả lời dứt khoát: trạng thái (err nil), RejectedError (ví dụ mã cannot_cancel) hoặc ErrNotSupported; ErrProviderUnavailable chỉ dành cho khi đã cố gọi đối tác mà không tới; core không bao giờ cho giao dịch quá hạn hết hạn và mỗi lượt quét làm cầu chì đếm thêm một lỗi", errText(err))
	}
}

func (e *env) chargeReq(tx string) connector.ChargeRequest {
	r := e.createReq(tx)
	return connector.ChargeRequest{TransactionCode: tx, IdempotencyKey: r.IdempotencyKey, Money: r.Money, Method: r.Method,
		Customer: r.Customer, ReturnURL: r.ReturnURL, CancelURL: r.CancelURL, ClientIP: r.ClientIP}
}

// checkCHG1: Charge thu tiền trực tiếp nên luật tiền của Create (PAY-3..6) áp nguyên cho nó.
func checkCHG1(e *env) {
	ch, _ := e.c.(connector.Charger)
	fn := func(tx string) func(ctx context.Context) error {
		return func(ctx context.Context) error {
			_, err := ch.Charge(ctx, e.chargeReq(tx))
			return err
		}
	}
	e.expectUnavailable("Charge", fn(e.tx()))
	e.expectUnknown(ModeHang, "Charge", fn(e.tx()))
	e.expectRejected("Charge", fn(e.tx()))
	e.expectUnknown(ModeGarbage, "Charge", fn(e.tx()))
}

// checkQRY4: truy vấn gặp thân rác mà trả err nil thì core tin Found/Status bịa ra.
func checkQRY4(e *env) {
	tx := e.tx()
	ref, ok := e.createNormal(tx)
	if !ok {
		return
	}
	e.expectError(ModeGarbage, "QueryPayment",
		"core áp kết quả connector tự suy ra (failed thì khách tạo lại thanh toán trong khi đối tác có thể đã thu tiền; không thấy thì giao dịch bị cho hết hạn)",
		func(ctx context.Context) error {
			_, err := e.querier().QueryPayment(ctx, connector.QueryPaymentRequest{TransactionCode: tx, ProviderRef: ref})
			return err
		})
}
