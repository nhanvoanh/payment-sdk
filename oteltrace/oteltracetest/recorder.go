// Package oteltracetest là bộ-test cho tracing: cài TracerProvider ghi span trong bộ nhớ và dò rò rỉ dữ liệu nhạy
// cảm. Chỉ dùng trong _test.go (xem rule test-kits).
package oteltracetest

import (
	"context"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// Install đặt TracerProvider toàn cục (AlwaysSample, SpanRecorder) và propagator W3C TraceContext, khôi phục cả hai
// trong t.Cleanup. Không an toàn khi chạy song song.
func Install(t testing.TB) *tracetest.SpanRecorder {
	t.Helper()
	prevTP, prevProp := otel.GetTracerProvider(), otel.GetTextMapPropagator()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()), sdktrace.WithSpanProcessor(rec))
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
		_ = tp.Shutdown(context.Background())
	})
	return rec
}

// Leaks trả "span <name>: <key>" cho mọi thuộc tính/sự kiện/mô tả status của span đã kết thúc chứa needle nào đó;
// rỗng nghĩa là sạch.
func Leaks(spans []sdktrace.ReadOnlySpan, needles ...string) []string {
	var out []string
	has := func(s string) bool {
		for _, n := range needles {
			if n != "" && strings.Contains(s, n) {
				return true
			}
		}
		return false
	}
	for _, sp := range spans {
		for _, a := range sp.Attributes() {
			if has(a.Value.Emit()) {
				out = append(out, "span "+sp.Name()+": "+string(a.Key))
			}
		}
		for _, ev := range sp.Events() {
			for _, a := range ev.Attributes {
				if has(a.Value.Emit()) {
					out = append(out, "span "+sp.Name()+": "+ev.Name+"."+string(a.Key))
				}
			}
		}
		if has(sp.Status().Description) {
			out = append(out, "span "+sp.Name()+": status.description")
		}
	}
	return out
}
