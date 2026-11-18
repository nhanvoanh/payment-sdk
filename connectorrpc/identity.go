package connectorrpc

import (
	"context"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"bitbucket.org/hasaki-tech/payment-connector-sdk/connectorpb"
)

var connectorServicePrefix = "/" + connectorpb.ConnectorService_ServiceDesc.ServiceName + "/"

// IdentityInterceptor từ chối mọi RPC của ConnectorService mà metadata MetadataConnectorName có giá trị khác name:
// handler KHÔNG được gọi, trả codes.FailedPrecondition kèm trailer MetadataIdentity=IdentityMismatch để proxy biết
// chắc lời gọi chưa được xử lý. Không có metadata thì cho qua (client cũ). Dịch vụ khác (health) không bị kiểm.
func IdentityInterceptor(name string) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
		if !strings.HasPrefix(info.FullMethod, connectorServicePrefix) {
			return h(ctx, req)
		}
		md, _ := metadata.FromIncomingContext(ctx)
		for _, want := range md.Get(MetadataConnectorName) {
			if want != name {
				_ = grpc.SetTrailer(ctx, metadata.Pairs(MetadataIdentity, IdentityMismatch))
				return nil, status.Errorf(codes.FailedPrecondition, "đây là connector %q, core gọi %q", name, want)
			}
		}
		return h(ctx, req)
	}
}
