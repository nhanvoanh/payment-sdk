package connectorrpc

import (
	"context"

	"bitbucket.org/hasaki-tech/payment-connector-sdk/connector"
	"bitbucket.org/hasaki-tech/payment-connector-sdk/connectorpb"
)

type server struct {
	connectorpb.UnimplementedConnectorServiceServer
	c connector.Connector
}

// NewServer bọc một connector thành dịch vụ gRPC. Lỗi của connector đi vào thân phản hồi (RPC vẫn trả OK),
// Raw luôn được gửi kể cả khi lỗi. Panic của connector không bắt ở đây: interceptor của connectorsdk lo.
func NewServer(c connector.Connector) connectorpb.ConnectorServiceServer {
	return &server{c: c}
}

// notSupported là lỗi trong thân phản hồi cho thao tác connector không có; không dùng mã gRPC Unimplemented.
func notSupported() *connectorpb.ConnectorError { return ErrorToProto(connector.ErrNotSupported) }

func (s *server) Describe(context.Context, *connectorpb.DescribeRequest) (*connectorpb.DescribeResponse, error) {
	resp := &connectorpb.DescribeResponse{ProtocolVersion: ProtocolVersion, Name: s.c.Name()}
	for _, m := range s.c.Methods() {
		resp.Methods = append(resp.Methods, valid(string(m)))
	}
	for _, op := range connector.AllOperations() {
		if connector.Supports(s.c, op) {
			resp.Operations = append(resp.Operations, string(op))
		}
	}
	if wr, ok := connector.AsWebhookReceiver(s.c); ok {
		resp.WebhookAcks = map[string]*connectorpb.WebhookAck{}
		for _, o := range WebhookOutcomes {
			st, body, ct := wr.AckWebhook(o)
			resp.WebhookAcks[string(o)] = &connectorpb.WebhookAck{Status: int32(st), Body: body, ContentType: valid(ct)}
		}
	}
	return resp, nil
}

func (s *server) CreatePayment(ctx context.Context, req *connectorpb.CreatePaymentRequest) (*connectorpb.CreatePaymentResponse, error) {
	res, err := s.c.CreatePayment(ctx, CreatePaymentRequestFromProto(req))
	resp := CreatePaymentResultToProto(res)
	resp.Error = ErrorToProto(err)
	return resp, nil
}

func (s *server) Charge(ctx context.Context, req *connectorpb.ChargeRequest) (*connectorpb.ChargeResponse, error) {
	ch, ok := connector.AsCharger(s.c)
	if !ok {
		return &connectorpb.ChargeResponse{Error: notSupported()}, nil
	}
	res, err := ch.Charge(ctx, ChargeRequestFromProto(req))
	resp := ChargeResultToProto(res)
	resp.Error = ErrorToProto(err)
	return resp, nil
}

func (s *server) QueryPayment(ctx context.Context, req *connectorpb.QueryPaymentRequest) (*connectorpb.QueryPaymentResponse, error) {
	q, ok := connector.AsPaymentQuerier(s.c)
	if !ok {
		return &connectorpb.QueryPaymentResponse{Error: notSupported()}, nil
	}
	res, err := q.QueryPayment(ctx, QueryPaymentRequestFromProto(req))
	resp := PaymentStatusToProto(res)
	resp.Error = ErrorToProto(err)
	return resp, nil
}

func (s *server) CancelPayment(ctx context.Context, req *connectorpb.CancelPaymentRequest) (*connectorpb.CancelPaymentResponse, error) {
	cn, ok := connector.AsCanceler(s.c)
	if !ok {
		return &connectorpb.CancelPaymentResponse{Error: notSupported()}, nil
	}
	res, err := cn.CancelPayment(ctx, CancelRequestFromProto(req))
	resp := CancelResultToProto(res)
	resp.Error = ErrorToProto(err)
	return resp, nil
}

func (s *server) CreateRefund(ctx context.Context, req *connectorpb.CreateRefundRequest) (*connectorpb.CreateRefundResponse, error) {
	r, ok := connector.AsRefunder(s.c)
	if !ok {
		return &connectorpb.CreateRefundResponse{Error: notSupported()}, nil
	}
	res, err := r.CreateRefund(ctx, RefundRequestFromProto(req))
	resp := RefundResultToProto(res)
	resp.Error = ErrorToProto(err)
	return resp, nil
}

func (s *server) QueryRefund(ctx context.Context, req *connectorpb.QueryRefundRequest) (*connectorpb.QueryRefundResponse, error) {
	q, ok := connector.AsRefundQuerier(s.c)
	if !ok {
		return &connectorpb.QueryRefundResponse{Error: notSupported()}, nil
	}
	res, err := q.QueryRefund(ctx, QueryRefundRequestFromProto(req))
	resp := RefundStatusToProto(res)
	resp.Error = ErrorToProto(err)
	return resp, nil
}

func (s *server) ParseWebhook(ctx context.Context, req *connectorpb.ParseWebhookRequest) (*connectorpb.ParseWebhookResponse, error) {
	w, ok := connector.AsWebhookReceiver(s.c)
	if !ok {
		return &connectorpb.ParseWebhookResponse{Error: notSupported()}, nil
	}
	ev, err := w.ParseWebhook(ctx, RawWebhookFromProto(req.GetRaw()))
	if err != nil {
		return &connectorpb.ParseWebhookResponse{Error: ErrorToProto(err)}, nil
	}
	return &connectorpb.ParseWebhookResponse{Event: ProviderEventToProto(ev)}, nil
}

func (s *server) ParseRedirect(ctx context.Context, req *connectorpb.ParseRedirectRequest) (*connectorpb.ParseRedirectResponse, error) {
	r, ok := connector.AsRedirecter(s.c)
	if !ok {
		return &connectorpb.ParseRedirectResponse{Error: notSupported()}, nil
	}
	ev, err := r.ParseRedirect(ctx, RawWebhookFromProto(req.GetRaw()))
	if err != nil {
		return &connectorpb.ParseRedirectResponse{Error: ErrorToProto(err)}, nil
	}
	return &connectorpb.ParseRedirectResponse{Event: ProviderEventToProto(ev)}, nil
}
