// Package grpctest dựng server connector trên bufconn để test client/server trong một tiến trình.
package grpctest

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/nhanvoanh/payment-sdk/connector"
	"github.com/nhanvoanh/payment-sdk/connectorpb"
	"github.com/nhanvoanh/payment-sdk/connectorrpc"
)

// Harness là một server gRPC đang chạy trên bufconn cùng một kết nối tới nó. Test có thể Stop server
// để mô phỏng chết giữa chừng; mọi thứ tự dọn khi test xong.
type Harness struct {
	Server *grpc.Server
	Conn   *grpc.ClientConn
}

// New phục vụ svc trên bufconn. Kênh không mã hoá chỉ dành cho test trong tiến trình.
func New(t testing.TB, svc connectorpb.ConnectorServiceServer, opts ...grpc.ServerOption) *Harness {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer(opts...)
	connectorpb.RegisterConnectorServiceServer(srv, svc)
	go func() { _ = srv.Serve(lis) }()

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpctest: %v", err)
	}
	t.Cleanup(func() {
		_ = conn.Close()
		srv.Stop()
	})
	return &Harness{Server: srv, Conn: conn}
}

// Client tạo Client tên name trên h và Describe; test thất bại nếu Describe lỗi.
func Client(t testing.TB, h *Harness, name string) *connectorrpc.Client {
	t.Helper()
	c := connectorrpc.NewClient(name, h.Conn, connectorrpc.ClientOptions{})
	if _, err := c.Describe(context.Background()); err != nil {
		t.Fatalf("grpctest: Describe: %v", err)
	}
	return c
}

// Loopback chạy c sau NewServer trên bufconn và trả Client đã Describe, tự dọn khi test xong.
func Loopback(t testing.TB, c connector.Connector) *connectorrpc.Client {
	t.Helper()
	return Client(t, New(t, connectorrpc.NewServer(c)), c.Name())
}

// Kill dừng cứng server rồi chờ kết nối phía client rời READY, để test "chưa gửi" không phụ thuộc
// vào việc client đã kịp nhận ra kết nối đứt hay chưa.
func (h *Harness) Kill(t testing.TB) {
	t.Helper()
	h.Server.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for h.Conn.GetState() == connectivity.Ready {
		if !h.Conn.WaitForStateChange(ctx, connectivity.Ready) {
			t.Fatal("grpctest: kết nối vẫn READY sau khi dừng server")
		}
	}
}
