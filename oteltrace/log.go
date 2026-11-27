package oteltrace

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/trace"
)

// LogAttrs trả {"trace_id", id, "span_id", id} cho ctx có span hợp lệ, ngược lại nil.
func LogAttrs(ctx context.Context) []any {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return nil
	}
	return []any{"trace_id", sc.TraceID().String(), "span_id", sc.SpanID().String()}
}

// NewLogHandler bọc h, thêm trace_id/span_id vào mọi record có span trong ctx.
func NewLogHandler(h slog.Handler) slog.Handler { return logHandler{h} }

type logHandler struct{ slog.Handler }

func (h logHandler) Handle(ctx context.Context, r slog.Record) error {
	if a := LogAttrs(ctx); a != nil {
		r = r.Clone()
		r.Add(a...)
	}
	return h.Handler.Handle(ctx, r)
}

func (h logHandler) WithAttrs(as []slog.Attr) slog.Handler {
	return logHandler{h.Handler.WithAttrs(as)}
}
func (h logHandler) WithGroup(n string) slog.Handler { return logHandler{h.Handler.WithGroup(n)} }
