// Command fakepartner chạy đối tác "acme" giả (acmetest) thành một tiến trình HTTP, để compose của người phát
// triển có đối tác để gọi mà không cần tài khoản thật. CHỈ cho máy phát triển: thông tin đăng nhập là hằng số
// công khai trong acmetest (merchant M-TEST, secret acme-test-secret), không có TLS.
//
// Đây là chỗ DUY NHẤT ngoài file _test.go và ngoài bộ-test được import acmetest, và chỉ acmetest: bộ-test khác
// vẫn bị cấm ở đây (testKitUsers trong arch_test.go ở gốc repo, depguard "fakepartner-kits"). Image của nó dựng bằng
// deploy/compose/Dockerfile.template của repo core; image connector của người dùng mẫu (template/Dockerfile) không chứa nó.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/nhanvoanh/payment-sdk/template/acme/acmetest"
)

func main() {
	addr := os.Getenv("FAKEPARTNER_ADDR")
	if addr == "" {
		addr = ":8081"
	}
	p, err := acmetest.Listen(addr)
	if err != nil {
		slog.Error("không mở được cổng của đối tác giả", "error", err)
		os.Exit(1)
	}
	slog.Info("đối tác giả acme đang chạy (chỉ cho phát triển)", "addr", addr)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	<-ctx.Done()
	p.Close()
}
