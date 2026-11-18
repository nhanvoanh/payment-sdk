package conformance

import (
	"bytes"
	"errors"
	"math/rand/v2"
	"time"

	"bitbucket.org/hasaki-tech/payment-connector-sdk/connector"
)

func (e *env) receiver() connector.WebhookReceiver { w, _ := e.c.(connector.WebhookReceiver); return w }

// cloneRaw sao chép sâu để connector có sửa đầu vào cũng không ảnh hưởng lần gọi sau.
func cloneRaw(r connector.RawWebhook) connector.RawWebhook {
	out := connector.RawWebhook{Body: bytes.Clone(r.Body), RemoteIP: r.RemoteIP}
	if r.Headers != nil {
		out.Headers = map[string][]string{}
		for k, v := range r.Headers {
			out.Headers[k] = append([]string(nil), v...)
		}
	}
	if r.Query != nil {
		out.Query = map[string][]string{}
		for k, v := range r.Query {
			out.Query[k] = append([]string(nil), v...)
		}
	}
	return out
}

func (e *env) parse(raw connector.RawWebhook) (connector.ProviderEvent, bool, error) {
	ctx, cancel := normalCtx()
	defer cancel()
	var ev connector.ProviderEvent
	var err error
	if !e.call("ParseWebhook", callLimit+time.Second, func() { ev, err = e.receiver().ParseWebhook(ctx, cloneRaw(raw)) }) {
		return connector.ProviderEvent{}, false, nil
	}
	return ev, true, err
}

func (e *env) signed(tx string) connector.RawWebhook {
	return e.p.SignedWebhook(tx, "ref_"+tx, connector.StatusSucceeded, e.h.Amount)
}

// parseValid phân tích một webhook hợp lệ; lỗi thì bỏ luật vì WHK-1 đã báo.
func (e *env) parseValid(raw connector.RawWebhook) (connector.ProviderEvent, bool) {
	ev, ok, err := e.parse(raw)
	if !ok {
		return ev, false
	}
	if err != nil {
		e.skipf("ParseWebhook từ chối webhook hợp lệ (xem WHK-1): %v", err)
		return ev, false
	}
	return ev, true
}

func checkWHK1(e *env) {
	tx := e.tx()
	ev, ok, err := e.parse(e.signed(tx))
	if !ok {
		return
	}
	if err != nil {
		e.fail("ParseWebhook từ chối webhook hợp lệ đã ký: %v; cần phân tích được; đối tác gửi lại mãi, khách đã trả tiền mà đơn không được xác nhận", err)
		return
	}
	if ev.EventID == "" {
		e.fail("ParseWebhook trả EventID rỗng; cần khác rỗng; core không khử trùng được, một sự kiện gửi lại có thể bị áp hai lần")
	}
	if ev.Kind != connector.EventKindPayment {
		e.fail("ParseWebhook cho webhook thanh toán trả Kind %q; cần %q; core áp sự kiện vào nhầm loại giao dịch", ev.Kind, connector.EventKindPayment)
	}
	if ev.TransactionCode != tx {
		e.fail("ParseWebhook trả TransactionCode %q; cần %q; core cập nhật nhầm giao dịch của khách khác", ev.TransactionCode, tx)
	}
	if ev.Status != connector.StatusSucceeded {
		e.fail("ParseWebhook cho webhook báo succeeded trả Status %q; cần succeeded; khách đã trả tiền mà đơn không được xác nhận", ev.Status)
	}
}

// checkWHK2: đối tác gửi lại cùng một webhook; EventID phải ổn định để core khử trùng.
func checkWHK2(e *env) {
	raw := e.signed(e.tx())
	ev1, ok := e.parseValid(raw)
	if !ok {
		return
	}
	ev2, ok, err := e.parse(raw)
	if !ok {
		return
	}
	if err != nil {
		e.fail("ParseWebhook lần thứ hai cho cùng một webhook trả %v; cần phân tích được như lần đầu; đối tác gửi lại là chuyện thường, từ chối thì đối tác gửi mãi", err)
		return
	}
	if ev1.EventID != ev2.EventID {
		e.fail("cùng một webhook phân tích hai lần ra EventID %q và %q; cần giống hệt; core coi lần gửi lại là sự kiện mới và có thể áp hai lần (ví dụ hoàn tiền hai lần)", ev1.EventID, ev2.EventID)
	}
}

func checkWHK3(e *env) {
	ev1, ok := e.parseValid(e.signed(e.tx()))
	if !ok {
		return
	}
	ev2, ok := e.parseValid(e.signed(e.tx()))
	if !ok {
		return
	}
	if ev1.EventID == ev2.EventID {
		e.fail("webhook của hai giao dịch khác nhau cùng ra EventID %q; cần khác nhau; core coi sự kiện thứ hai là bản trùng và bỏ qua, khách đã trả tiền mà đơn không được xác nhận", ev1.EventID)
	}
}

// checkWHK4: webhook bị sửa không bao giờ được chấp nhận. Partner cài Tamperer thì nội dung được ký chắc
// chắn đã đổi nên đòi đúng ErrInvalidSignature; không cài thì lật một byte giữa thân, byte đó có thể là
// byte cấu trúc hay không được ký, nên chỉ đòi có lỗi.
func checkWHK4(e *env) {
	signed := e.signed(e.tx())
	t, strict := e.p.(Tamperer)
	var raw connector.RawWebhook
	if strict {
		if g := guard(callLimit, func() { raw = t.TamperedWebhook(cloneRaw(signed)) }); g != nil {
			e.skipf("Partner.TamperedWebhook hỏng: %v", g)
			return
		}
	} else {
		raw = cloneRaw(signed)
		if len(raw.Body) == 0 {
			e.skipf("SignedWebhook trả thân rỗng, không có byte để sửa; hãy cài Tamperer cho Partner")
			return
		}
		raw.Body[len(raw.Body)/2] ^= 0x01
	}
	_, ok, err := e.parse(raw)
	switch {
	case !ok:
	case err == nil:
		e.fail("ParseWebhook chấp nhận webhook đã bị sửa (err nil); cần lỗi, nên là ErrInvalidSignature; kẻ gian giả được webhook báo đã thanh toán và lấy hàng không trả tiền")
	case strict && !errors.Is(err, connector.ErrInvalidSignature):
		e.fail("ParseWebhook với webhook bị sửa nội dung được ký (Partner.TamperedWebhook) trả %v; cần lỗi khớp ErrInvalidSignature; core không phân biệt được webhook giả với lỗi tạm thời, trả lỗi để đối tác gửi lại thay vì từ chối hẳn", err)
	}
}

// checkWHK5: đầu vào hỏng phải ra lỗi, không panic.
func checkWHK5(e *env) {
	valid := e.signed(e.tx())
	big := make([]byte, 2<<20)
	r := rand.New(rand.NewChaCha8([32]byte{7})) // hạt cố định để test tất định
	for i := range big {
		big[i] = byte(r.Uint32())
	}
	emptyBody := cloneRaw(valid)
	emptyBody.Body = nil
	bigBody := cloneRaw(valid)
	bigBody.Body = big
	cases := []struct {
		name string
		raw  connector.RawWebhook
	}{
		{"thân rỗng, header nil", connector.RawWebhook{}},
		{"header hợp lệ, thân rỗng", emptyBody},
		{"header nil, thân 2 MiB ngẫu nhiên", connector.RawWebhook{Body: big}},
		{"header hợp lệ, thân 2 MiB ngẫu nhiên", bigBody},
	}
	for _, c := range cases {
		_, ok, err := e.parse(c.raw)
		if ok && err == nil {
			e.fail("ParseWebhook với %s trả err nil; cần lỗi; core áp một sự kiện không có thật vào giao dịch", c.name)
		}
	}
}

var outcomes = []connector.WebhookOutcome{
	connector.OutcomeApplied, connector.OutcomeDuplicate, connector.OutcomeIgnored, connector.OutcomeRejected,
}

// checkWHK6: core dựng bảng phản hồi một lần lúc nạp connector, nên AckWebhook phải là hàm thuần.
func checkWHK6(e *env) {
	w := e.receiver()
	for _, o := range outcomes {
		var s1, s2 int
		var b1, b2 []byte
		var c1, c2 string
		if !e.call("AckWebhook("+string(o)+")", callLimit, func() {
			s1, b1, c1 = w.AckWebhook(o)
			s2, b2, c2 = w.AckWebhook(o)
		}) {
			return
		}
		if s1 < 100 || s1 > 599 {
			e.fail("AckWebhook(%s) trả status %d; cần 100–599; core không nạp được connector, mọi webhook của đối tác bị từ chối", o, s1)
		}
		if s1 != s2 || !bytes.Equal(b1, b2) || c1 != c2 {
			e.fail("AckWebhook(%s) hai lần ra (%d, %q, %q) và (%d, %q, %q); cần giống hệt (hàm thuần); core chỉ hỏi một lần lúc nạp nên đối tác nhận phản hồi khác ý connector và có thể gửi lại hay ngừng gửi sai lúc", o, s1, b1, c1, s2, b2, c2)
		}
	}
}
