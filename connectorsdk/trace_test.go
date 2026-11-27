package connectorsdk

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/nhanvoanh/payment-sdk/connector"
	"github.com/nhanvoanh/payment-sdk/connectorpb"
	"github.com/nhanvoanh/payment-sdk/connectorrpc"
	"github.com/nhanvoanh/payment-sdk/connectortest"
	"github.com/nhanvoanh/payment-sdk/oteltrace/oteltracetest"
	"github.com/nhanvoanh/payment-sdk/paykit"
	"github.com/nhanvoanh/payment-sdk/providerhttp"
)

// tracedEnv chạy connector "fake" (CreatePayment gọi đối tác httptest qua providerhttp) trên ServeListener insecure.
type tracedEnv struct {
	rec  *tracetest.SpanRecorder
	logs *syncBuf
	conn connectorpb.ConnectorServiceClient
	stop func()
}

func newTracedEnv(t *testing.T) *tracedEnv {
	t.Helper()
	rec := oteltracetest.Install(t)
	partner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(partner.Close)
	hc, err := providerhttp.New(providerhttp.Config{Timeout: 5 * time.Second})
	require.NoError(t, err)

	f := connectortest.New("fake", paykit.MethodQR)
	f.OnCreate = func(ctx context.Context, _ connector.CreatePaymentRequest) (connector.CreatePaymentResult, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, partner.URL+"/pay", strings.NewReader("{}"))
		if err != nil {
			return connector.CreatePaymentResult{}, err
		}
		if _, err := hc.Do(req); err != nil {
			return connector.CreatePaymentResult{}, err
		}
		return connector.CreatePaymentResult{ProviderRef: "r", Status: connector.StatusPending}, nil
	}

	logs := &syncBuf{}
	r := start(t, f, Options{Insecure: true, Logger: slog.New(slog.NewJSONHandler(logs, nil))})
	conn, err := grpc.NewClient(r.addr, grpc.WithTransportCredentials(insecure.NewCredentials()),
		connectorrpc.TracingDialOption("fake"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return &tracedEnv{rec: rec, logs: logs, conn: connectorpb.NewConnectorServiceClient(conn), stop: func() {
		_ = conn.Close()
		r.cancel()
		<-r.exited
	}}
}

func spanByKind(spans []sdktrace.ReadOnlySpan, k trace.SpanKind, prefix string) []sdktrace.ReadOnlySpan {
	var out []sdktrace.ReadOnlySpan
	for _, s := range spans {
		if s.SpanKind() == k && strings.HasPrefix(s.Name(), prefix) {
			out = append(out, s)
		}
	}
	return out
}

func TestServe_TracedCallEndToEnd(t *testing.T) {
	e := newTracedEnv(t)
	ctx, parent := otel.Tracer("test").Start(ctx5(t), "parent")
	_, err := e.conn.CreatePayment(ctx, createReq())
	require.NoError(t, err)
	parent.End()
	e.stop()

	spans := e.rec.Ended()
	client := spanByKind(spans, trace.SpanKindClient, "hasaki.payment.connector.v1.ConnectorService/CreatePayment")
	server := spanByKind(spans, trace.SpanKindServer, "")
	httpS := spanByKind(spans, trace.SpanKindClient, "HTTP POST")
	require.Len(t, client, 1)
	require.Len(t, server, 1)
	require.Len(t, httpS, 1)
	assert.Equal(t, server[0].SpanContext().SpanID(), httpS[0].Parent().SpanID())
	assert.Equal(t, parent.SpanContext().TraceID(), server[0].SpanContext().TraceID())
	assert.Equal(t, parent.SpanContext().TraceID(), httpS[0].SpanContext().TraceID())
}

func TestServe_UntracedCallFromCore(t *testing.T) {
	e := newTracedEnv(t)
	_, err := e.conn.CreatePayment(ctx5(t), createReq())
	require.NoError(t, err)
	e.stop()
	assert.Empty(t, e.rec.Ended())
}

func rpcLine(t *testing.T, logs string) map[string]any {
	t.Helper()
	for _, l := range strings.Split(strings.TrimSpace(logs), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(l), &m) == nil && m["msg"] == "rpc" {
			return m
		}
	}
	t.Fatalf("no rpc log line in %q", logs)
	return nil
}

func TestServe_RPCLogHasTraceID(t *testing.T) {
	e := newTracedEnv(t)
	ctx, parent := otel.Tracer("test").Start(ctx5(t), "parent")
	_, err := e.conn.CreatePayment(ctx, createReq())
	require.NoError(t, err)
	parent.End()
	e.stop()
	line := rpcLine(t, e.logs.String())
	assert.Equal(t, parent.SpanContext().TraceID().String(), line["trace_id"])
	assert.NotEmpty(t, line["span_id"])

	u := newTracedEnv(t)
	_, err = u.conn.CreatePayment(ctx5(t), createReq())
	require.NoError(t, err)
	u.stop()
	assert.NotContains(t, rpcLine(t, u.logs.String()), "trace_id")
}

func TestMain_ServiceName(t *testing.T) {
	env := map[string]string{"CONNECTOR_ENV": "staging"}
	cfg := traceConfig(connectortest.New("fake"), func(k string) string { return env[k] })
	assert.Equal(t, "payment-connector-fake", cfg.ServiceName)
	assert.Equal(t, "staging", cfg.Environment)
	assert.NotEmpty(t, cfg.ServiceVersion)
	assert.NotEqual(t, "(devel)", cfg.ServiceVersion)
}
