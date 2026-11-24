package conformance

import (
	"errors"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/nhanvoanh/payment-sdk/connector"
	"github.com/nhanvoanh/payment-sdk/paykit"
)

// Thời điểm tạo mà mọi request của bộ luật mang (như core gửi: UTC, micro giây). Cố ý ở quá khứ, sát nửa đêm
// UTC (đã sang ngày hôm sau ở UTC+7), và lệnh hoàn ở ngày khác: mã dựng từ đồng hồ lúc gọi sẽ lộ ra ở ID-4,
// mã dựng từ CreatedAt thì truy vấn/huỷ/hoàn ở các luật khác tìm lại được đúng giao dịch.
var (
	txCreatedAt     = time.Date(2021, 3, 4, 23, 59, 58, 123456000, time.UTC)
	refundCreatedAt = time.Date(2021, 3, 6, 8, 15, 0, 654321000, time.UTC)
)

var nameRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

// checkID1: tên là khoá cấu hình và khoá khử trùng webhook (đối tác, EventID) của core.
func checkID1(e *env) {
	var n1, n2 string
	if !e.call("Name()", callLimit, func() { n1, n2 = e.c.Name(), e.c.Name() }) {
		return
	}
	if !nameRe.MatchString(n1) {
		e.fail("Name() trả %q; cần khớp ^[a-z][a-z0-9_]{0,31}$; core không nạp được connector, mọi thanh toán qua đối tác này đều không chạy", n1)
	}
	if n1 != n2 {
		e.fail("Name() trả %q rồi %q; cần không đổi; core khử trùng webhook theo tên đối tác nên sự kiện có thể bị áp hai lần", n1, n2)
	}
}

// checkID2: core chọn connector theo phương thức thanh toán.
func checkID2(e *env) {
	var ms []paykit.Method
	if !e.call("Methods()", callLimit, func() { ms = e.c.Methods() }) {
		return
	}
	if len(ms) == 0 {
		e.fail("Methods() rỗng; cần ít nhất một phương thức; core không định tuyến được giao dịch nào tới connector")
		return
	}
	seen := map[paykit.Method]bool{}
	for _, m := range ms {
		if seen[m] {
			e.fail("Methods() có %q hai lần; cần không trùng; cấu hình định tuyến của core bị nhập nhằng", m)
		}
		seen[m] = true
	}
	if !seen[e.h.Method] {
		e.fail("Methods() = %v không có %q (Harness.Method); cần có; giao dịch theo phương thức này sẽ không tới được connector", ms, e.h.Method)
	}
}

// checkID3: core tin Supports; khai mà không cài thì thao tác tiền (huỷ, hoàn) hỏng lúc chạy.
func checkID3(e *env) {
	for _, op := range connector.AllOperations() {
		var s bool
		if !e.call("Supports("+string(op)+")", callLimit, func() { s = connector.Supports(e.c, op) }) {
			return
		}
		if s && !implements(e.orig, op) {
			e.fail("Supports(%s) đúng nhưng connector không cài interface tương ứng (connector.As… sai); cần chỉ khai thao tác đã cài; core gọi thao tác này sẽ nhận ErrNotSupported và lệnh huỷ/hoàn tiền của khách không chạy được", op)
		}
	}
}

// clockStep: chờ giữa lần gọi 2 và 3 của ID-4 (chỉ lượt thẳng) để giây của đồng hồ chắc chắn đổi.
const clockStep = 1100 * time.Millisecond

// checkID4: mã gửi đối tác dựng TẤT ĐỊNH từ TransactionCode/RefundCode + CreatedAt, không từ đồng hồ lúc gọi. Bốn lần
// CreatePayment: (1) và (2) cùng request liên tiếp phải gửi CÙNG mã đơn (bắt số ngẫu nhiên, nano giây, bộ đếm); chờ
// ~1,1 giây (chỉ lượt thẳng, lượt /grpc không chờ cho nhanh); (3) cùng request phải gửi lại đúng mã (1) (bắt
// giờ-phút-giây lấy từ đồng hồ); mã không được mang ngày HÔM NAY (yyyymmdd/yymmdd ở UTC hay UTC+7) khi CreatedAt ở
// ngày khác; (4) cùng request nhưng CreatedAt lệch ba ngày: nếu mã (1) mang ngày của CreatedAt (dạng nhận ra được)
// thì mã (4) phải mang ngày mới. Có refund và Partner cài RefundRecorder: hai lần CreateRefund cùng request, một
// trước một sau lúc chờ, phải gửi cùng mã hoàn.
func checkID4(e *env) {
	rec, ok := e.p.(RequestRecorder)
	if !ok {
		e.skipf("đối tác giả không cài conformance.RequestRecorder")
		return
	}
	e.p.SetMode(ModeNormal)
	tx := e.tx()
	req := e.createReq(tx)
	later := req
	later.CreatedAt = req.CreatedAt.Add(72 * time.Hour)
	before := len(rec.OrderCodes())
	var ref string
	create := func(i int, r connector.CreatePaymentRequest) bool {
		// Panic hay treo khi tạo (lần sau: cùng khoá) thuộc PAY-1/PAY-7, không báo trùng ở đây.
		ctx, cancel := normalCtx()
		defer cancel()
		var res connector.CreatePaymentResult
		if g := guard(callLimit+time.Second, func() { res, _ = e.c.CreatePayment(ctx, r) }); g != nil {
			e.skipf("CreatePayment lần %d không xong (%s), xem PAY-1 và PAY-7", i, g.Error())
			return false
		}
		if i == 1 {
			ref = res.ProviderRef
		}
		return true
	}
	if !create(1, req) || !create(2, req) {
		return
	}
	refunds := e.refundCodes(tx, ref)
	if refunds != nil && !refunds.first() {
		refunds = nil
	}
	if e.srv == nil {
		time.Sleep(clockStep)
	}
	if !create(3, req) || !create(4, later) {
		return
	}
	codes := rec.OrderCodes()[before:]
	if len(codes) != 4 {
		e.notCheckedf("đối tác giả ghi %d mã đơn sau bốn lần CreatePayment, cần 4 (connector có thể tự trả lại kết quả theo khoá idempotency)", len(codes))
		return
	}
	const why = "cần dựng mã chỉ từ TransactionCode và CreatedAt, không từ đồng hồ, số ngẫu nhiên hay bộ đếm; truy vấn/huỷ một lệnh tạo chưa rõ kết quả sẽ hỏi sai mã, đối tác báo không thấy và core cho hết hạn một đơn khách đã trả"
	if codes[0] != codes[1] {
		e.fail("hai lần CreatePayment liên tiếp cùng request (cùng TransactionCode, CreatedAt) gửi đối tác hai mã đơn khác nhau %q và %q; %s", codes[0], codes[1], why)
		return
	}
	if codes[2] != codes[0] {
		e.fail("CreatePayment cùng request gọi lại sau %v gửi đối tác mã đơn %q, khác mã lần đầu %q (mã mang giờ của đồng hồ?); %s", clockStep, codes[2], codes[0], why)
		return
	}
	if day := todayIn(codes[0], req.CreatedAt); day != "" {
		e.fail("mã đơn %q mang ngày hôm nay %q trong khi CreatedAt là %s; cần lấy ngày từ CreatedAt, không từ đồng hồ lúc gọi; truy vấn sau nửa đêm sẽ hỏi sai mã và đối tác báo không thấy đơn khách đã trả", codes[0], day, req.CreatedAt.Format(time.RFC3339Nano))
		return
	}
	if dateIn(codes[0], req.CreatedAt) != "" && dateIn(codes[3], later.CreatedAt) == "" {
		e.fail("mã đơn %q mang ngày của CreatedAt, nhưng với CreatedAt lệch ba ngày (%s) connector gửi %q, không mang ngày mới; cần lấy ngày từ CreatedAt của request; truy vấn giao dịch tạo hôm khác sẽ hỏi sai mã", codes[0], later.CreatedAt.Format(time.RFC3339Nano), codes[3])
		return
	}
	if refunds != nil {
		refunds.second()
	}
}

// refundProbe là lượt mã hoàn của ID-4: hai lần CreateRefund cùng request, so mã đối tác giả nhận.
type refundProbe struct {
	e      *env
	rec    RefundRecorder
	req    connector.RefundRequest
	before int
}

// refundCodes chuẩn bị lượt mã hoàn; nil khi connector không khai refund, hoặc Partner không cài RefundRecorder
// (khi đó luật vào Skipped với lý do, nếu không có vi phạm khác).
func (e *env) refundCodes(tx, ref string) *refundProbe {
	if !e.ops[connector.OpRefund] {
		return nil
	}
	rec, ok := e.p.(RefundRecorder)
	if !ok {
		e.skipf("lượt mã hoàn tiền không kiểm được: Partner cần cài conformance.RefundRecorder")
		return nil
	}
	e.p.SetPaymentStatus(tx, connector.StatusSucceeded, e.h.Amount)
	return &refundProbe{e: e, rec: rec, req: e.refundReq(tx, ref, "R"+tx), before: len(rec.RefundCodes())}
}

func (r *refundProbe) call(i int) bool {
	rf, _ := r.e.c.(connector.Refunder)
	ctx, cancel := normalCtx()
	defer cancel()
	var err error
	if g := guard(callLimit+time.Second, func() { _, err = rf.CreateRefund(ctx, r.req) }); g != nil {
		r.e.skipf("CreateRefund lần %d không xong (%s), xem RFD-1", i, g.Error())
		return false
	}
	if err != nil {
		r.e.skipf("CreateRefund lần %d lỗi (%s) nên không so được mã hoàn, xem RFD-1", i, errText(err))
		return false
	}
	return true
}

func (r *refundProbe) first() bool { return r.call(1) }

func (r *refundProbe) second() {
	if !r.call(2) {
		return
	}
	codes := r.rec.RefundCodes()[r.before:]
	if len(codes) != 2 {
		r.e.notCheckedf("đối tác giả ghi %d mã hoàn sau hai lần CreateRefund, cần 2 (connector có thể tự trả lại kết quả theo khoá idempotency)", len(codes))
		return
	}
	if codes[0] != codes[1] {
		r.e.fail("hai lần CreateRefund cùng request (cùng RefundCode, CreatedAt) gửi đối tác hai mã hoàn khác nhau %q và %q; cần dựng mã chỉ từ RefundCode và CreatedAt; truy vấn một lệnh hoàn chưa rõ kết quả sẽ hỏi sai mã và core có thể hoàn tiền cho khách hai lần", codes[0], codes[1])
	}
}

var (
	vnZone      = time.FixedZone("UTC+7", 7*3600)
	dateLayouts = []string{"20060102", "060102"}
)

// datePos là chỗ ngày nằm trong một mã: tính từ đầu (from) và từ cuối (tail), cùng dạng và múi giờ.
type datePos struct {
	from, tail int
	layout     string
	loc        *time.Location
}

// findDate tìm ngày của t trong code; ok sai nếu không có dạng nhận ra được.
func findDate(code string, t time.Time) (datePos, bool) {
	for _, loc := range []*time.Location{time.UTC, vnZone} {
		for _, f := range dateLayouts {
			if i := strings.Index(code, t.In(loc).Format(f)); i >= 0 {
				return datePos{from: i, tail: len(code) - i, layout: f, loc: loc}, true
			}
		}
	}
	return datePos{}, false
}

// zeroDateAt: code mang ngày 0001-01-01 ở đúng chỗ (tính từ đầu hay từ cuối) mà mã có CreatedAt mang ngày.
func zeroDateAt(code string, p datePos) bool {
	z := time.Time{}.In(p.loc).Format(p.layout)
	at := func(i int) bool { return i >= 0 && i+len(z) <= len(code) && code[i:i+len(z)] == z }
	return at(p.from) || at(len(code)-p.tail)
}

// dateIn trả dạng ngày của t (yyyymmdd hay yymmdd, ở UTC hay UTC+7) có trong code; rỗng nếu không có.
func dateIn(code string, t time.Time) string {
	for _, loc := range []*time.Location{time.UTC, vnZone} {
		for _, f := range dateLayouts {
			if d := t.In(loc).Format(f); strings.Contains(code, d) {
				return d
			}
		}
	}
	return ""
}

// todayIn trả dạng ngày hôm nay có trong code mà ngày của createdAt không có.
func todayIn(code string, createdAt time.Time) string {
	created := map[string]bool{}
	for _, loc := range []*time.Location{time.UTC, vnZone} {
		for _, f := range dateLayouts {
			created[createdAt.In(loc).Format(f)] = true
		}
	}
	now := time.Now()
	for _, loc := range []*time.Location{time.UTC, vnZone} {
		for _, f := range dateLayouts {
			if d := now.In(loc).Format(f); !created[d] && strings.Contains(code, d) {
				return d
			}
		}
	}
	return ""
}

// checkID5: CreatedAt zero (core cũ chưa gửi trường này) không bao giờ được đoán thành một ngày. Tạo với CreatedAt
// zero → hoặc ErrProviderUnavailable (chắc chắn chưa gửi; RequestRecorder không được thấy request nào), hoặc
// thành công với mã KHÔNG mang ngày 0001-01-01 (010101/00010101). Truy vấn (nếu có query_payment) một giao dịch
// đã tạo, đối tác báo đã thu, với CreatedAt zero → Found đúng hoặc ErrUnknownOutcome; Found=false là vi phạm.
// Ngày 0001-01-01 được dò ở ĐÚNG chỗ mà mã của lệnh tạo có CreatedAt mang ngày (tính từ đầu hay từ cuối), không
// theo chuỗi con bất kỳ: hằng số hay mã giao dịch có sẵn "010101" không phải ngày đoán ra.
func checkID5(e *env) {
	// Bước chuẩn bị: một giao dịch tạo bình thường (CreatedAt có), để lỗi chung của Create thuộc về PAY-1.
	tx := e.tx()
	rec, hasRec := e.p.(RequestRecorder)
	var setupBefore int
	if hasRec {
		setupBefore = len(rec.OrderCodes())
	}
	if _, ok := e.createNormal(tx); !ok {
		return
	}
	// Chỗ ngày nằm trong mã của lệnh tạo có CreatedAt; mã không mang ngày thì không có gì để đoán sai.
	var pos datePos
	var dated bool
	if hasRec {
		if setup := rec.OrderCodes()[setupBefore:]; len(setup) > 0 {
			pos, dated = findDate(setup[0], txCreatedAt)
		}
	}
	req := e.createReq(e.tx())
	req.CreatedAt = time.Time{}
	var before int
	if hasRec {
		before = len(rec.OrderCodes())
	}
	ctx, cancel := normalCtx()
	_, ok, err := e.create(ctx, callLimit+time.Second, req)
	cancel()
	if !ok {
		return
	}
	var sent []string
	if hasRec {
		sent = rec.OrderCodes()[before:]
	}
	switch {
	case err == nil:
		if dated && slices.ContainsFunc(sent, func(c string) bool { return zeroDateAt(c, pos) }) {
			e.fail("CreatePayment với CreatedAt zero gửi đối tác mã %v mang ngày 0001-01-01; cần trả ErrProviderUnavailable (core chưa gửi CreatedAt — nâng core trước connector) hoặc dựng mã không mang ngày; sau khi nâng core, truy vấn dựng mã với ngày thật, đối tác báo không thấy và core cho hết hạn một đơn khách đã trả", sent)
		}
	case connector.Classify(err) == connector.CallUnavailable && !errors.Is(err, connector.ErrNotSupported):
		if len(sent) > 0 {
			e.fail("CreatePayment với CreatedAt zero trả ErrProviderUnavailable nhưng đối tác đã nhận mã %v; ErrProviderUnavailable chỉ dành cho khi CHẮC CHẮN chưa gửi; core chốt thất bại trong khi đối tác có thể đã thu", sent)
		}
	default:
		e.fail("CreatePayment với CreatedAt zero trả %v; cần ErrProviderUnavailable (chưa gửi gì) hoặc thành công với mã không mang ngày; lỗi khác khiến core giữ giao dịch chờ và hỏi lại với cùng CreatedAt zero", errText(err))
	}
	if !e.ops[connector.OpQueryPayment] {
		return
	}
	e.p.SetPaymentStatus(tx, connector.StatusSucceeded, e.h.Amount)
	st, ok, qerr := e.query(connector.QueryPaymentRequest{TransactionCode: tx})
	if !ok {
		return
	}
	switch {
	case qerr == nil && !st.Found:
		e.fail("QueryPayment với CreatedAt zero cho giao dịch đã tạo (đối tác báo đã thu) trả Found=false; cần ErrUnknownOutcome khi không dựng được mã đã gửi (hoặc Found đúng nếu mã không phụ thuộc ngày); core có thể cho hết hạn một đơn khách đã trả")
	case qerr != nil && !errors.Is(qerr, connector.ErrUnknownOutcome):
		e.fail("QueryPayment với CreatedAt zero trả %v; cần ErrUnknownOutcome (không dựng được mã đã gửi) hoặc Found đúng", errText(qerr))
	}
}
