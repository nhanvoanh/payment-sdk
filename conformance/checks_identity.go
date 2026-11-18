package conformance

import (
	"errors"
	"regexp"
	"strings"
	"time"

	"bitbucket.org/hasaki-tech/payment-connector-sdk/connector"
	"bitbucket.org/hasaki-tech/payment-connector-sdk/paykit"
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

// checkID4: mã đơn gửi đối tác dựng TẤT ĐỊNH từ TransactionCode + CreatedAt, không từ đồng hồ lúc gọi. Ba lần
// CreatePayment, không chờ: (1) và (2) cùng request liên tiếp phải gửi CÙNG mã (bắt số ngẫu nhiên, nano giây, bộ
// đếm); mã không được mang ngày HÔM NAY (yyyymmdd/yymmdd ở UTC hay UTC+7) khi CreatedAt ở ngày khác; (3) cùng
// request nhưng CreatedAt lệch ba ngày: nếu mã (1) mang ngày của CreatedAt (dạng nhận ra được) thì mã (3) phải mang
// ngày mới (chứng tỏ ngày lấy từ CreatedAt); mã không mang ngày nhận ra được thì (3) không bị xét.
func checkID4(e *env) {
	rec, ok := e.p.(RequestRecorder)
	if !ok {
		e.skipf("đối tác giả không cài conformance.RequestRecorder")
		return
	}
	e.p.SetMode(ModeNormal)
	req := e.createReq(e.tx())
	later := req
	later.CreatedAt = req.CreatedAt.Add(72 * time.Hour)
	before := len(rec.OrderCodes())
	for i, r := range []connector.CreatePaymentRequest{req, req, later} {
		// Panic hay treo khi tạo (lần hai: cùng khoá) thuộc PAY-1/PAY-7, không báo trùng ở đây.
		ctx, cancel := normalCtx()
		g := guard(callLimit+time.Second, func() { _, _ = e.c.CreatePayment(ctx, r) })
		cancel()
		if g != nil {
			e.skipf("CreatePayment lần %d không xong (%s), xem PAY-1 và PAY-7", i+1, g.Error())
			return
		}
	}
	codes := rec.OrderCodes()[before:]
	if len(codes) != 3 {
		e.notCheckedf("đối tác giả ghi %d mã đơn sau ba lần CreatePayment, cần 3 (connector có thể tự trả lại kết quả theo khoá idempotency)", len(codes))
		return
	}
	if codes[0] != codes[1] {
		e.fail("hai lần CreatePayment liên tiếp cùng request (cùng TransactionCode, CreatedAt) gửi đối tác hai mã đơn khác nhau %q và %q; cần dựng mã chỉ từ TransactionCode và CreatedAt, không từ đồng hồ, số ngẫu nhiên hay bộ đếm; truy vấn/huỷ một lệnh tạo chưa rõ kết quả sẽ hỏi sai mã, đối tác báo không thấy và core cho hết hạn một đơn khách đã trả", codes[0], codes[1])
		return
	}
	if day := todayIn(codes[0], req.CreatedAt); day != "" {
		e.fail("mã đơn %q mang ngày hôm nay %q trong khi CreatedAt là %s; cần lấy ngày từ CreatedAt, không từ đồng hồ lúc gọi; truy vấn sau nửa đêm sẽ hỏi sai mã và đối tác báo không thấy đơn khách đã trả", codes[0], day, req.CreatedAt.Format(time.RFC3339Nano))
		return
	}
	if dateIn(codes[0], req.CreatedAt) != "" && dateIn(codes[2], later.CreatedAt) == "" {
		e.fail("mã đơn %q mang ngày của CreatedAt, nhưng với CreatedAt lệch ba ngày (%s) connector gửi %q, không mang ngày mới; cần lấy ngày từ CreatedAt của request; truy vấn giao dịch tạo hôm khác sẽ hỏi sai mã", codes[0], later.CreatedAt.Format(time.RFC3339Nano), codes[2])
	}
}

var (
	vnZone      = time.FixedZone("UTC+7", 7*3600)
	dateLayouts = []string{"20060102", "060102"}
)

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
func checkID5(e *env) {
	// Bước chuẩn bị: một giao dịch tạo bình thường (CreatedAt có), để lỗi chung của Create thuộc về PAY-1.
	tx := e.tx()
	if _, ok := e.createNormal(tx); !ok {
		return
	}
	rec, hasRec := e.p.(RequestRecorder)
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
	zero := time.Time{}
	switch {
	case err == nil:
		if dateIn(strings.Join(sent, " "), zero) != "" {
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
