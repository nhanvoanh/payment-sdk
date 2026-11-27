package oteltrace_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/nhanvoanh/payment-sdk/oteltrace"
)

// saveGlobals khôi phục provider, propagator và error handler toàn cục sau test (Setup đổi chúng).
func saveGlobals(t *testing.T) {
	t.Helper()
	tp, prop := otel.GetTracerProvider(), otel.GetTextMapPropagator()
	t.Cleanup(func() {
		otel.SetTracerProvider(tp)
		otel.SetTextMapPropagator(prop)
		otel.SetErrorHandler(otel.ErrorHandlerFunc(func(error) {}))
	})
}

func TestSetup_OffByDefault(t *testing.T) {
	for _, v := range []string{"", "none", " NONE "} {
		t.Run("v="+v, func(t *testing.T) {
			saveGlobals(t)
			t.Setenv("OTEL_TRACES_EXPORTER", v)
			before := otel.GetTracerProvider()
			shutdown, err := oteltrace.Setup(context.Background(), oteltrace.Config{ServiceName: "payment-core"})
			require.NoError(t, err)
			assert.Same(t, before, otel.GetTracerProvider())
			assert.NoError(t, shutdown(context.Background()))
		})
	}
}

func TestSetup_Console(t *testing.T) {
	saveGlobals(t)
	t.Setenv("OTEL_TRACES_EXPORTER", "console")
	var buf bytes.Buffer
	shutdown, err := oteltrace.Setup(context.Background(), oteltrace.Config{ServiceName: "payment-core", Console: &buf})
	require.NoError(t, err)

	ctx, span := otel.Tracer("x").Start(context.Background(), "my-span-name")
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	assert.NotEmpty(t, carrier.Get("traceparent"))
	span.End()
	require.NoError(t, shutdown(context.Background()))

	assert.Contains(t, buf.String(), "my-span-name")
	assert.Contains(t, buf.String(), "payment-core")
}

func TestSetup_RejectsUnknownExporter(t *testing.T) {
	for _, v := range []string{"jaeger", "otlp,console", "zipkin"} {
		t.Run(v, func(t *testing.T) {
			saveGlobals(t)
			t.Setenv("OTEL_TRACES_EXPORTER", v)
			_, err := oteltrace.Setup(context.Background(), oteltrace.Config{ServiceName: "s"})
			require.Error(t, err)
			assert.Contains(t, err.Error(), "OTEL_TRACES_EXPORTER")
		})
	}
	t.Run("normalised", func(t *testing.T) {
		saveGlobals(t)
		t.Setenv("OTEL_TRACES_EXPORTER", " OTLP ")
		t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1")
		shutdown, err := oteltrace.Setup(context.Background(), oteltrace.Config{ServiceName: "s"})
		require.NoError(t, err)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = shutdown(ctx)
	})
}

func TestSetup_RejectsUnknownProtocol(t *testing.T) {
	saveGlobals(t)
	t.Setenv("OTEL_TRACES_EXPORTER", "otlp")
	t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "http/json")
	_, err := oteltrace.Setup(context.Background(), oteltrace.Config{ServiceName: "s"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "OTEL_EXPORTER_OTLP_PROTOCOL")

	// biến riêng cho traces thắng biến chung
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", "grpc")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1")
	shutdown, err := oteltrace.Setup(context.Background(), oteltrace.Config{ServiceName: "s"})
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = shutdown(ctx)

	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", "bogus")
	_, err = oteltrace.Setup(context.Background(), oteltrace.Config{ServiceName: "s"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "OTEL_EXPORTER_OTLP_TRACES_PROTOCOL")
}

func TestSetup_RequiresServiceName(t *testing.T) {
	saveGlobals(t)
	t.Setenv("OTEL_TRACES_EXPORTER", "console")
	_, err := oteltrace.Setup(context.Background(), oteltrace.Config{Console: &bytes.Buffer{}})
	require.Error(t, err)
}

func TestSetup_DeadCollectorDoesNotBlock(t *testing.T) {
	for _, proto := range []string{"grpc", "http/protobuf"} {
		t.Run(proto, func(t *testing.T) {
			saveGlobals(t)
			t.Setenv("OTEL_TRACES_EXPORTER", "otlp")
			t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", proto)
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1")

			start := time.Now()
			shutdown, err := oteltrace.Setup(context.Background(), oteltrace.Config{ServiceName: "s",
				Logger: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))})
			require.NoError(t, err)
			assert.Less(t, time.Since(start), time.Second)

			start = time.Now()
			for i := 0; i < 10; i++ {
				_, sp := otel.Tracer("x").Start(context.Background(), "s")
				sp.End()
			}
			assert.Less(t, time.Since(start), time.Second)

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			start = time.Now()
			_ = shutdown(ctx)
			assert.Less(t, time.Since(start), 3*time.Second)
		})
	}
}

func TestSetup_SamplerFromEnv(t *testing.T) {
	saveGlobals(t)
	t.Setenv("OTEL_TRACES_EXPORTER", "console")
	t.Setenv("OTEL_TRACES_SAMPLER", "always_off")
	var buf bytes.Buffer
	shutdown, err := oteltrace.Setup(context.Background(), oteltrace.Config{ServiceName: "s", Console: &buf})
	require.NoError(t, err)
	_, sp := otel.Tracer("x").Start(context.Background(), "never-seen")
	sp.End()
	require.NoError(t, shutdown(context.Background()))
	assert.NotContains(t, buf.String(), "never-seen")
}

type recHandler struct {
	mu   sync.Mutex
	recs []slog.Record
}

func (h *recHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *recHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.recs = append(h.recs, r)
	return nil
}
func (h *recHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recHandler) WithGroup(string) slog.Handler      { return h }

func TestSetup_ExporterErrorsAreRateLimited(t *testing.T) {
	saveGlobals(t)
	t.Setenv("OTEL_TRACES_EXPORTER", "console")
	h := &recHandler{}
	shutdown, err := oteltrace.Setup(context.Background(), oteltrace.Config{ServiceName: "s",
		Console: &bytes.Buffer{}, Logger: slog.New(h)})
	require.NoError(t, err)
	defer func() { _ = shutdown(context.Background()) }()

	for i := 0; i < 5; i++ {
		otel.Handle(assert.AnError)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	require.Len(t, h.recs, 1)
	assert.Equal(t, slog.LevelWarn, h.recs[0].Level)
	assert.Equal(t, "otel export error", h.recs[0].Message)
}

func TestHasParent(t *testing.T) {
	assert.False(t, oteltrace.HasParent(context.Background()))

	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(tracetest.NewSpanRecorder()))
	ctx, sp := tp.Tracer("x").Start(context.Background(), "s")
	defer sp.End()
	assert.True(t, oteltrace.HasParent(ctx))

	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{1},
		SpanID:     trace.SpanID{2},
		Remote:     true,
		TraceFlags: 0,
	})
	assert.True(t, oteltrace.HasParent(trace.ContextWithRemoteSpanContext(context.Background(), sc)))
}

var _ = strings.TrimSpace

func TestSetup_MalformedResourceAttributesIsWarnNotFatal(t *testing.T) {
	saveGlobals(t)
	t.Setenv("OTEL_TRACES_EXPORTER", "console")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "foo")
	h := &recHandler{}
	var buf bytes.Buffer
	shutdown, err := oteltrace.Setup(context.Background(), oteltrace.Config{ServiceName: "s",
		Console: &buf, Logger: slog.New(h)})
	require.NoError(t, err)
	_, sp := otel.Tracer("x").Start(context.Background(), "still-traced")
	sp.End()
	require.NoError(t, shutdown(context.Background()))
	assert.Contains(t, buf.String(), "still-traced")
	h.mu.Lock()
	defer h.mu.Unlock()
	// Một WARN trực tiếp của Setup; SDK có thể báo thêm cùng lỗi qua error handler (otel export error).
	require.NotEmpty(t, h.recs)
	assert.Contains(t, h.recs[0].Message, "resource")
	for _, r := range h.recs {
		assert.Equal(t, slog.LevelWarn, r.Level)
	}
}

func TestSetup_InvalidSamplerIsReportedThroughLogger(t *testing.T) {
	saveGlobals(t)
	t.Setenv("OTEL_TRACES_EXPORTER", "console")
	t.Setenv("OTEL_TRACES_SAMPLER", "bogus")
	h := &recHandler{}
	var buf bytes.Buffer
	shutdown, err := oteltrace.Setup(context.Background(), oteltrace.Config{ServiceName: "s",
		Console: &buf, Logger: slog.New(h)})
	require.NoError(t, err)
	_, sp := otel.Tracer("x").Start(context.Background(), "sampled-anyway")
	sp.End()
	require.NoError(t, shutdown(context.Background()))
	assert.Contains(t, buf.String(), "sampled-anyway")
	h.mu.Lock()
	defer h.mu.Unlock()
	require.NotEmpty(t, h.recs)
	assert.Equal(t, slog.LevelWarn, h.recs[0].Level)
}
