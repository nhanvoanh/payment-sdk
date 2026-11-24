package connectorrpc

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/nhanvoanh/payment-sdk/connector"
	"github.com/nhanvoanh/payment-sdk/connectorpb"
	"github.com/nhanvoanh/payment-sdk/paykit"
)

const (
	defaultCallTimeout = 30 * time.Second
	defaultReadyWait   = time.Second
	// maxWebhookBody: thân lớn hơn không gửi, vì connector ở xa chỉ nhận được thông điệp gRPC có giới hạn.
	maxWebhookBody = 1 << 20
)

// ClientOptions của Client; giá trị 0 là mặc định.
type ClientOptions struct {
	// CallTimeout chỉ áp khi ctx chưa có hạn. Mặc định 30s.
	CallTimeout time.Duration
	// ReadyWait là thời gian tối đa chờ kết nối READY trước khi gửi. Mặc định 1s.
	ReadyWait time.Duration
	// OnQuarantine (tuỳ chọn) được gọi khi proxy tự cách ly vì một lời gọi nghiệp vụ bị server từ chối danh
	// tính; reason nêu RPC và lỗi. Describe sai danh tính không gọi hook (lỗi đã trả cho người gọi Describe).
	// Gọi đồng bộ trên goroutine của lời gọi: không được chặn.
	OnQuarantine func(reason string)
}

// ErrIdentityMismatch: tiến trình ở địa chỉ này không phải connector đã cấu hình — Describe trả tên hoặc phiên
// bản giao thức khác, hoặc server từ chối lời gọi vì tên trong metadata không khớp. Client bị cách ly cho tới
// lần Describe thành công sau.
var ErrIdentityMismatch = errors.New("connectorrpc: connector ở địa chỉ này không phải connector đã cấu hình")

const (
	// MetadataConnectorName là khoá metadata proxy gửi kèm MỌI RPC: tên connector mà core mong gặp.
	MetadataConnectorName = "x-connector-name"
	// MetadataIdentity là khoá trailer server đặt khi từ chối vì sai tên; giá trị IdentityMismatch.
	MetadataIdentity = "x-connector-identity"
	IdentityMismatch = "mismatch"
)

// Description là những gì connector ở xa khai ở Describe.
type Description struct {
	Name       string
	Methods    []paykit.Method
	Operations map[connector.Operation]bool
}

type described struct {
	Description
	acks map[connector.WebhookOutcome]ackResp
}

type ackResp struct {
	status int
	body   []byte
	ct     string
}

// Client là proxy phía core: cài connector.Connector, mọi interface tuỳ chọn và CapabilityReporter
// trên đường dây gRPC.
//
// Bất biến tiền: ErrProviderUnavailable chỉ trả khi kết nối chưa READY TRƯỚC khi gọi RPC (chắc chắn chưa
// gửi) hoặc khi connector nói rõ trong thân phản hồi. Đã gọi RPC mà lỗi (kể cả codes.Unavailable) là
// ErrUnknownOutcome. Không dùng WaitForReady và không bật retry của gRPC.
//
// Ngoại lệ duy nhất của "mã gRPC khác OK là chưa rõ": codes.FailedPrecondition KÈM trailer
// MetadataIdentity=IdentityMismatch. Server của SDK chỉ trả cặp đó khi tên trong metadata khác tên của nó, và
// trả TRƯỚC khi gọi handler, nên lời gọi chắc chắn chưa được xử lý: thao tác tiền/truy vấn nhận
// ErrProviderUnavailable, Parse* nhận ErrUnknownOutcome. FailedPrecondition không có dấu đó vẫn là chưa rõ.
//
// Cách ly: Describe trả sai tên/phiên bản, hoặc server từ chối vì sai tên, thì Client bị cách ly. Mô tả hợp lệ
// cuối cùng được GIỮ (Supports/Methods/AckWebhook không đổi, để use case không hiểu nhầm "không có năng lực");
// mọi RPC nghiệp vụ trả lỗi chưa gửi (tiền/truy vấn: ErrProviderUnavailable; Parse*: ErrUnknownOutcome).
// Lời gọi đã gửi trước khi cách ly mà phản hồi về sau đó thì phản hồi bị bỏ, trả ErrUnknownOutcome. Lần
// Describe đúng sau đó gỡ cách ly. Lỗi vận chuyển ở Describe không cách ly và giữ mô tả cũ.
//
// Describe được tuần tự hoá bằng khoá riêng; các RPC khác gọi đồng thời được.
type Client struct {
	name        string
	conn        *grpc.ClientConn
	rpc         connectorpb.ConnectorServiceClient
	opts        ClientOptions
	desc        atomic.Pointer[described]
	quarantined atomic.Bool
	gen         atomic.Uint64 // tăng mỗi lần cách ly; phản hồi của lời gọi chụp thế hệ cũ không được tin
	describeMu  sync.Mutex
	afterReady  func() // chỉ cho test: chạy sau waitReady, trước lần đọc lại cờ cách ly
}

// NewClient tạo proxy cho connector tên name. Chưa mô tả: gọi Describe trước khi dùng.
func NewClient(name string, conn *grpc.ClientConn, opts ClientOptions) *Client {
	if opts.CallTimeout <= 0 {
		opts.CallTimeout = defaultCallTimeout
	}
	if opts.ReadyWait <= 0 {
		opts.ReadyWait = defaultReadyWait
	}
	return &Client{name: name, conn: conn, rpc: connectorpb.NewConnectorServiceClient(conn), opts: opts}
}

var (
	_ connector.Connector          = (*Client)(nil)
	_ connector.CapabilityReporter = (*Client)(nil)
	_ connector.PaymentQuerier     = (*Client)(nil)
	_ connector.WebhookReceiver    = (*Client)(nil)
	_ connector.Redirecter         = (*Client)(nil)
	_ connector.Charger            = (*Client)(nil)
	_ connector.Canceler           = (*Client)(nil)
	_ connector.Refunder           = (*Client)(nil)
	_ connector.RefundQuerier      = (*Client)(nil)
)

// waitReady: true khi kết nối READY, chờ tối đa ReadyWait và không quá hạn của ctx.
func (c *Client) waitReady(ctx context.Context) bool {
	if c.conn.GetState() == connectivity.Ready {
		return true
	}
	c.conn.Connect()
	wctx, cancel := context.WithTimeout(ctx, c.opts.ReadyWait)
	defer cancel()
	for {
		s := c.conn.GetState()
		switch s {
		case connectivity.Ready:
			return true
		case connectivity.Shutdown:
			return false
		}
		if !c.conn.WaitForStateChange(wctx, s) {
			return false
		}
	}
}

const describeRPC = "Describe"

// invoke gọi một RPC. notSent là lỗi trả khi chắc chắn chưa gửi (kết nối không READY, đang cách ly, hoặc server
// từ chối vì sai tên trước handler); mọi lỗi khác sau khi đã gọi là ErrUnknownOutcome, giữ nguyên văn lỗi gRPC.
func (c *Client) invoke(ctx context.Context, method string, notSent error, send func(ctx context.Context, opts ...grpc.CallOption) error) error {
	business := method != describeRPC
	if business && c.quarantined.Load() {
		return c.refuseQuarantined(method, notSent)
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.opts.CallTimeout)
		defer cancel()
	}
	if !c.waitReady(ctx) {
		return fmt.Errorf("%w: %s: kết nối chưa sẵn sàng, chưa gửi", notSent, method)
	}
	if c.afterReady != nil {
		c.afterReady()
	}
	// Chụp thế hệ TRƯỚC khi đọc lại cờ: cách ly xảy ra sau lần đọc này sẽ làm thế hệ đổi và phản hồi bị bỏ.
	gen := c.gen.Load()
	if business && c.quarantined.Load() {
		return c.refuseQuarantined(method, notSent)
	}
	ctx = metadata.AppendToOutgoingContext(ctx, MetadataConnectorName, c.name)
	var trailer metadata.MD
	if err := send(ctx, grpc.Trailer(&trailer)); err != nil {
		if identityRejected(err, trailer) {
			c.quarantine()
			if business && c.opts.OnQuarantine != nil {
				c.opts.OnQuarantine(fmt.Sprintf("%s bị server từ chối vì sai tên connector: %v", method, err))
			}
			return fmt.Errorf("%w: %s: %w: server từ chối trước khi xử lý: %v", notSent, method, ErrIdentityMismatch, err)
		}
		return fmt.Errorf("%w: %s: %v", connector.ErrUnknownOutcome, method, err)
	}
	if business && (c.gen.Load() != gen || c.quarantined.Load()) {
		return fmt.Errorf("%w: %s: connector bị cách ly trong lúc lời gọi đang bay; bỏ phản hồi của tiến trình không đáng tin",
			connector.ErrUnknownOutcome, method)
	}
	return nil
}

func (c *Client) refuseQuarantined(method string, notSent error) error {
	return fmt.Errorf("%w: %s: %w: connector đang bị cách ly, chưa gửi", notSent, method, ErrIdentityMismatch)
}

// identityRejected: đúng cặp FailedPrecondition + trailer đánh dấu của server SDK; thiếu một trong hai là chưa rõ.
func identityRejected(err error, trailer metadata.MD) bool {
	if status.Code(err) != codes.FailedPrecondition {
		return false
	}
	for _, v := range trailer.Get(MetadataIdentity) {
		if v == IdentityMismatch {
			return true
		}
	}
	return false
}

// Describe gọi RPC, kiểm phiên bản và tên rồi lưu nguyên tử. Lỗi thì không lưu gì; sai danh tính thì cách ly.
// Cách ly xảy ra trong lúc Describe đang bay (thế hệ đổi) thì không lưu và không gỡ cách ly: câu trả lời
// đúng tên có thể tới từ tiến trình khác với tiến trình vừa từ chối một lời gọi nghiệp vụ.
func (c *Client) Describe(ctx context.Context) (Description, error) {
	c.describeMu.Lock()
	defer c.describeMu.Unlock()
	gen := c.gen.Load()
	var resp *connectorpb.DescribeResponse
	err := c.invoke(ctx, describeRPC, connector.ErrProviderUnavailable, func(ctx context.Context, opts ...grpc.CallOption) (e error) {
		resp, e = c.rpc.Describe(ctx, &connectorpb.DescribeRequest{}, opts...)
		return e
	})
	if err != nil {
		return Description{}, err
	}
	if resp.GetProtocolVersion() != ProtocolVersion {
		c.quarantine()
		return Description{}, fmt.Errorf("%w: phiên bản giao thức %d, cần %d", ErrIdentityMismatch, resp.GetProtocolVersion(), ProtocolVersion)
	}
	if resp.GetName() != c.name {
		c.quarantine()
		return Description{}, fmt.Errorf("%w: connector tên %q, cấu hình là %q", ErrIdentityMismatch, resp.GetName(), c.name)
	}
	d := &described{Description: Description{Name: resp.GetName(), Operations: map[connector.Operation]bool{}}}
	for _, m := range resp.GetMethods() {
		d.Methods = append(d.Methods, paykit.Method(m))
	}
	for _, op := range connector.AllOperations() {
		for _, s := range resp.GetOperations() {
			if s == string(op) {
				d.Operations[op] = true
			}
		}
	}
	if d.Operations[connector.OpWebhook] {
		d.acks = map[connector.WebhookOutcome]ackResp{}
		for _, o := range WebhookOutcomes {
			a := resp.GetWebhookAcks()[string(o)]
			if a == nil {
				return Description{}, fmt.Errorf("connectorrpc: connector nhận webhook nhưng thiếu webhook_acks[%s]", o)
			}
			if a.GetStatus() < 100 || a.GetStatus() > 599 {
				return Description{}, fmt.Errorf("connectorrpc: webhook_acks[%s] có status HTTP %d không hợp lệ", o, a.GetStatus())
			}
			d.acks[o] = ackResp{status: int(a.GetStatus()), body: a.GetBody(), ct: a.GetContentType()}
		}
	}
	if c.gen.Load() != gen {
		return Description{}, fmt.Errorf("%w: connector bị cách ly trong lúc Describe đang bay; giữ cách ly tới lần Describe sau", ErrIdentityMismatch)
	}
	c.desc.Store(d)
	c.quarantined.Store(false)
	// quarantine tăng thế hệ TRƯỚC khi đặt cờ: thế hệ đổi giữa lần kiểm trên và lần xoá cờ thì đặt lại cờ.
	if c.gen.Load() != gen {
		c.quarantined.Store(true)
		return Description{}, fmt.Errorf("%w: connector bị cách ly trong lúc Describe đang bay; giữ cách ly tới lần Describe sau", ErrIdentityMismatch)
	}
	return d.copyOut(), nil
}

// quarantine tăng thế hệ rồi đặt cờ; mô tả hợp lệ cuối cùng được giữ.
func (c *Client) quarantine() {
	c.gen.Add(1)
	c.quarantined.Store(true)
}

// Quarantined: tiến trình ở địa chỉ này đã tự nhận là connector khác; mọi RPC nghiệp vụ bị chặn.
func (c *Client) Quarantined() bool { return c.quarantined.Load() }

func (d *described) copyOut() Description {
	out := Description{Name: d.Name, Methods: append([]paykit.Method(nil), d.Methods...), Operations: map[connector.Operation]bool{}}
	for k, v := range d.Operations {
		out.Operations[k] = v
	}
	return out
}

// Described cho biết đã từng có mô tả hợp lệ (vẫn đúng khi đang cách ly).
func (c *Client) Described() bool { return c.desc.Load() != nil }

func (c *Client) Name() string { return c.name }

func (c *Client) Methods() []paykit.Method {
	d := c.desc.Load()
	if d == nil {
		return nil
	}
	return append([]paykit.Method(nil), d.Methods...)
}

// Supports đọc mô tả đã lưu; chưa mô tả thì mọi thao tác là false.
func (c *Client) Supports(op connector.Operation) bool {
	d := c.desc.Load()
	return d != nil && d.Operations[op]
}

// AckWebhook đọc bảng lấy ở Describe, không gọi RPC; outcome không có trong bảng thì (500, nil, "").
func (c *Client) AckWebhook(outcome connector.WebhookOutcome) (int, []byte, string) {
	d := c.desc.Load()
	if d == nil {
		return 500, nil, ""
	}
	a, ok := d.acks[outcome]
	if !ok {
		return 500, nil, ""
	}
	return a.status, a.body, a.ct
}

func (c *Client) CreatePayment(ctx context.Context, req connector.CreatePaymentRequest) (connector.CreatePaymentResult, error) {
	var resp *connectorpb.CreatePaymentResponse
	if err := c.invoke(ctx, "CreatePayment", connector.ErrProviderUnavailable, func(ctx context.Context, opts ...grpc.CallOption) (e error) {
		resp, e = c.rpc.CreatePayment(ctx, CreatePaymentRequestToProto(req), opts...)
		return e
	}); err != nil {
		return connector.CreatePaymentResult{}, err
	}
	return CreatePaymentResultFromProto(resp)
}

func (c *Client) Charge(ctx context.Context, req connector.ChargeRequest) (connector.ChargeResult, error) {
	var resp *connectorpb.ChargeResponse
	if err := c.invoke(ctx, "Charge", connector.ErrProviderUnavailable, func(ctx context.Context, opts ...grpc.CallOption) (e error) {
		resp, e = c.rpc.Charge(ctx, ChargeRequestToProto(req), opts...)
		return e
	}); err != nil {
		return connector.ChargeResult{}, err
	}
	return ChargeResultFromProto(resp)
}

func (c *Client) QueryPayment(ctx context.Context, req connector.QueryPaymentRequest) (connector.PaymentStatus, error) {
	var resp *connectorpb.QueryPaymentResponse
	if err := c.invoke(ctx, "QueryPayment", connector.ErrProviderUnavailable, func(ctx context.Context, opts ...grpc.CallOption) (e error) {
		resp, e = c.rpc.QueryPayment(ctx, QueryPaymentRequestToProto(req), opts...)
		return e
	}); err != nil {
		return connector.PaymentStatus{}, err
	}
	return PaymentStatusFromProto(resp)
}

func (c *Client) CancelPayment(ctx context.Context, req connector.CancelRequest) (connector.CancelResult, error) {
	var resp *connectorpb.CancelPaymentResponse
	if err := c.invoke(ctx, "CancelPayment", connector.ErrProviderUnavailable, func(ctx context.Context, opts ...grpc.CallOption) (e error) {
		resp, e = c.rpc.CancelPayment(ctx, CancelRequestToProto(req), opts...)
		return e
	}); err != nil {
		return connector.CancelResult{}, err
	}
	return CancelResultFromProto(resp)
}

func (c *Client) CreateRefund(ctx context.Context, req connector.RefundRequest) (connector.RefundResult, error) {
	var resp *connectorpb.CreateRefundResponse
	if err := c.invoke(ctx, "CreateRefund", connector.ErrProviderUnavailable, func(ctx context.Context, opts ...grpc.CallOption) (e error) {
		resp, e = c.rpc.CreateRefund(ctx, RefundRequestToProto(req), opts...)
		return e
	}); err != nil {
		return connector.RefundResult{}, err
	}
	return RefundResultFromProto(resp)
}

func (c *Client) QueryRefund(ctx context.Context, req connector.QueryRefundRequest) (connector.RefundStatus, error) {
	var resp *connectorpb.QueryRefundResponse
	if err := c.invoke(ctx, "QueryRefund", connector.ErrProviderUnavailable, func(ctx context.Context, opts ...grpc.CallOption) (e error) {
		resp, e = c.rpc.QueryRefund(ctx, QueryRefundRequestToProto(req), opts...)
		return e
	}); err != nil {
		return connector.RefundStatus{}, err
	}
	return RefundStatusFromProto(resp)
}

// errWebhookTooLarge: không gửi thân quá lớn, và nó không phải chữ ký hợp lệ.
func errWebhookTooLarge(n int) error {
	return fmt.Errorf("%w: thân %d byte vượt giới hạn %d", connector.ErrInvalidSignature, n, maxWebhookBody)
}

// ParseWebhook không phải lời gọi tiền: lỗi vận chuyển (kể cả chưa gửi được) là ErrUnknownOutcome,
// để core trả lỗi cho đối tác và đối tác gửi lại.
func (c *Client) ParseWebhook(ctx context.Context, raw connector.RawWebhook) (connector.ProviderEvent, error) {
	if len(raw.Body) > maxWebhookBody {
		return connector.ProviderEvent{}, errWebhookTooLarge(len(raw.Body))
	}
	var resp *connectorpb.ParseWebhookResponse
	if err := c.invoke(ctx, "ParseWebhook", connector.ErrUnknownOutcome, func(ctx context.Context, opts ...grpc.CallOption) (e error) {
		resp, e = c.rpc.ParseWebhook(ctx, &connectorpb.ParseWebhookRequest{Raw: RawWebhookToProto(raw)}, opts...)
		return e
	}); err != nil {
		return connector.ProviderEvent{}, err
	}
	return ParseWebhookResultFromProto(resp)
}

// ParseRedirect: cùng quy tắc với ParseWebhook.
func (c *Client) ParseRedirect(ctx context.Context, raw connector.RawWebhook) (connector.ProviderEvent, error) {
	if len(raw.Body) > maxWebhookBody {
		return connector.ProviderEvent{}, errWebhookTooLarge(len(raw.Body))
	}
	var resp *connectorpb.ParseRedirectResponse
	if err := c.invoke(ctx, "ParseRedirect", connector.ErrUnknownOutcome, func(ctx context.Context, opts ...grpc.CallOption) (e error) {
		resp, e = c.rpc.ParseRedirect(ctx, &connectorpb.ParseRedirectRequest{Raw: RawWebhookToProto(raw)}, opts...)
		return e
	}); err != nil {
		return connector.ProviderEvent{}, err
	}
	return ParseRedirectResultFromProto(resp)
}
