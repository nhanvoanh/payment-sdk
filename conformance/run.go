package conformance

import (
	"strings"
	"testing"

	"bitbucket.org/hasaki-tech/payment-connector-sdk/connector"
)

// rule là một luật; op (và alsoOp) khác rỗng nghĩa là chỉ áp khi connector khai thao tác đó.
type rule struct {
	id            string
	op            connector.Operation
	alsoOp        connector.Operation
	createPartner bool   // cần Create gọi đối tác (Harness.CreateCallsPartner)
	grpcSkip      string // khác rỗng: lượt /grpc theo thiết kế không kiểm được gì (vào NotChecked với lý do này)
	check         func(e *env)
}

// rules theo thứ tự cố định; mã luật là hợp đồng với developer, xem README.md.
var rules = []rule{
	{id: "ID-1", check: checkID1},
	{id: "ID-2", check: checkID2},
	{id: "ID-3", check: checkID3},
	{id: "PAY-1", check: checkPAY1},
	{id: "PAY-2", check: checkPAY2},
	{id: "PAY-3", createPartner: true, check: checkPAY3},
	{id: "PAY-4", createPartner: true, check: checkPAY4},
	{id: "PAY-5", createPartner: true, check: checkPAY5},
	{id: "PAY-6", createPartner: true, check: checkPAY6},
	{id: "PAY-7", check: checkPAY7},
	{id: "QRY-1", op: connector.OpQueryPayment, check: checkQRY1},
	{id: "QRY-2", op: connector.OpQueryPayment, check: checkQRY2},
	{id: "QRY-3", op: connector.OpQueryPayment, check: checkQRY3},
	{id: "QRY-4", op: connector.OpQueryPayment, check: checkQRY4},
	{id: "CHG-1", op: connector.OpCharge, check: checkCHG1},
	{id: "CXL-1", op: connector.OpCancel, check: checkCXL1},
	{id: "CXL-2", op: connector.OpCancel, check: checkCXL2},
	{id: "CXL-3", op: connector.OpCancel, check: checkCXL3},
	{id: "CXL-4", op: connector.OpCancel, alsoOp: connector.OpQueryPayment, check: checkCXL4},
	{id: "WHK-1", op: connector.OpWebhook, check: checkWHK1},
	{id: "WHK-2", op: connector.OpWebhook, check: checkWHK2},
	{id: "WHK-3", op: connector.OpWebhook, check: checkWHK3},
	{id: "WHK-4", op: connector.OpWebhook, check: checkWHK4},
	{id: "WHK-5", op: connector.OpWebhook, check: checkWHK5},
	{id: "WHK-6", op: connector.OpWebhook, check: checkWHK6,
		grpcSkip: "proxy gRPC trả ack từ bảng lấy một lần lúc Describe, không gọi lại connector nên không kiểm được tính thuần"},
	{id: "RFD-1", op: connector.OpRefund, check: checkRFD1},
	{id: "RFD-2", op: connector.OpQueryRefund, check: checkRFD2},
	{id: "RFD-3", op: connector.OpQueryRefund, check: checkRFD3},
	{id: "LOG-1", check: checkLOG1},
	{id: "CTX-1", check: checkCTX1,
		grpcSkip: "proxy gRPC gặp ctx đã huỷ thì trả về ngay mà không gửi, connector không nhận được lời gọi nào để kiểm"},
}

const grpcSuffix = "/grpc"

// Check chạy mọi luật, mỗi luật hai lượt (thẳng và qua gRPC), mỗi lượt một cặp connector/đối tác mới.
// Không bao giờ đánh trượt tb: mọi vi phạm nằm trong Report; tb chỉ để đăng ký dọn dẹp.
func Check(tb testing.TB, h Harness) Report {
	var rep Report
	for _, r := range rules {
		for _, viaGRPC := range []bool{false, true} {
			code := r.id
			if viaGRPC {
				code += grpcSuffix
			}
			findings, skip, notChecked := runOne(tb, h, r, code, viaGRPC)
			if notChecked != "" {
				rep.NotChecked = append(rep.NotChecked, code+": "+notChecked)
				continue
			}
			for _, d := range findings {
				rep.Findings = append(rep.Findings, Finding{Rule: code, Detail: d})
			}
			if skip != "" && len(findings) == 0 {
				rep.Skipped = append(rep.Skipped, code+": "+skip)
			}
		}
	}
	return rep
}

// runOne chạy một lượt của một luật; h.New và grpctest.New chạy trên goroutine của test vì có thể gọi tb.Fatal.
// Trả (finding, lý do bỏ, lý do không kiểm theo thiết kế).
func runOne(tb testing.TB, h Harness, r rule, code string, viaGRPC bool) ([]string, string, string) {
	if r.createPartner && !h.CreateCallsPartner {
		return nil, "Create không gọi đối tác (Harness.CreateCallsPartner sai)", ""
	}
	c, p := h.New(tb)
	e := &env{h: h, c: c, orig: c, p: p, rule: code, ops: declared(c)}
	for _, op := range []connector.Operation{r.op, r.alsoOp} {
		if op != "" && !e.ops[op] {
			return nil, "connector không khai thao tác " + string(op), ""
		}
	}
	if viaGRPC && r.grpcSkip != "" {
		return nil, "", r.grpcSkip
	}
	if viaGRPC {
		client, srv, err := loopback(tb, c)
		e.srv = srv
		if err != nil {
			if r.id != "ID-1" {
				return nil, "không dựng được kênh gRPC vì Describe lỗi (xem ID-1/grpc): " + err.Error(), ""
			}
			e.fail("Describe qua gRPC lỗi: %v; cần mô tả được (tên không đổi, đủ webhook_acks hợp lệ, không panic); core không nạp được connector, mọi thanh toán qua đối tác này đều không chạy", err)
		} else {
			e.c = client
		}
	}
	if len(e.findings) == 0 {
		if err := guard(ruleLimit, func() { r.check(e) }); err != nil {
			e.fail("luật không chạy xong: %v; connector phải trả về trong hạn và không panic", err)
		}
	}
	e.mu.Lock()
	skipped := e.skip != ""
	e.mu.Unlock()
	// Luật bị bỏ vì bước chuẩn bị hỏng: panic (nếu có) là của bước chuẩn bị, đã thuộc về PAY-1.
	if e.srv != nil && !skipped {
		for _, pm := range e.srv.drain() {
			e.fail("connector panic khi chạy qua gRPC (%s); cần trả lỗi thay vì panic; core nhận lời gọi không rõ kết quả và giao dịch của khách bị kẹt", pm)
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.findings...), e.skip, ""
}

// Run chạy Check và báo mỗi lượt của mỗi luật thành một subtest: vi phạm là t.Errorf, luật bị bỏ là
// t.Skip. Lượt không kiểm được theo thiết kế (NotChecked) chỉ được t.Log, không thành subtest.
func Run(t *testing.T, h Harness) {
	t.Helper()
	rep := Check(t, h)
	notChecked := map[string]bool{}
	for _, s := range rep.NotChecked {
		code, _, _ := strings.Cut(s, ": ")
		notChecked[code] = true
		t.Log("không kiểm theo thiết kế: " + s)
	}
	for _, r := range rules {
		for _, code := range []string{r.id, r.id + grpcSuffix} {
			if notChecked[code] {
				continue
			}
			t.Run(code, func(t *testing.T) {
				for _, f := range rep.Findings {
					if f.Rule == code {
						t.Errorf("%s: %s", f.Rule, f.Detail)
					}
				}
				for _, s := range rep.Skipped {
					if reason, ok := strings.CutPrefix(s, code+": "); ok {
						t.Skip(reason)
					}
				}
			})
		}
	}
}
