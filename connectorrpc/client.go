package connectorrpc

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"

	"bitbucket.org/hasaki-tech/payment-connector-sdk/connector"
	"bitbucket.org/hasaki-tech/payment-connector-sdk/connectorpb"
	"bitbucket.org/hasaki-tech/payment-connector-sdk/paykit"
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
}

// ErrIdentityMismatch: Describe nhận được phản hồi nhưng tên hoặc phiên bản giao thức không khớp, tức địa chỉ
// đang trỏ tới một tiến trình khác. Client khi đó bị cách ly cho tới lần Describe thành công sau.
var ErrIdentityMismatch = errors.New("connectorrpc: connector ở địa chỉ này không phải connector đã cấu hình")

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
// Cách ly: Describe trả tên hoặc phiên bản giao thức sai thì mô tả bị xoá (Described, Supports, Methods đều
// rỗng) và mọi RPC nghiệp vụ trả ErrProviderUnavailable mà không gửi, vì tiến trình bên kia không phải
// connector đã cấu hình. Lỗi vận chuyển ở Describe không cách ly và giữ mô tả cũ.
type Client struct {
	name        string
	conn        *grpc.ClientConn
	rpc         connectorpb.ConnectorServiceClient
	opts        ClientOptions
	desc        atomic.Pointer[described]
	quarantined atomic.Bool
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

// invoke gọi một RPC. notSent là lỗi trả khi kết nối không READY (chắc chắn chưa gửi); mọi lỗi sau khi
// đã gọi là ErrUnknownOutcome, giữ nguyên văn lỗi gRPC.
func (c *Client) invoke(ctx context.Context, method string, notSent error, send func(ctx context.Context) error) error {
	if method != "Describe" && c.quarantined.Load() {
		return fmt.Errorf("%w: %s: connector đang bị cách ly vì Describe trả sai danh tính, chưa gửi",
			connector.ErrProviderUnavailable, method)
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.opts.CallTimeout)
		defer cancel()
	}
	if !c.waitReady(ctx) {
		return fmt.Errorf("%w: %s: kết nối chưa sẵn sàng, chưa gửi", notSent, method)
	}
	if err := send(ctx); err != nil {
		return fmt.Errorf("%w: %s: %v", connector.ErrUnknownOutcome, method, err)
	}
	return nil
}

// Describe gọi RPC, kiểm phiên bản và tên rồi lưu nguyên tử. Lỗi thì không lưu gì.
func (c *Client) Describe(ctx context.Context) (Description, error) {
	var resp *connectorpb.DescribeResponse
	err := c.invoke(ctx, "Describe", connector.ErrProviderUnavailable, func(ctx context.Context) (e error) {
		resp, e = c.rpc.Describe(ctx, &connectorpb.DescribeRequest{})
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
	for _, op := range allOperations {
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
	c.desc.Store(d)
	c.quarantined.Store(false)
	return d.copyOut(), nil
}

// quarantine chặn RPC nghiệp vụ trước rồi mới xoá mô tả, để không lời gọi nào thấy "chưa cách ly" sau khi mô tả đã mất.
func (c *Client) quarantine() {
	c.quarantined.Store(true)
	c.desc.Store(nil)
}

func (d *described) copyOut() Description {
	out := Description{Name: d.Name, Methods: append([]paykit.Method(nil), d.Methods...), Operations: map[connector.Operation]bool{}}
	for k, v := range d.Operations {
		out.Operations[k] = v
	}
	return out
}

// Described cho biết đã Describe thành công chưa.
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
	if err := c.invoke(ctx, "CreatePayment", connector.ErrProviderUnavailable, func(ctx context.Context) (e error) {
		resp, e = c.rpc.CreatePayment(ctx, CreatePaymentRequestToProto(req))
		return e
	}); err != nil {
		return connector.CreatePaymentResult{}, err
	}
	return CreatePaymentResultFromProto(resp)
}

func (c *Client) Charge(ctx context.Context, req connector.ChargeRequest) (connector.ChargeResult, error) {
	var resp *connectorpb.ChargeResponse
	if err := c.invoke(ctx, "Charge", connector.ErrProviderUnavailable, func(ctx context.Context) (e error) {
		resp, e = c.rpc.Charge(ctx, ChargeRequestToProto(req))
		return e
	}); err != nil {
		return connector.ChargeResult{}, err
	}
	return ChargeResultFromProto(resp)
}

func (c *Client) QueryPayment(ctx context.Context, req connector.QueryPaymentRequest) (connector.PaymentStatus, error) {
	var resp *connectorpb.QueryPaymentResponse
	if err := c.invoke(ctx, "QueryPayment", connector.ErrProviderUnavailable, func(ctx context.Context) (e error) {
		resp, e = c.rpc.QueryPayment(ctx, QueryPaymentRequestToProto(req))
		return e
	}); err != nil {
		return connector.PaymentStatus{}, err
	}
	return PaymentStatusFromProto(resp)
}

func (c *Client) CancelPayment(ctx context.Context, req connector.CancelRequest) (connector.CancelResult, error) {
	var resp *connectorpb.CancelPaymentResponse
	if err := c.invoke(ctx, "CancelPayment", connector.ErrProviderUnavailable, func(ctx context.Context) (e error) {
		resp, e = c.rpc.CancelPayment(ctx, CancelRequestToProto(req))
		return e
	}); err != nil {
		return connector.CancelResult{}, err
	}
	return CancelResultFromProto(resp)
}

func (c *Client) CreateRefund(ctx context.Context, req connector.RefundRequest) (connector.RefundResult, error) {
	var resp *connectorpb.CreateRefundResponse
	if err := c.invoke(ctx, "CreateRefund", connector.ErrProviderUnavailable, func(ctx context.Context) (e error) {
		resp, e = c.rpc.CreateRefund(ctx, RefundRequestToProto(req))
		return e
	}); err != nil {
		return connector.RefundResult{}, err
	}
	return RefundResultFromProto(resp)
}

func (c *Client) QueryRefund(ctx context.Context, req connector.QueryRefundRequest) (connector.RefundStatus, error) {
	var resp *connectorpb.QueryRefundResponse
	if err := c.invoke(ctx, "QueryRefund", connector.ErrProviderUnavailable, func(ctx context.Context) (e error) {
		resp, e = c.rpc.QueryRefund(ctx, QueryRefundRequestToProto(req))
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
	if err := c.invoke(ctx, "ParseWebhook", connector.ErrUnknownOutcome, func(ctx context.Context) (e error) {
		resp, e = c.rpc.ParseWebhook(ctx, &connectorpb.ParseWebhookRequest{Raw: RawWebhookToProto(raw)})
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
	if err := c.invoke(ctx, "ParseRedirect", connector.ErrUnknownOutcome, func(ctx context.Context) (e error) {
		resp, e = c.rpc.ParseRedirect(ctx, &connectorpb.ParseRedirectRequest{Raw: RawWebhookToProto(raw)})
		return e
	}); err != nil {
		return connector.ProviderEvent{}, err
	}
	return ParseRedirectResultFromProto(resp)
}
