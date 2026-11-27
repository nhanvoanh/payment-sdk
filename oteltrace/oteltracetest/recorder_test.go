package oteltracetest_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/nhanvoanh/payment-sdk/oteltrace/oteltracetest"
)

func TestLeaks(t *testing.T) {
	rec := oteltracetest.Install(t)
	_, sp := otel.Tracer("x").Start(context.Background(), "dirty")
	sp.SetAttributes(attribute.String("k1", "a-SECRET-b"))
	sp.AddEvent("ev", trace.WithAttributes(attribute.String("k2", "SECRET")))
	sp.End()
	_, ok := otel.Tracer("x").Start(context.Background(), "clean")
	ok.SetAttributes(attribute.String("k", "fine"))
	ok.End()

	ended := rec.Ended()
	assert.Len(t, ended, 2)
	got := oteltracetest.Leaks(toRO(ended), "SECRET")
	assert.Len(t, got, 2)
	assert.Contains(t, got[0], "span dirty: ")
	assert.Empty(t, oteltracetest.Leaks(toRO(ended[1:]), "SECRET"))
}

func toRO(s []sdktrace.ReadOnlySpan) []sdktrace.ReadOnlySpan { return s }
