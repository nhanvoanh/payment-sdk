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
	}
}

// checkRFD1: cùng luật tiền như Create (PAY-3..5) cho lệnh hoàn tiền.
func checkRFD1(e *env) {
	tx := e.tx()
	ref := e.createNormal(tx)
	e.p.SetPaymentStatus(tx, connector.StatusSucceeded, e.h.Amount)
	rf, _ := e.c.(connector.Refunder)
	fn := func(code string) func(ctx context.Context) error {
		return func(ctx context.Context) error {
			_, err := rf.CreateRefund(ctx, e.refundReq(tx, ref, code))
			return err
		}
	}
	e.expectUnavailable("CreateRefund", fn("R"+tx+"D"))
	e.expectHang("CreateRefund", fn("R"+tx+"H"))
	e.expectRejected("CreateRefund", fn("R"+tx+"J"))
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
		st, err = q.QueryRefund(ctx, connector.QueryRefundRequest{RefundCode: "R" + tx, TransactionCode: tx})
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
