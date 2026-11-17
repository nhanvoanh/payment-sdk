package connectorsdk

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"bitbucket.org/hasaki-tech/payment-connector-sdk/connector"
	"bitbucket.org/hasaki-tech/payment-connector-sdk/connectorrpc"
)

// OptionsFromEnv đọc cấu hình từ biến môi trường:
// CONNECTOR_LISTEN_ADDR (mặc định :9090), CONNECTOR_TLS_CERT/KEY/CA, CONNECTOR_ALLOWED_CLIENTS (phẩy),
// CONNECTOR_INSECURE (chỉ đúng chuỗi "true" mới bật). Thiếu chứng chỉ mà không bật Insecure là lỗi.
func OptionsFromEnv(getenv func(string) string) (Options, error) {
	o := Options{ListenAddr: getenv("CONNECTOR_LISTEN_ADDR")}
	if o.ListenAddr == "" {
		o.ListenAddr = defaultListenAddr
	}
	for _, n := range strings.Split(getenv("CONNECTOR_ALLOWED_CLIENTS"), ",") {
		if n = strings.TrimSpace(n); n != "" {
			o.AllowedClients = append(o.AllowedClients, n)
		}
	}
	f := connectorrpc.TLSFiles{
		CertFile: getenv("CONNECTOR_TLS_CERT"), KeyFile: getenv("CONNECTOR_TLS_KEY"), CAFile: getenv("CONNECTOR_TLS_CA"),
	}
	anyTLS := f.CertFile != "" || f.KeyFile != "" || f.CAFile != ""

	if getenv("CONNECTOR_INSECURE") == "true" {
		if anyTLS {
			return Options{}, errors.New("connectorsdk: CONNECTOR_INSECURE=true không đi cùng CONNECTOR_TLS_*")
		}
		o.Insecure = true
		return o, nil
	}
	if f.CertFile == "" || f.KeyFile == "" || f.CAFile == "" {
		return Options{}, errors.New("connectorsdk: cần CONNECTOR_TLS_CERT, CONNECTOR_TLS_KEY và CONNECTOR_TLS_CA (hoặc CONNECTOR_INSECURE=true khi phát triển)")
	}
	if len(o.AllowedClients) == 0 {
		return Options{}, errors.New("connectorsdk: cần CONNECTOR_ALLOWED_CLIENTS khi dùng TLS")
	}
	cfg, err := connectorrpc.ServerTLS(f)
	if err != nil {
		return Options{}, err
	}
	o.TLS = cfg
	return o, nil
}

// Main là điểm vào của tiến trình connector: đọc cấu hình từ môi trường, phục vụ tới khi nhận SIGINT/SIGTERM.
// Lỗi thì ghi log và thoát với mã 1.
func Main(c connector.Connector) {
	o, err := OptionsFromEnv(os.Getenv)
	if err == nil {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		err = Serve(ctx, c, o)
	}
	if err != nil {
		slog.Error("connector dừng vì lỗi", "error", err)
		os.Exit(1)
	}
}
