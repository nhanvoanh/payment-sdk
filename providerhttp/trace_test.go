package providerhttp_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	oteltraceapi "go.opentelemetry.io/otel/trace"

	"github.com/nhanvoanh/payment-sdk/oteltrace"
	"github.com/nhanvoanh/payment-sdk/oteltrace/oteltracetest"
	"github.com/nhanvoanh/payment-sdk/providerhttp"
)

func attrMap(sp sdktrace.ReadOnlySpan) map[string]attribute.Value {
	m := map[string]attribute.Value{}
	for _, a := range sp.Attributes() {
		m[string(a.Key)] = a.Value
	}
	return m
}

func postReq(ctx context.Context, t *testing.T, url, body string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func httpSpans(spans []sdktrace.ReadOnlySpan) []sdktrace.ReadOnlySpan {
	var out []sdktrace.ReadOnlySpan
	for _, s := range spans {
		if strings.HasPrefix(s.Name(), "HTTP ") {
			out = append(out, s)
		}
	}
	return out
}

func TestDo_SpanUnderParent(t *testing.T) {
	rec := oteltracetest.Install(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = w.Write([]byte("hello-response"))
	}))
	defer srv.Close()

	ctx, parent := otel.Tracer("t").Start(context.Background(), "parent")
	c := newClient(t, providerhttp.Config{})
	if _, err := c.Do(postReq(ctx, t, srv.URL+"/v2/create", "12345")); err != nil {
		t.Fatal(err)
	}
	parent.End()

	hs := httpSpans(rec.Ended())
	if len(hs) != 1 {
		t.Fatalf("spans = %d, want 1", len(hs))
	}
	sp := hs[0]
	if sp.Name() != "HTTP POST" || sp.SpanKind() != oteltraceapi.SpanKindClient {
		t.Fatalf("name/kind = %s/%v", sp.Name(), sp.SpanKind())
	}
	if sp.Parent().SpanID() != parent.SpanContext().SpanID() {
		t.Fatal("not a child of parent")
	}
	if sp.InstrumentationScope().Name != oteltrace.TracerName+"/providerhttp" {
		t.Fatalf("scope = %s", sp.InstrumentationScope().Name)
	}
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	a := attrMap(sp)
	if len(a) != 8 {
		t.Fatalf("attrs = %v", sp.Attributes())
	}
	checks := map[string]any{
		"http.request.method": "POST", "server.address": host, "server.port": mustAtoi(t, port),
		"url.path": "/v2/create", "http.response.status_code": 200,
		"http.request.body.size": 5, "http.response.body.size": len("hello-response"),
		"payment.call.outcome": "ok",
	}
	for k, want := range checks {
		v, ok := a[k]
		if !ok {
			t.Fatalf("missing %s", k)
		}
		switch w := want.(type) {
		case string:
			if v.AsString() != w {
				t.Fatalf("%s = %s, want %s", k, v.AsString(), w)
			}
		case int:
			if v.AsInt64() != int64(w) {
				t.Fatalf("%s = %d, want %d", k, v.AsInt64(), w)
			}
		}
	}
	if sp.Status().Code == codes.Error {
		t.Fatal("status should not be Error")
	}
}

func mustAtoi(t *testing.T, s string) int {
	t.Helper()
	n := 0
	for _, ch := range s {
		n = n*10 + int(ch-'0')
	}
	return n
}

func TestDo_NoParentNoSpan(t *testing.T) {
	rec := oteltracetest.Install(t)
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	c := newClient(t, providerhttp.Config{})
	if _, err := c.Do(postReq(context.Background(), t, srv.URL+"/x", "b")); err != nil {
		t.Fatal(err)
	}
	if n := len(rec.Ended()); n != 0 {
		t.Fatalf("spans = %d, want 0", n)
	}
}

func TestDo_NoTraceparentToPartner(t *testing.T) {
	oteltracetest.Install(t)
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = r.Header.Clone() }))
	defer srv.Close()
	ctx, parent := otel.Tracer("t").Start(context.Background(), "parent")
	defer parent.End()
	c := newClient(t, providerhttp.Config{})
	if _, err := c.Do(postReq(ctx, t, srv.URL+"/x", "b")); err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"Traceparent", "Tracestate"} {
		if got.Get(h) != "" {
			t.Fatalf("%s leaked to partner", h)
		}
	}
}

func TestDo_NoQueryOrSecretsInSpan(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
		url     func(srv *httptest.Server) string
		outcome string
	}{
		{"success", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("SENTINEL-R")) },
			func(s *httptest.Server) string { return s.URL + "/pay?sig=SENTINEL-Q" }, "ok"},
		{"midresponse", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", "1000")
			_, _ = w.Write([]byte("SENTINEL-R"))
			w.(http.Flusher).Flush()
			conn, _, _ := w.(http.Hijacker).Hijack()
			_ = conn.Close()
		}, func(s *httptest.Server) string { return s.URL + "/pay?sig=SENTINEL-Q" }, "unknown"},
		{"unreachable", nil, func(*httptest.Server) string {
			l, _ := net.Listen("tcp", "127.0.0.1:0")
			addr := l.Addr().String()
			_ = l.Close()
			return "http://" + addr + "/pay?sig=SENTINEL-Q"
		}, "unavailable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := oteltracetest.Install(t)
			var srv *httptest.Server
			if tc.handler != nil {
				srv = httptest.NewServer(tc.handler)
				defer srv.Close()
			}
			ctx, parent := otel.Tracer("t").Start(context.Background(), "parent")
			c := newClient(t, providerhttp.Config{})
			_, err := c.Do(postReq(ctx, t, tc.url(srv), "SENTINEL-B"))
			parent.End()
			if (err != nil) != (tc.outcome != "ok") {
				t.Fatalf("err = %v", err)
			}
			spans := rec.Ended()
			if l := oteltracetest.Leaks(spans, "SENTINEL"); len(l) != 0 {
				t.Fatalf("leaks: %v", l)
			}
			hs := httpSpans(spans)
			if len(hs) != 1 {
				t.Fatalf("http spans = %d", len(hs))
			}
			if got := attrMap(hs[0])["payment.call.outcome"].AsString(); got != tc.outcome {
				t.Fatalf("outcome = %s, want %s", got, tc.outcome)
			}
			if tc.outcome != "ok" && hs[0].Status().Description != tc.outcome {
				t.Fatalf("status desc = %q", hs[0].Status().Description)
			}
		})
	}
}

func TestDo_TimeoutOutcome(t *testing.T) {
	rec := oteltracetest.Install(t)
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		time.Sleep(300 * time.Millisecond)
	}))
	defer srv.Close()
	ctx, parent := otel.Tracer("t").Start(context.Background(), "parent")
	c := newClient(t, providerhttp.Config{Timeout: 100 * time.Millisecond})
	if _, err := c.Do(postReq(ctx, t, srv.URL+"/slow", "b")); err == nil {
		t.Fatal("want error")
	}
	parent.End()
	hs := httpSpans(rec.Ended())
	if len(hs) != 1 {
		t.Fatalf("http spans = %d", len(hs))
	}
	if hs[0].Status().Code != codes.Error || attrMap(hs[0])["payment.call.outcome"].AsString() != "unknown" {
		t.Fatalf("status=%v attrs=%v", hs[0].Status(), hs[0].Attributes())
	}
}
