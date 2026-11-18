package conformance

import (
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

// checkID4: mã đơn gửi đối tác dựng TẤT ĐỊNH từ TransactionCode + CreatedAt, không từ đồng hồ lúc gọi. Hai lần
// CreatePayment với cùng request, cách nhau qua một ranh giới giây của đồng hồ, phải gửi cùng một mã; mã cũng
// không được mang ngày HÔM NAY (dạng yymmdd/yyyymmdd ở UTC hay UTC+7) khi CreatedAt ở ngày khác.
func checkID4(e *env) {
	rec, ok := e.p.(RequestRecorder)
	if !ok {
		e.skipf("đối tác giả không cài conformance.RequestRecorder")
		return
	}
	e.p.SetMode(ModeNormal)
	req := e.createReq(e.tx())
	before := len(rec.OrderCodes())
	for i := 0; i < 2; i++ {
		if i == 1 {
			// Qua hẳn một ranh giới giây: mã mang giờ:phút:giây lúc gọi sẽ khác.
			now := time.Now()
			time.Sleep(now.Truncate(time.Second).Add(time.Second + 20*time.Millisecond).Sub(now))
		}
		// Panic hay treo khi tạo (lần hai: cùng khoá) thuộc PAY-1/PAY-7, không báo trùng ở đây.
		ctx, cancel := normalCtx()
		g := guard(callLimit+time.Second, func() { _, _ = e.c.CreatePayment(ctx, req) })
		cancel()
		if g != nil {
			e.skipf("CreatePayment lần %d không xong (%s), xem PAY-1 và PAY-7", i+1, g.Error())
			return
		}
	}
	codes := rec.OrderCodes()[before:]
	if len(codes) != 2 {
		e.skipf("đối tác giả ghi %d mã đơn sau hai lần CreatePayment, cần 2 (xem RequestRecorder)", len(codes))
		return
	}
	if codes[0] != codes[1] {
		e.fail("hai lần CreatePayment cùng request (cùng TransactionCode, CreatedAt) gửi đối tác hai mã đơn khác nhau %q và %q; cần dựng mã chỉ từ TransactionCode và CreatedAt, không từ đồng hồ lúc gọi; truy vấn/huỷ một lệnh tạo chưa rõ kết quả sẽ hỏi sai mã, đối tác báo không thấy và core cho hết hạn một đơn khách đã trả", codes[0], codes[1])
		return
	}
	if day := todayIn(codes[0], req.CreatedAt); day != "" {
		e.fail("mã đơn %q mang ngày hôm nay %q trong khi CreatedAt là %s; cần lấy ngày từ CreatedAt, không từ đồng hồ lúc gọi; truy vấn sau nửa đêm sẽ hỏi sai mã và đối tác báo không thấy đơn khách đã trả", codes[0], day, req.CreatedAt.Format(time.RFC3339Nano))
	}
}

// todayIn trả dạng ngày hôm nay (yyyymmdd, yymmdd ở UTC hay UTC+7) có trong code mà ngày của createdAt không có.
func todayIn(code string, createdAt time.Time) string {
	vn := time.FixedZone("UTC+7", 7*3600)
	created := map[string]bool{}
	for _, loc := range []*time.Location{time.UTC, vn} {
		for _, f := range []string{"20060102", "060102"} {
			created[createdAt.In(loc).Format(f)] = true
		}
	}
	now := time.Now()
	for _, loc := range []*time.Location{time.UTC, vn} {
		for _, f := range []string{"20060102", "060102"} {
			if d := now.In(loc).Format(f); !created[d] && strings.Contains(code, d) {
				return d
			}
		}
	}
	return ""
}
