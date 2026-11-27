package oteltrace_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace"

	"github.com/nhanvoanh/payment-sdk/oteltrace"
)

func spanCtx() (context.Context, trace.SpanContext) {
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: trace.TraceID{0xab, 1}, SpanID: trace.SpanID{0xcd, 2}, TraceFlags: trace.FlagsSampled,
	})
	return trace.ContextWithSpanContext(context.Background(), sc), sc
}

func TestLogAttrs(t *testing.T) {
	assert.Nil(t, oteltrace.LogAttrs(context.Background()))
	ctx, sc := spanCtx()
	assert.Equal(t, []any{"trace_id", sc.TraceID().String(), "span_id", sc.SpanID().String()}, oteltrace.LogAttrs(ctx))
}

func decode(t *testing.T, b *bytes.Buffer) map[string]any {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal(b.Bytes(), &m))
	b.Reset()
	return m
}

func TestNewLogHandler(t *testing.T) {
	var buf bytes.Buffer
	ctx, sc := spanCtx()
	check := func(t *testing.T, l *slog.Logger, group string) {
		t.Helper()
		l.InfoContext(ctx, "m")
		m := decode(t, &buf)
		if group != "" {
			m = m[group].(map[string]any)
		}
		assert.Equal(t, sc.TraceID().String(), m["trace_id"])
		assert.Equal(t, sc.SpanID().String(), m["span_id"])
		l.Info("m")
		m = decode(t, &buf)
		assert.NotContains(t, m, "trace_id")
		if group == "" {
			group = "-"
		}
		assert.NotContains(t, m, group) // nhóm rỗng bị slog bỏ; không có trace_id lọt vào
	}
	base := slog.New(oteltrace.NewLogHandler(slog.NewJSONHandler(&buf, nil)))
	t.Run("plain", func(t *testing.T) { check(t, base, "") })
	t.Run("with-attrs", func(t *testing.T) { check(t, base.With("k", "v"), "") })
	t.Run("with-group", func(t *testing.T) { check(t, base.WithGroup("g"), "g") })
}
