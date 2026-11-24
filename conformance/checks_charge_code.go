package conformance

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"strings"

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

// minEncodedFrag: mảnh base64 ngắn hơn không được dùng để dò (tránh báo nhầm với mã rất ngắn).
const minEncodedFrag = 8

// leaks báo text còn một dạng nào của code: thô; JSON-escape, URL-encode, khác hoa thường (bộ so của LOG-1); base64
// (Std/URL, có hay không đệm, cả khi mã nằm giữa một khối base64 lớn hơn); hex (thường/hoa).
func leaks(text, code string) bool {
	if text == "" || code == "" {
		return false
	}
	if strings.Contains(text, code) || redact.Contains(text, code) {
		return true
	}
	for _, f := range encodedForms(code) {
		if strings.Contains(text, f) {
			return true
		}
	}
	return false
}

// encodedForms trả các chuỗi mà một dạng mã hoá đảo ngược được của code chắc chắn chứa. Với base64, mã đứng sau k byte
// bất kỳ (k = 0, 1, 2 theo mod 3) thì chỉ các nhóm 4 ký tự mã hoá trọn byte của mã là cố định: lấy đúng đoạn đó.
func encodedForms(code string) []string {
	h := hex.EncodeToString([]byte(code))
	out := []string{h, strings.ToUpper(h)}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.URLEncoding} {
		for k := 0; k < 3; k++ {
			data := append(make([]byte, k), code...)
			full := enc.EncodeToString(data)
			start, end := 4*((k+2)/3), 4*(len(data)/3)
			if end-start >= minEncodedFrag {
				out = append(out, full[start:end])
			}
			if k == 0 && len(full) >= minEncodedFrag {
				out = append(out, full, strings.TrimRight(full, "="))
			}
		}
	}
	return out
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
			e.fail("%s trả %s còn mã thanh toán (thô, JSON-escape, URL-encode, khác hoa thường, base64 hay hex); %s; %s", what, p.name, codeAdvice, codeHarm)
		}
	}
}

// checkCHG2: Charge mang mã thanh toán khách đưa ra để quét (từ Harness.ChargeCode, không có thì mã canh) ở mọi chế độ
// của đối tác giả (ModeNormal, ModeDown, ModeHang, ModeReject, ModeGarbage), rồi truy vấn giao dịch đã thu nếu
// connector khai truy vấn. Mã (mọi dạng của leaks) không được nằm trong RawExchange, câu lỗi, ProviderRef, NextAction,
// mã/lời nhắn của đối tác, hay log của connector (LogReporter). Lỗi Charge trả về không phải vi phạm của CHG-2 (loại
// lỗi thuộc CHG-1). ModeDown và ModeHang quan trọng: lỗi vận chuyển của net/http mang nguyên URL kèm query.
func checkCHG2(e *env) {
	ch, _ := e.c.(connector.Charger)
	defer e.p.SetMode(ModeNormal)
	var codes, gaps []string
	for _, m := range []Mode{ModeNormal, ModeDown, ModeHang, ModeReject, ModeGarbage} {
		situation := map[Mode]string{
			ModeNormal:  "ModeNormal, đối tác trả lời bình thường",
			ModeDown:    "ModeDown, đối tác từ chối kết nối",
			ModeHang:    "ModeHang, đối tác treo",
			ModeReject:  "ModeReject, đối tác từ chối",
			ModeGarbage: "ModeGarbage, đối tác trả thân không đọc được",
		}[m]
		what := "Charge (" + situation + ")"
		tx := e.tx()
		req := e.chargeReq(tx)
		if req.PaymentCode == "" {
			req.PaymentCode = chargeCodeCanary
			e.usePayCode(chargeCodeCanary)
		}
		codes = append(codes, req.PaymentCode)
		var res connector.ChargeResult
		// modeCall đặt chế độ, hạn ctx và hạn lời gọi như expectUnknown (ModeHang: 300 ms, lượt /grpc bọc ctx handler).
		ok, err := e.modeCall(m, "Charge", func(ctx context.Context) error {
			var cerr error
			res, cerr = ch.Charge(ctx, req)
			return cerr
		})
		if !ok {
			continue // treo hay panic: finding đã có; res bị goroutine bỏ mặc có thể còn ghi
		}
		ps := append(rawPlaces(res.Raw), errPlaces(err)...)
		ps = append(ps, place{"ProviderRef", res.ProviderRef})
		ps = append(ps, nextActionPlaces(res.NextAction)...)
		e.expectNoPayCode(what, req.PaymentCode, ps)

		if m != ModeNormal {
			continue
		}
		if err != nil {
			legs := "kết quả thành công"
			if e.ops[connector.OpQueryPayment] {
				legs += " và lượt truy vấn sau đó"
			}
			gaps = append(gaps, "Charge mang mã ở ModeNormal trả lỗi, nên "+legs+
				" không kiểm được (đối tác giả có nhận mã từ Harness.ChargeCode, hay mã canh của bộ luật, không?)")
			continue
		}
		if !e.ops[connector.OpQueryPayment] {
			continue
		}
		e.p.SetMode(ModeNormal)
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
	e.p.SetMode(ModeNormal)

	// Lý do bỏ (Skipped) chỉ hiện khi lượt không có finding. Lưu ý: lượt /grpc có lý do bỏ thì runOne không đọc
	// panic phía server gRPC (srv.drain); Charge panic chỉ khi mang mã vẫn bị lượt thẳng báo (e.call), như PAY-8.
	defer func() {
		if len(gaps) > 0 {
			e.skipf("%s", strings.Join(gaps, "; "))
		}
	}()
	lr, has := e.p.(LogReporter)
	if !has {
		gaps = append(gaps, "phần log không kiểm được (chỉ kiểm giá trị trả về): Partner cần cài conformance.LogReporter")
		return
	}
	var logs string
	if g := guard(callLimit, func() { logs = lr.ConnectorLogs() }); g != nil {
		gaps = append(gaps, "phần log không kiểm được: Partner.ConnectorLogs (conformance.LogReporter) không trả về hoặc panic")
		return
	}
	for _, c := range codes {
		if leaks(logs, c) {
			e.fail("log của connector (LogReporter) còn mã thanh toán (thô, JSON-escape, URL-encode, khác hoa thường, base64 hay hex); cần không bao giờ ghi mã vào log ở mức nào; log của connector được thu gom và giữ lâu: mã mang quyền trừ tiền của khách nằm lại ở đó")
			return
		}
	}
}
