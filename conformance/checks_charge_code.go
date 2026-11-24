package conformance

import (
	"strings"
	"time"

	"github.com/nhanvoanh/payment-sdk/connector"
	"github.com/nhanvoanh/payment-sdk/paykit"
	"github.com/nhanvoanh/payment-sdk/redact"
)

// chargeCodeCanary là mã thanh toán canh của CHG-2 khi Harness.ChargeCode không đặt. Chỉ là chuỗi thử, không phải mã thật.
const chargeCodeCanary = "CFPAYCODE7Q4X9K2M8ZT3"

const (
	codeAdvice = "cần gửi mã cho đối tác chỉ ở dạng đối tác yêu cầu và không trả lại nó; che bằng providerhttp.RedactValues(body, mã) trước khi trả"
	codeHarm   = "core lưu Raw, ProviderRef, NextAction và ghi câu lỗi vào log: mã mang quyền trừ tiền của khách nằm lại ở đó"
)

// place là một chỗ connector trả về mà core lưu hay ghi log.
type place struct{ name, text string }

// leaks báo text còn một dạng nào của code: thô, hoặc JSON-escape, URL-encode, khác hoa thường (bộ so của LOG-1).
func leaks(text, code string) bool {
	return text != "" && (strings.Contains(text, code) || redact.Contains(text, code))
}

func rawPlaces(raw connector.RawExchange) []place {
	return []place{{"RawExchange.RequestBody", raw.RequestBody}, {"RawExchange.ResponseBody", raw.ResponseBody}}
}

func errPlaces(err error) []place {
	if err == nil {
		return nil
	}
	return []place{{"câu lỗi", err.Error()}}
}

func nextActionPlaces(na paykit.NextAction) []place {
	out := []place{{"NextAction.URL", na.URL}, {"NextAction.QRData", na.QRData}}
	for k, v := range na.FormFields {
		out = append(out, place{"khoá của NextAction.FormFields", k}, place{"giá trị của NextAction.FormFields", v})
	}
	return out
}

// expectNoPayCode: không chỗ nào trong ps còn mã. Finding nêu thao tác, tình huống và chỗ lộ; không bao giờ in mã,
// văn bản bị lộ hay câu lỗi.
func (e *env) expectNoPayCode(what string, code string, ps []place) {
	reported := map[string]bool{}
	for _, p := range ps {
		if !reported[p.name] && leaks(p.text, code) {
			reported[p.name] = true
			e.fail("%s trả %s còn mã thanh toán (thô, JSON-escape, URL-encode hay khác hoa thường); %s; %s", what, p.name, codeAdvice, codeHarm)
		}
	}
}

// checkCHG2: Charge mang mã thanh toán khách đưa ra để quét (từ Harness.ChargeCode, không có thì mã canh) ở ModeNormal,
// ModeReject, ModeGarbage, rồi truy vấn giao dịch đã thu nếu connector khai truy vấn. Mã không được nằm trong
// RawExchange, câu lỗi, ProviderRef, NextAction, mã/lời nhắn của đối tác, hay log của connector (LogReporter).
// Lỗi Charge trả về không phải vi phạm của CHG-2 (loại lỗi thuộc CHG-1).
func checkCHG2(e *env) {
	ch, _ := e.c.(connector.Charger)
	defer e.p.SetMode(ModeNormal)
	var codes []string
	for _, m := range []Mode{ModeNormal, ModeReject, ModeGarbage} {
		situation := "ModeNormal, đối tác trả lời bình thường"
		switch m {
		case ModeReject:
			situation = "ModeReject, đối tác từ chối"
		case ModeGarbage:
			situation = "ModeGarbage, đối tác trả thân không đọc được"
		}
		what := "Charge (" + situation + ")"
		e.p.SetMode(m)
		tx := e.tx()
		req := e.chargeReq(tx)
		if req.PaymentCode == "" {
			req.PaymentCode = chargeCodeCanary
			e.usePayCode(chargeCodeCanary)
		}
		codes = append(codes, req.PaymentCode)
		ctx, cancel := normalCtx()
		var res connector.ChargeResult
		var err error
		ok := e.call("Charge", callLimit+time.Second, func() { res, err = ch.Charge(ctx, req) })
		cancel()
		if !ok {
			continue
		}
		ps := append(rawPlaces(res.Raw), errPlaces(err)...)
		ps = append(ps, place{"ProviderRef", res.ProviderRef})
		ps = append(ps, nextActionPlaces(res.NextAction)...)
		e.expectNoPayCode(what, req.PaymentCode, ps)

		if m != ModeNormal || err != nil || !e.ops[connector.OpQueryPayment] {
			continue
		}
		e.p.SetPaymentStatus(tx, connector.StatusSucceeded, req.Money)
		st, qok, qerr := e.query(connector.QueryPaymentRequest{TransactionCode: tx, ProviderRef: res.ProviderRef, CreatedAt: txCreatedAt})
		if !qok {
			continue
		}
		qs := append(rawPlaces(st.Raw), errPlaces(qerr)...)
		qs = append(qs, place{"ProviderRef", st.ProviderRef}, place{"ProviderCode", st.ProviderCode},
			place{"ProviderMessage", st.ProviderMessage})
		e.expectNoPayCode("QueryPayment sau Charge mang mã", req.PaymentCode, qs)
	}

	lr, has := e.p.(LogReporter)
	if !has { // Check chỉ đưa lý do bỏ vào Skipped khi lượt không có finding
		e.skipf("phần log không kiểm được (chỉ kiểm giá trị trả về): Partner cần cài conformance.LogReporter")
		return
	}
	var logs string
	if g := guard(callLimit, func() { logs = lr.ConnectorLogs() }); g != nil {
		e.skipf("phần log không kiểm được: Partner.ConnectorLogs (conformance.LogReporter) không trả về hoặc panic")
		return
	}
	for _, c := range codes {
		if leaks(logs, c) {
			e.fail("log của connector (LogReporter) còn mã thanh toán (thô, JSON-escape, URL-encode hay khác hoa thường); cần không bao giờ ghi mã vào log ở mức nào; log của connector được thu gom và giữ lâu: mã mang quyền trừ tiền của khách nằm lại ở đó")
			return
		}
	}
}
