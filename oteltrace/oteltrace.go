// Package oteltrace dựng OpenTelemetry tracing cho các dịch vụ dùng SDK. Mặc định TẮT: chỉ khi
// OTEL_TRACES_EXPORTER là otlp hoặc console thì mới đụng tới provider toàn cục.
package oteltrace

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
	"go.opentelemetry.io/otel/trace"
)

// TracerName là tiền tố instrumentation scope của các span do SDK tạo.
const TracerName = "github.com/nhanvoanh/payment-sdk"

// errorLogInterval là khoảng tối thiểu giữa hai dòng log lỗi exporter.
const errorLogInterval = time.Minute

// Config là cấu hình Setup.
type Config struct {
	ServiceName    string       // bắt buộc, vd "payment-core", "payment-connector-zalopay"
	ServiceVersion string       // "" → "unknown"
	Environment    string       // deployment.environment.name; "" → bỏ thuộc tính
	Logger         *slog.Logger // nil → slog.Default(); nhận lỗi exporter
	Console        io.Writer    // nil → os.Stderr; đích của console exporter
}

// Setup đọc OTEL_TRACES_EXPORTER (""/none → tắt, không đụng global; otlp; console) và dựng TracerProvider, propagator
// W3C TraceContext, error handler có giới hạn tần suất. Lỗi exporter không bao giờ làm hỏng lời gọi hay chặn khởi động.
func Setup(ctx context.Context, cfg Config) (shutdown func(context.Context) error, err error) {
	kind := strings.ToLower(strings.TrimSpace(os.Getenv("OTEL_TRACES_EXPORTER")))
	if kind == "" || kind == "none" {
		return func(context.Context) error { return nil }, nil
	}
	if kind != "otlp" && kind != "console" {
		return nil, fmt.Errorf("oteltrace: OTEL_TRACES_EXPORTER=%q is not supported (want none, otlp or console)", kind)
	}
	if strings.TrimSpace(cfg.ServiceName) == "" {
		return nil, errors.New("oteltrace: Config.ServiceName is required")
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	// Dựng resource TRƯỚC exporter để lỗi resource không bao giờ làm rò exporter.
	res, err := newResource(ctx, cfg)
	if err != nil {
		if res == nil || !(errors.Is(err, resource.ErrPartialResource) || errors.Is(err, resource.ErrSchemaURLConflict)) {
			return nil, fmt.Errorf("oteltrace: build resource: %w", err)
		}
		logger.Warn("oteltrace: resource built with problems, continuing with the partial resource", "error", err)
	}

	var exp sdktrace.SpanExporter
	switch kind {
	case "console":
		w := cfg.Console
		if w == nil {
			w = os.Stderr
		}
		exp, err = stdouttrace.New(stdouttrace.WithWriter(w))
	default:
		exp, err = newOTLP(ctx)
	}
	if err != nil {
		return nil, fmt.Errorf("oteltrace: create %s exporter: %w", kind, err)
	}

	// Error handler phải có trước NewTracerProvider: OTEL_TRACES_SAMPLER không hợp lệ được báo qua handler.
	otel.SetErrorHandler(newErrorHandler(logger))
	// Không đặt sampler: SDK tự đọc OTEL_TRACES_SAMPLER / OTEL_TRACES_SAMPLER_ARG.
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp), sdktrace.WithResource(res))
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	return tp.Shutdown, nil
}

// newOTLP chọn giao thức theo OTEL_EXPORTER_OTLP_TRACES_PROTOCOL rồi OTEL_EXPORTER_OTLP_PROTOCOL. Cả hai exporter đều
// không dial đồng bộ nên collector chết không chặn khởi động.
func newOTLP(ctx context.Context) (sdktrace.SpanExporter, error) {
	name, proto := "OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", strings.ToLower(strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL")))
	if proto == "" {
		name, proto = "OTEL_EXPORTER_OTLP_PROTOCOL", strings.ToLower(strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_PROTOCOL")))
	}
	switch proto {
	case "", "grpc":
		return otlptracegrpc.New(ctx)
	case "http/protobuf":
		return otlptracehttp.New(ctx)
	}
	return nil, fmt.Errorf("%s=%q is not supported (want grpc or http/protobuf)", name, proto)
}

func newResource(ctx context.Context, cfg Config) (*resource.Resource, error) {
	version := cfg.ServiceVersion
	if version == "" {
		version = "unknown"
	}
	attrs := []attribute.KeyValue{semconv.ServiceName(cfg.ServiceName), semconv.ServiceVersion(version)}
	if cfg.Environment != "" {
		attrs = append(attrs, semconv.DeploymentEnvironmentName(cfg.Environment))
	}
	if host, err := os.Hostname(); err == nil && host != "" {
		attrs = append(attrs, semconv.ServiceInstanceID(host))
	}
	// Không WithSchemaURL: resource không schema gộp được với mọi detector (tránh ErrSchemaURLConflict khi sdk nâng cấp).
	// WithFromEnv sau WithAttributes: OTEL_SERVICE_NAME / OTEL_RESOURCE_ATTRIBUTES ghi đè Config.
	return resource.New(ctx, resource.WithAttributes(attrs...),
		resource.WithFromEnv(), resource.WithTelemetrySDK())
}

// newErrorHandler log WARN "otel export error" tối đa một lần mỗi phút (đồng hồ đơn điệu).
func newErrorHandler(l *slog.Logger) otel.ErrorHandler {
	var (
		mu   sync.Mutex
		last time.Time
		seen bool
	)
	return otel.ErrorHandlerFunc(func(err error) {
		mu.Lock()
		now := time.Now()
		if seen && now.Sub(last) < errorLogInterval {
			mu.Unlock()
			return
		}
		seen, last = true, now
		mu.Unlock()
		l.Warn("otel export error", "error", err)
	})
}

// HasParent báo ctx đã mang span context hợp lệ (kể cả remote, không sampled). SDK chỉ tạo span khi có parent.
func HasParent(ctx context.Context) bool { return trace.SpanContextFromContext(ctx).IsValid() }
