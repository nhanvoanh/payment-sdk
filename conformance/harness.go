// Package conformance là bộ luật mọi connector phải qua trước khi được nối vào core. Mỗi luật chạy hai
// lần: gọi thẳng connector, và gọi qua gRPC (mã luật có hậu tố /grpc) như core thật sẽ gọi.
package conformance

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"bitbucket.org/hasaki-tech/payment-connector-sdk/connector"
	"bitbucket.org/hasaki-tech/payment-connector-sdk/connectorrpc"
	"bitbucket.org/hasaki-tech/payment-connector-sdk/connectorrpc/grpctest"
	"bitbucket.org/hasaki-tech/payment-connector-sdk/paykit"
)

// Mode là cách đối tác giả cư xử ở lần gọi kế tiếp.
type Mode int

const (
	ModeNormal  Mode = iota // trả lời bình thường
	ModeDown                // từ chối kết nối: chắc chắn chưa nhận lời gọi
	ModeHang                // nhận lời gọi rồi im, không bao giờ trả lời
	ModeReject              // trả lời rõ ràng là từ chối
	ModeGarbage             // trả 200 với thân không hiểu được
)

// Partner là đối tác giả mà connector trong Harness.New nói chuyện cùng.
type Partner interface {
	SetMode(m Mode)
	// SignedWebhook dựng một webhook hợp lệ đã ký báo trạng thái cho giao dịch.
	SignedWebhook(transactionCode, providerRef string, status connector.Status, paid paykit.Money) connector.RawWebhook
	// SetPaymentStatus quyết định câu trả lời cho lần truy vấn kế tiếp.
	SetPaymentStatus(transactionCode string, status connector.Status, paid paykit.Money)
}

// Tamperer là phần tuỳ chọn của Partner: trả bản sao của một webhook đã ký với nội dung được ký bị sửa
// (chữ ký giữ nguyên). Partner cài nó thì WHK-4 đòi đúng ErrInvalidSignature; không cài thì WHK-4 tự lật
// một byte giữa thân và chỉ đòi connector không chấp nhận.
type Tamperer interface {
	TamperedWebhook(raw connector.RawWebhook) connector.RawWebhook
}

// RequestCounter là phần tuỳ chọn của Partner: số request nghiệp vụ (không tính lấy token) đối tác giả đã NHẬN
// từ lúc tạo. Đếm ngay khi NHẬN request, trước khi treo hay trả lời, và trên MỌI đường nghiệp vụ (tạo, truy vấn,
// huỷ, hoàn...); ModeDown không nhận nên không đếm. Partner cài nó thì lượt huỷ thiếu ProviderRef khi đối tác
// treo của CXL-2 phân biệt được quyết định tại chỗ với lời gọi tới đối tác; không cài thì lượt đó bị bỏ.
type RequestCounter interface {
	BusinessRequests() int
}

// RequestRecorder là phần tuỳ chọn của Partner: mã đơn (mã giao dịch phía đối tác, như connector đặt trong
// request) của mọi lệnh tạo thanh toán đối tác giả đã NHẬN, theo thứ tự nhận. Partner cài nó thì ID-4 kiểm được
// connector dựng mã tất định từ TransactionCode + CreatedAt; không cài thì ID-4 bị bỏ.
type RequestRecorder interface {
	OrderCodes() []string
}

// Harness mô tả connector cần kiểm.
type Harness struct {
	New    func(tb testing.TB) (connector.Connector, Partner) // mỗi kiểm tra một cặp mới
	Method paykit.Method
	Amount paykit.Money
	// CreateCallsPartner sai nghĩa là tạo giao dịch chỉ ký tại chỗ: bỏ các kiểm tra Down/Hang/Reject/Garbage của Create.
	CreateCallsPartner bool
}

// Finding là một vi phạm: Rule là mã luật, Detail nêu điều thấy được, điều mong đợi và hậu quả.
type Finding struct{ Rule, Detail string }

// Report là kết quả Check. Skipped và NotChecked có dạng "MÃ: lý do".
type Report struct {
	Findings []Finding
	// Skipped: luật không chạy vì phía developer — connector không khai thao tác, CreateCallsPartner sai,
	// bước chuẩn bị (Create ở ModeNormal) hỏng, hay Describe qua gRPC hỏng. Developer nên đọc và có thể phải xử lý.
	Skipped []string
	// NotChecked: lượt mà bộ luật theo thiết kế không kiểm được (ví dụ proxy gRPC không chuyển lời gọi tới
	// connector). Không bao giờ là việc của developer; chỉ để biết phạm vi kiểm.
	NotChecked []string
}

const (
	callLimit   = 5 * time.Second        // lời gọi bình thường tới đối tác giả trong tiến trình
	hangCtx     = 300 * time.Millisecond // hạn của ctx khi đối tác treo
	hangSlack   = 2 * time.Second        // thời gian cho phép vượt hạn ctx
	cancelLimit = 2 * time.Second        // ctx đã huỷ thì phải trả về trong khoảng này
	ruleLimit   = 30 * time.Second       // lưới an toàn cho cả một luật
	settleWait  = 200 * time.Millisecond // chờ request đang bay tới đối tác giả trước khi đọc bộ đếm
)

// guardError: fn panic (panicked đúng) hoặc không xong trong hạn.
type guardError struct {
	panicked bool
	value    any
	limit    time.Duration
}

func (g *guardError) Error() string {
	if g.panicked {
		return fmt.Sprintf("panic: %v", g.value)
	}
	return fmt.Sprintf("không trả về sau %v", g.limit)
}

// guard chạy fn trong goroutine riêng có bắt panic; lỗi khi fn panic hoặc không xong trong limit.
// Hết hạn thì bỏ mặc goroutine (connector treo), không chờ.
func guard(limit time.Duration, fn func()) *guardError {
	done := make(chan *guardError, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- &guardError{panicked: true, value: r}
				return
			}
			done <- nil
		}()
		fn()
	}()
	t := time.NewTimer(limit)
	defer t.Stop()
	select {
	case g := <-done:
		return g
	case <-t.C:
		return &guardError{limit: limit}
	}
}

// env là bối cảnh của một lượt chạy một luật (thẳng hoặc qua gRPC).
type env struct {
	h    Harness
	c    connector.Connector // connector đang kiểm: chính nó hoặc proxy gRPC
	orig connector.Connector // connector của developer
	p    Partner
	ops  map[connector.Operation]bool // thao tác connector của developer khai (As… đúng)
	srv  *serverHooks                 // khác nil ở lượt /grpc
	rule string
	seq  int

	mu         sync.Mutex
	findings   []string
	skip       string
	notChecked string // lượt không kiểm được vì connector cư xử hợp lệ mà luật không quan sát được (vào Report.NotChecked)
}

func (e *env) fail(format string, args ...any) {
	e.mu.Lock()
	e.findings = append(e.findings, fmt.Sprintf(format, args...))
	e.mu.Unlock()
}

// notCheckedf: lượt không kết luận được dù connector không sai (ví dụ connector tự trả lại kết quả theo khoá
// idempotency nên đối tác không nhận đủ request để so).
func (e *env) notCheckedf(format string, args ...any) {
	e.mu.Lock()
	e.notChecked = fmt.Sprintf(format, args...)
	e.mu.Unlock()
}

func (e *env) skipf(format string, args ...any) {
	e.mu.Lock()
	e.skip = fmt.Sprintf(format, args...)
	e.mu.Unlock()
}

// call chạy một lời gọi connector; panic hay quá limit thành finding và trả false. Trả false thì người gọi KHÔNG
// được đọc biến fn ghi: hết hạn thì goroutine bị bỏ mặc vẫn có thể ghi chúng bất cứ lúc nào (race).
func (e *env) call(what string, limit time.Duration, fn func()) bool {
	g := guard(limit, fn)
	switch {
	case g == nil:
		return true
	case g.panicked:
		e.fail("%s panic: %v; cần trả lỗi thay vì panic; core coi lời gọi là không rõ kết quả và giao dịch của khách bị kẹt", what, g.value)
	default:
		e.fail("%s không trả về sau %v; cần tôn trọng hạn của ctx; lời gọi treo giữ chân core và giao dịch của khách bị kẹt", what, g.limit)
	}
	return false
}

// tx sinh mã giao dịch mới, chỉ chữ hoa và số để hợp với đa số đối tác.
func (e *env) tx() string {
	e.seq++
	code := make([]byte, 0, len(e.rule))
	for i := 0; i < len(e.rule); i++ {
		if b := e.rule[i]; b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' {
			code = append(code, b)
		}
	}
	return fmt.Sprintf("CF%s%02d", code, e.seq)
}

func normalCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), callLimit)
}

// serverHooks là interceptor phía server gRPC: ghi panic của connector để lượt /grpc cũng báo được, và
// khi luật đang chạy phần "đối tác treo" thì bọc ctx của handler bằng hạn ngắn hơn hạn của proxy.
type serverHooks struct {
	deadline atomic.Int64 // time.Duration; 0 là không bọc
	mu       sync.Mutex
	panics   []string
}

func (s *serverHooks) setHandlerDeadline(d time.Duration) { s.deadline.Store(int64(d)) }

func (s *serverHooks) intercept(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (resp any, err error) {
	if d := time.Duration(s.deadline.Load()); d > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}
	defer func() {
		if r := recover(); r != nil {
			s.mu.Lock()
			s.panics = append(s.panics, fmt.Sprintf("%s: %v", info.FullMethod, r))
			s.mu.Unlock()
			err = status.Error(codes.Internal, "connector panic")
		}
	}()
	return h(ctx, req)
}

func (s *serverHooks) drain() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.panics
	s.panics = nil
	return out
}

// loopback phục vụ c qua gRPC trên bufconn và trả proxy phía core đã Describe. Không dùng
// grpctest.Loopback vì nó không bắt panic phía server (panic sẽ làm sập cả tiến trình test) và gọi
// tb.Fatal khi Describe hỏng; ở đây Describe hỏng là lỗi trả về để thành finding của ID-1/grpc.
func loopback(tb testing.TB, c connector.Connector) (*connectorrpc.Client, *serverHooks, error) {
	pl := &serverHooks{}
	gh := grpctest.New(tb, connectorrpc.NewServer(c), grpc.ChainUnaryInterceptor(pl.intercept))
	var name string
	if err := guard(callLimit, func() { name = c.Name() }); err != nil {
		return nil, pl, fmt.Errorf("Name(): %w", err)
	}
	client := connectorrpc.NewClient(name, gh.Conn, connectorrpc.ClientOptions{CallTimeout: callLimit})
	var derr error
	if err := guard(callLimit+time.Second, func() {
		ctx, cancel := normalCtx()
		defer cancel()
		_, derr = client.Describe(ctx)
	}); err != nil {
		return nil, pl, fmt.Errorf("Describe: %w", err)
	}
	if derr != nil {
		return nil, pl, derr
	}
	return client, pl, nil
}

// declared trả các thao tác connector khai (connector.As… đúng); Supports panic thì coi là không khai.
func declared(c connector.Connector) map[connector.Operation]bool {
	ops := map[connector.Operation]bool{}
	for _, op := range connector.AllOperations() {
		var ok bool
		if guard(callLimit, func() { ok = asOp(c, op) }) == nil && ok {
			ops[op] = true
		}
	}
	return ops
}

func asOp(c connector.Connector, op connector.Operation) bool {
	var ok bool
	switch op {
	case connector.OpQueryPayment:
		_, ok = connector.AsPaymentQuerier(c)
	case connector.OpWebhook:
		_, ok = connector.AsWebhookReceiver(c)
	case connector.OpRedirect:
		_, ok = connector.AsRedirecter(c)
	case connector.OpCharge:
		_, ok = connector.AsCharger(c)
	case connector.OpCancel:
		_, ok = connector.AsCanceler(c)
	case connector.OpRefund:
		_, ok = connector.AsRefunder(c)
	case connector.OpQueryRefund:
		_, ok = connector.AsRefundQuerier(c)
	}
	return ok
}

// implements: c có cài interface của op hay không, không hỏi Supports.
func implements(c connector.Connector, op connector.Operation) bool {
	var ok bool
	switch op {
	case connector.OpQueryPayment:
		_, ok = c.(connector.PaymentQuerier)
	case connector.OpWebhook:
		_, ok = c.(connector.WebhookReceiver)
	case connector.OpRedirect:
		_, ok = c.(connector.Redirecter)
	case connector.OpCharge:
		_, ok = c.(connector.Charger)
	case connector.OpCancel:
		_, ok = c.(connector.Canceler)
	case connector.OpRefund:
		_, ok = c.(connector.Refunder)
	case connector.OpQueryRefund:
		_, ok = c.(connector.RefundQuerier)
	}
	return ok
}
