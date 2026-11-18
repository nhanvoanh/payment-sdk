package conformance

import (
	"regexp"

	"bitbucket.org/hasaki-tech/payment-connector-sdk/connector"
	"bitbucket.org/hasaki-tech/payment-connector-sdk/paykit"
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
