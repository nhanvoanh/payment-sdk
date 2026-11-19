package conformance

import (
	"context"
	"time"

	"bitbucket.org/hasaki-tech/payment-connector-sdk/connector"
)

func (e *env) refundReq(tx, ref, refundCode string) connector.RefundRequest {
	return connector.RefundRequest{
		RefundCode: refundCode, IdempotencyKey: "idem-" + refundCode, TransactionCode: tx, ProviderRef: ref,
		Money: e.h.Amount, Reason: "conformance", CustomerID: "C1",
		TransactionCreatedAt: txCreatedAt, CreatedAt: refundCreatedAt,
	}
}

// checkRFD1: cùng luật tiền như Create (PAY-3..6) cho lệnh hoàn tiền.
func checkRFD1(e *env) {
	tx := e.tx()
	ref, ok := e.createNormal(tx)
	if !ok {
		return
	}
	e.p.SetPaymentStatus(tx, connector.StatusSucceeded, e.h.Amount)
	rf, _ := e.c.(connector.Refunder)
	fn := func(code string) func(ctx context.Context) error {
		return func(ctx context.Context) error {
			_, err := rf.CreateRefund(ctx, e.refundReq(tx, ref, code))
			return err
		}
	}
	e.expectUnavailable("CreateRefund", fn("R"+tx+"D"))
	e.expectUnknown(ModeHang, "CreateRefund", fn("R"+tx+"H"))
	e.expectRejected("CreateRefund", fn("R"+tx+"J"))
	e.expectUnknown(ModeGarbage, "CreateRefund", fn("R"+tx+"G"))
}

// checkRFD2: lệnh hoàn chưa tới được đối tác thì truy vấn phải nói "không thấy", không phải lỗi.
func checkRFD2(e *env) {
	e.p.SetMode(ModeNormal)
	q, _ := e.c.(connector.RefundQuerier)
	tx := e.tx()
	ctx, cancel := normalCtx()
	defer cancel()
	var st connector.RefundStatus
	var err error
	if !e.call("QueryRefund", callLimit+time.Second, func() {
		st, err = q.QueryRefund(ctx, connector.QueryRefundRequest{RefundCode: "R" + tx, TransactionCode: tx, CreatedAt: refundCreatedAt, TransactionCreatedAt: txCreatedAt})
	}) {
		return
	}
	if err != nil {
		e.fail("QueryRefund cho lệnh hoàn đối tác không biết trả %v; cần Found false và err nil; core không đóng được lệnh hoàn chưa tới đối tác, khách không được hoàn tiền lại", err)
		return
	}
	if st.Found {
		e.fail("QueryRefund cho lệnh hoàn đối tác không biết trả Found true (Status %q); cần Found false; core có thể ghi nhận một khoản hoàn không có thật", st.Status)
	}
}

// checkRFD3: truy vấn hoàn tiền khi đối tác treo hay trả rác phải ra lỗi; err nil thì core tin kết quả bịa.
func checkRFD3(e *env) {
	q, _ := e.c.(connector.RefundQuerier)
	tx := e.tx()
	for _, m := range []Mode{ModeHang, ModeGarbage} {
		e.expectError(m, "QueryRefund",
			"core áp kết quả connector tự suy ra (failed thì core chốt hoàn thất bại và có thể hoàn tiền cho khách hai lần)",
			func(ctx context.Context) error {
				_, err := q.QueryRefund(ctx, connector.QueryRefundRequest{RefundCode: "R" + tx, TransactionCode: tx, CreatedAt: refundCreatedAt, TransactionCreatedAt: txCreatedAt})
				return err
			})
	}
}

// checkRFD4: lệnh hoàn đối tác ĐÃ nhận phải tìm được bằng RefundCode + CreatedAt khi core chưa có ProviderRef (core
// mất kết quả tạo: tiến trình chết sau lời gọi, lần ghi kết quả hỏng). Core chốt failed/not_sent và nhả số tiền
// đang giữ cho refund còn `new` mà truy vấn trả Found=false, nên "không thấy" chỉ hợp lệ khi đối tác chắc chắn
// không có lệnh hoàn đó. Không truy vấn được khi thiếu ProviderRef thì trả ErrUnknownOutcome.
func checkRFD4(e *env) {
	tx := e.tx()
	ref, ok := e.createNormal(tx)
	if !ok {
		return
	}
	e.p.SetPaymentStatus(tx, connector.StatusSucceeded, e.h.Amount)
	e.p.SetMode(ModeNormal)
	rf, _ := e.c.(connector.Refunder)
	q, _ := e.c.(connector.RefundQuerier)
	code := "R" + tx + "Q"
	ctx, cancel := normalCtx()
	defer cancel()
	var err error
	if !e.call("CreateRefund", callLimit+time.Second, func() { _, err = rf.CreateRefund(ctx, e.refundReq(tx, ref, code)) }) {
		return
	}
	if err != nil {
		e.skipf("CreateRefund ở ModeNormal lỗi (%s) nên không kiểm được truy vấn thiếu ProviderRef, xem RFD-1", errText(err))
		return
	}
	var st connector.RefundStatus
	if !e.call("QueryRefund", callLimit+time.Second, func() {
		st, err = q.QueryRefund(ctx, connector.QueryRefundRequest{RefundCode: code, TransactionCode: tx,
			CreatedAt: refundCreatedAt, TransactionCreatedAt: txCreatedAt})
	}) {
		return
	}
	switch {
	case err == nil && st.Found, isUnknownForCore(err):
		return
	case err == nil:
		e.fail("QueryRefund (ProviderRef rỗng) cho lệnh hoàn đối tác vừa nhận trả Found false; cần Found true (truy vấn theo mã dựng từ RefundCode + CreatedAt) hoặc ErrUnknownOutcome; core chốt refund failed/not_sent và nhả số tiền trong khi đối tác có thể đã hoàn, khách có thể được hoàn hai lần")
	default:
		e.fail("QueryRefund (ProviderRef rỗng) cho lệnh hoàn đối tác vừa nhận trả %v (core hiểu là %s); cần Found true hoặc ErrUnknownOutcome; core không biết đối tác có giữ lệnh hoàn hay không", err, outcomeText(err))
	}
}
