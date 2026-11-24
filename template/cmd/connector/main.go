// Command connector là tiến trình connector acme: đọc cấu hình từ biến môi trường rồi phục vụ gRPC
// qua mTLS cho core (CONNECTOR_* xem connectorsdk.OptionsFromEnv, ACME_* xem acme.ConfigFromEnv).
package main

import (
	"log/slog"
	"os"

	"github.com/nhanvoanh/payment-sdk/connectorsdk"
	"github.com/nhanvoanh/payment-sdk/template/acme"
)

func main() {
	cfg, err := acme.ConfigFromEnv(os.Getenv)
	if err != nil {
		slog.Error("cấu hình acme sai", "error", err)
		os.Exit(1)
	}
	c, err := acme.New(cfg)
	if err != nil {
		slog.Error("không dựng được connector acme", "error", err)
		os.Exit(1)
	}
	connectorsdk.Main(c)
}
