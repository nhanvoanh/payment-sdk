package providerhttp_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"bitbucket.org/hasaki-tech/payment-connector-sdk/connector"
	"bitbucket.org/hasaki-tech/payment-connector-sdk/providerhttp"
)

func newClient(t *testing.T, cfg providerhttp.Config) *providerhttp.Client {
	t.Helper()
	if cfg.Timeout == 0 {
		cfg.Timeout = 5 * time.Second
	}
	c, err := providerhttp.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func get(t *testing.T, url string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func post(ctx context.Context, t *testing.T, url, body string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return req
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func requireKind(t *testing.T, err error, want error) *providerhttp.Error {
	t.Helper()
	if err == nil {
		t.Fatalf("muốn lỗi %v, được nil", want)
	}
	other := connector.ErrUnknownOutcome
	if want == connector.ErrUnknownOutcome {
		other = connector.ErrProviderUnavailable
	}
	if !errors.Is(err, want) {
		t.Fatalf("muốn errors.Is %v, được %v", want, err)
	}
	if errors.Is(err, other) {
		t.Fatalf("lỗi không được khớp cả %v: %v", other, err)
	}
	var pe *providerhttp.Error
	if !errors.As(err, &pe) {
		t.Fatalf("errors.As *Error thất bại: %v", err)
	}
	return pe
}

func TestNew_RequiresTimeout(t *testing.T) {
	if _, err := providerhttp.New(providerhttp.Config{}); err == nil {
		t.Fatal("Timeout bằng 0 phải bị từ chối")
	}
	if _, err := providerhttp.New(providerhttp.Config{Timeout: -time.Second}); err == nil {
		t.Fatal("Timeout âm phải bị từ chối")
	}
}

func TestDo_ConnectionRefusedIsUnavailable(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	_, err = newClient(t, providerhttp.Config{}).Do(post(context.Background(), t, "http://"+addr, `{"a":1}`))
	pe := requireKind(t, err, connector.ErrProviderUnavailable)
	if pe.Raw.HTTPStatus != 0 || pe.Raw.RequestBody != `{"a":1}` {
		t.Fatalf("Raw = %+v", pe.Raw)
	}
}

func TestDo_DNSFailureIsUnavailable(t *testing.T) {
	rt := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, &net.DNSError{Err: "no such host", Name: "x.invalid", IsNotFound: true}
	})
	_, err := newClient(t, providerhttp.Config{Transport: rt}).Do(get(t, "http://x.invalid/"))
	_ = requireKind(t, err, connector.ErrProviderUnavailable)
}

// Một lần thử đã ghi header rồi lần thử sau (net/http tự gửi lại) thất bại trước khi ghi: vẫn là unknown.
func TestDo_AnyAttemptWroteIsUnknown(t *testing.T) {
	var n int
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		n++
		if n == 1 {
			httptrace.ContextClientTrace(r.Context()).WroteHeaders()
		}
		return nil, errors.New("dial tcp: connection refused")
	})
	_, err := newClient(t, providerhttp.Config{Transport: rt}).Do(get(t, "http://x.invalid/"))
	if n != 1 {
		t.Fatalf("transport giả bị gọi %d lần", n)
	}
	_ = requireKind(t, err, connector.ErrUnknownOutcome)
}

// blockingServer nhận request rồi giữ nguyên cho tới khi test xong; received báo đã nhận.
func blockingServer(t *testing.T) (*httptest.Server, chan struct{}) {
	t.Helper()
	received := make(chan struct{}, 8)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		received <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(release); srv.Close() })
	return srv, received
}

func TestDo_TimeoutAfterSendIsUnknown(t *testing.T) {
	srv, received := blockingServer(t)
	c := newClient(t, providerhttp.Config{Timeout: 200 * time.Millisecond})
	_, err := c.Do(post(context.Background(), t, srv.URL, "x"))
	_ = requireKind(t, err, connector.ErrUnknownOutcome)
	select {
	case <-received:
	default:
		t.Fatal("server chưa nhận request nên test không có ý nghĩa")
	}
}

func TestDo_ContextCancelAfterSendIsUnknown(t *testing.T) {
	srv, received := blockingServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { <-received; cancel() }()
	_, err := newClient(t, providerhttp.Config{}).Do(post(ctx, t, srv.URL, "x"))
	_ = requireKind(t, err, connector.ErrUnknownOutcome)
}

func TestDo_ContextCancelBeforeSendIsUnavailable(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := newClient(t, providerhttp.Config{}).Do(post(ctx, t, srv.URL, "x"))
	_ = requireKind(t, err, connector.ErrProviderUnavailable)
	if hits.Load() != 0 {
		t.Fatal("request không được tới server")
	}
}

func TestDo_ResetAfterSendIsUnknown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}))
	defer srv.Close()
	_, err := newClient(t, providerhttp.Config{}).Do(post(context.Background(), t, srv.URL, "x"))
	_ = requireKind(t, err, connector.ErrUnknownOutcome)
}

func TestDo_TruncatedBodyIsUnknown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "100")
		_, _ = w.Write([]byte("abc"))
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	}))
	defer srv.Close()
	_, err := newClient(t, providerhttp.Config{}).Do(post(context.Background(), t, srv.URL, "x"))
	pe := requireKind(t, err, connector.ErrUnknownOutcome)
	if pe.Raw.HTTPStatus != 200 {
		t.Fatalf("Raw.HTTPStatus = %d", pe.Raw.HTTPStatus)
	}
}

func TestDo_OversizeBodyIsUnknown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("a"), 200))
	}))
	defer srv.Close()
	c := newClient(t, providerhttp.Config{MaxResponseBytes: 100})
	_, err := c.Do(get(t, srv.URL))
	_ = requireKind(t, err, connector.ErrUnknownOutcome)

	// đúng bằng giới hạn thì vẫn đọc được
	c = newClient(t, providerhttp.Config{MaxResponseBytes: 200})
	resp, err := c.Do(get(t, srv.URL))
	if err != nil || len(resp.Body) != 200 {
		t.Fatalf("resp=%v err=%v", resp, err)
	}
}

func TestDo_5xxIsResponseNotError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Req", "1")
		w.WriteHeader(502)
		_, _ = w.Write([]byte("bad gateway"))
	}))
	defer srv.Close()
	resp, err := newClient(t, providerhttp.Config{}).Do(post(context.Background(), t, srv.URL, "req"))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != 502 || string(resp.Body) != "bad gateway" || resp.Header.Get("X-Req") != "1" {
		t.Fatalf("resp = %+v", resp)
	}
	if resp.Raw.HTTPStatus != 502 || resp.Raw.ResponseBody != "bad gateway" || resp.Raw.RequestBody != "req" {
		t.Fatalf("Raw = %+v", resp.Raw)
	}
}

func TestDo_NoRedirectFollow(t *testing.T) {
	var target atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/x" {
			target.Add(1)
			return
		}
		http.Redirect(w, r, "/x", http.StatusFound)
	}))
	defer srv.Close()
	resp, err := newClient(t, providerhttp.Config{}).Do(post(context.Background(), t, srv.URL, "x"))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != 302 || resp.Header.Get("Location") != "/x" || target.Load() != 0 {
		t.Fatalf("status=%d loc=%q target=%d", resp.Status, resp.Header.Get("Location"), target.Load())
	}
}

func TestDo_RawRedactedAndCapped(t *testing.T) {
	big := strings.Repeat("é", 50000) // 100000 byte, hơn 64 KiB, nhiều byte mỗi rune
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "secret-out "+big)
	}))
	defer srv.Close()
	redact := func(b []byte) []byte { return bytes.ReplaceAll(b, []byte("secret"), []byte("***")) }
	c := newClient(t, providerhttp.Config{Redact: redact})
	req := post(context.Background(), t, srv.URL, "secret-in")
	req.Header.Set("Authorization", "Bearer topsecret")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Raw.RequestBody != "***-in" {
		t.Fatalf("RequestBody = %q", resp.Raw.RequestBody)
	}
	if !strings.HasPrefix(resp.Raw.ResponseBody, "***-out ") || len(resp.Raw.ResponseBody) > 64<<10 {
		t.Fatalf("ResponseBody len=%d prefix=%q", len(resp.Raw.ResponseBody), resp.Raw.ResponseBody[:12])
	}
	if !strings.HasSuffix(resp.Raw.ResponseBody, "é") {
		t.Fatal("cắt giữa rune")
	}
	if len(resp.Body) != len("secret-out ")+len(big) {
		t.Fatal("Response.Body phải nguyên vẹn, không redact, không cắt")
	}
	if strings.Contains(resp.Raw.RequestBody+resp.Raw.ResponseBody, "topsecret") {
		t.Fatal("header không được vào Raw")
	}
}

func TestDo_RequestBodyOverLimitFailsBeforeSend(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer srv.Close()
	c := newClient(t, providerhttp.Config{MaxResponseBytes: 10})
	_, err := c.Do(post(context.Background(), t, srv.URL, strings.Repeat("a", 11)))
	if err == nil {
		t.Fatal("muốn lỗi")
	}
	if errors.Is(err, connector.ErrUnknownOutcome) || errors.Is(err, connector.ErrProviderUnavailable) {
		t.Fatalf("lỗi của người gọi không được mang sentinel: %v", err)
	}
	if hits.Load() != 0 {
		t.Fatal("không được gửi")
	}
}

func TestDo_BodyWithoutGetBodyIsSent(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = string(b)
	}))
	defer srv.Close()
	req := get(t, srv.URL)
	req.Method = http.MethodPost
	req.Body = io.NopCloser(io.MultiReader(strings.NewReader("ab"), strings.NewReader("cd"))) // GetBody == nil
	if _, err := newClient(t, providerhttp.Config{}).Do(req); err != nil {
		t.Fatal(err)
	}
	if got != "abcd" {
		t.Fatalf("server nhận %q", got)
	}
}

func TestPool_ReusesConnections(t *testing.T) {
	var conns atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			conns.Add(1)
		}
	}
	srv.Start()
	defer srv.Close()
	c := newClient(t, providerhttp.Config{})
	for i := 0; i < 100; i++ {
		if _, err := c.Do(post(context.Background(), t, srv.URL, "x")); err != nil {
			t.Fatal(err)
		}
	}
	if n := conns.Load(); n > 2 {
		t.Fatalf("mở %d kết nối cho 100 request tuần tự", n)
	}
}
