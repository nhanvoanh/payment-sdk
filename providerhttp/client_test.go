package providerhttp_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nhanvoanh/payment-sdk/connector"
	"github.com/nhanvoanh/payment-sdk/providerhttp"
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

func newClientRT(t *testing.T, rt http.RoundTripper) *providerhttp.Client {
	t.Helper()
	c, err := providerhttp.NewWithTransport(providerhttp.Config{Timeout: 5 * time.Second}, rt)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func trace(r *http.Request) *httptrace.ClientTrace { return httptrace.ContextClientTrace(r.Context()) }

// Transport thật bắn GetConn rồi mới phân giải tên; lỗi DNS nghĩa là chưa có kết nối.
func TestDo_DNSFailureIsUnavailable(t *testing.T) {
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		trace(r).GetConn("x.invalid:80")
		return nil, &net.DNSError{Err: "no such host", Name: "x.invalid", IsNotFound: true}
	})
	_, err := newClientRT(t, rt).Do(get(t, "http://x.invalid/"))
	_ = requireKind(t, err, connector.ErrProviderUnavailable)
}

// Transport không bắn hook nào: không có chứng cứ dương nên là unknown.
func TestDo_NoHooksIsUnknown(t *testing.T) {
	rt := roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("boom") })
	_, err := newClientRT(t, rt).Do(get(t, "http://x.invalid/"))
	_ = requireKind(t, err, connector.ErrUnknownOutcome)
}

// Đã có kết nối (GotConn) mà lỗi, dù chưa hook Wrote* nào: unknown.
func TestDo_GotConnThenErrorIsUnknown(t *testing.T) {
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		trace(r).GetConn("h:80")
		trace(r).GotConn(httptrace.GotConnInfo{})
		return nil, errors.New("reset")
	})
	_, err := newClientRT(t, rt).Do(get(t, "http://x.invalid/"))
	_ = requireKind(t, err, connector.ErrUnknownOutcome)
}

// Hai lần thử trong một RoundTrip (như net/http tự gửi lại): lần 1 đã ghi header, lần 2 chỉ GetConn rồi
// lỗi dial. Cờ dính nên kết quả vẫn là unknown.
func TestDo_FlagsStickyAcrossAttempts(t *testing.T) {
	var attempts int
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		tr := trace(r)
		attempts++
		tr.GetConn("h:80")
		tr.GotConn(httptrace.GotConnInfo{Reused: true})
		tr.WroteHeaders()
		attempts++
		tr.GetConn("h:80")
		return nil, errors.New("dial tcp: connection refused")
	})
	_, err := newClientRT(t, rt).Do(get(t, "http://x.invalid/"))
	if attempts != 2 {
		t.Fatalf("attempts = %d", attempts)
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

func TestDo_RequestBodyOverLimitIsUnavailableBeforeSend(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer srv.Close()
	c := newClient(t, providerhttp.Config{MaxRequestBytes: 10, MaxResponseBytes: 1})
	_, err := c.Do(post(context.Background(), t, srv.URL, strings.Repeat("a", 11)))
	pe := requireKind(t, err, connector.ErrProviderUnavailable)
	if pe.Raw.RequestBody == "" {
		t.Fatal("Raw phải có thân đã đọc")
	}
	if hits.Load() != 0 {
		t.Fatal("không được gửi")
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("đĩa hỏng") }

func TestDo_RequestBodyReadErrorIsUnavailable(t *testing.T) {
	req := get(t, "http://127.0.0.1:1/")
	req.Method, req.Body = http.MethodPost, io.NopCloser(errReader{})
	_, err := newClient(t, providerhttp.Config{}).Do(req)
	_ = requireKind(t, err, connector.ErrProviderUnavailable)
}

// Reader treo: hạn Timeout vẫn có hiệu lực, vẫn là chưa gửi.
func TestDo_RequestBodyReadHonoursTimeout(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()
	req := get(t, "http://127.0.0.1:1/")
	req.Method, req.Body = http.MethodPost, pr
	c := newClient(t, providerhttp.Config{Timeout: 50 * time.Millisecond})
	_, err := c.Do(req)
	_ = requireKind(t, err, connector.ErrProviderUnavailable)
}

// POST không có Idempotency-Key không bao giờ bị gửi hai lần, kể cả khi server đóng kết nối tái dùng
// sau khi nhận.
func TestDo_PostWithoutIdempotencyKeyNeverResent(t *testing.T) {
	var pays atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if r.URL.Path != "/pay" {
			return
		}
		pays.Add(1)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}))
	defer srv.Close()
	c := newClient(t, providerhttp.Config{})
	if _, err := c.Do(post(context.Background(), t, srv.URL+"/warm", "x")); err != nil {
		t.Fatal(err)
	}
	_, err := c.Do(post(context.Background(), t, srv.URL+"/pay", "x"))
	_ = requireKind(t, err, connector.ErrUnknownOutcome)
	if n := pays.Load(); n != 1 {
		t.Fatalf("server nhận %d lần", n)
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

func tlsServer(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(h)
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

// Client mặc định (TLSClientConfig nil) không chào h2 trong ALPN; server ghi lại ClientHello.
func TestDefaultClientDoesNotOfferH2(t *testing.T) {
	for name, tc := range map[string]*tls.Config{
		"nil":            nil,
		"NextProtos nil": {},
		"NextProtos h2":  {NextProtos: []string{"h2", "http/1.1"}},
	} {
		t.Run(name, func(t *testing.T) {
			var mu sync.Mutex
			var offered []string
			srv := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
			srv.EnableHTTP2 = true
			srv.TLS = &tls.Config{GetConfigForClient: func(h *tls.ClientHelloInfo) (*tls.Config, error) {
				mu.Lock()
				offered = append(offered, h.SupportedProtos...)
				mu.Unlock()
				return nil, nil
			}}
			srv.StartTLS()
			defer srv.Close()
			// Không tin CA của server: handshake hỏng, nhưng ClientHello đã tới server.
			_, _ = newClient(t, providerhttp.Config{TLSClientConfig: tc}).Do(get(t, srv.URL))
			mu.Lock()
			defer mu.Unlock()
			if len(offered) == 0 {
				t.Fatal("server không nhận ClientHello")
			}
			for _, p := range offered {
				if p == "h2" {
					t.Fatalf("client chào h2: %v", offered)
				}
			}
		})
	}
}

// Đầu-cuối: client mặc định gọi thành công tới server TLS bật h2, và server thấy HTTP/1.1.
func TestDefaultClientEndToEndAgainstH2Server(t *testing.T) {
	var proto atomic.Int32
	srv := tlsServer(t, func(_ http.ResponseWriter, r *http.Request) { proto.Store(int32(r.ProtoMajor)) })
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	for _, protos := range [][]string{nil, {"h2", "http/1.1"}} {
		proto.Store(0)
		c := newClient(t, providerhttp.Config{TLSClientConfig: &tls.Config{RootCAs: pool, NextProtos: protos}})
		resp, err := c.Do(get(t, srv.URL))
		if err != nil || resp.Status != 200 {
			t.Fatalf("NextProtos=%v: resp=%v err=%v", protos, resp, err)
		}
		if proto.Load() != 1 {
			t.Fatalf("NextProtos=%v: server thấy HTTP/%d", protos, proto.Load())
		}
	}
}

type hangBody struct {
	closed chan struct{}
	done   chan struct{}
}

func (b *hangBody) Read([]byte) (int, error) {
	defer close(b.done)
	<-b.closed
	return 0, io.ErrClosedPipe
}
func (b *hangBody) Close() error {
	select {
	case <-b.closed:
	default:
		close(b.closed)
	}
	return nil
}

// Hết hạn khi đang đọc thân: body bị đóng để reader treo thoát, không rò goroutine.
func TestDo_RequestBodyTimeoutClosesBody(t *testing.T) {
	b := &hangBody{closed: make(chan struct{}), done: make(chan struct{})}
	req := get(t, "http://127.0.0.1:1/")
	req.Method, req.Body = http.MethodPost, b
	_, err := newClient(t, providerhttp.Config{Timeout: 30 * time.Millisecond}).Do(req)
	_ = requireKind(t, err, connector.ErrProviderUnavailable)
	select {
	case <-b.done:
	case <-time.After(5 * time.Second):
		t.Fatal("reader vẫn treo sau khi hết hạn")
	}
}

// Luật phân loại tự đủ, không dựa vào việc tắt h2: với transport h2 tiêm vào, không request nào server
// đã nhận mà client báo unavailable (ctx hết hạn rải quanh thời điểm ghi frame HEADERS).
func TestDo_HTTP2NeverUnavailableAfterServerReceived(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]bool{}
	var h2 atomic.Bool
	srv := tlsServer(t, func(_ http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor == 2 {
			h2.Store(true)
		}
		mu.Lock()
		seen[r.URL.Query().Get("id")] = true
		mu.Unlock()
	})
	c := newClientRT(t, srv.Client().Transport)
	if _, err := c.Do(get(t, srv.URL+"/?id=warm")); err != nil {
		t.Fatal(err)
	}
	if !h2.Load() {
		t.Fatal("transport tiêm không nói h2, test vô nghĩa")
	}
	var umu sync.Mutex
	var unavailable []string
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		g := g
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 400; i++ {
				id := fmt.Sprintf("%d-%d", g, i)
				ctx, cancel := context.WithTimeout(context.Background(), time.Duration(i%800)*500*time.Nanosecond)
				req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/pay?id="+id, nil)
				req.Header.Set("Authorization", "Bearer x")
				_, err := c.Do(req)
				cancel()
				if errors.Is(err, connector.ErrProviderUnavailable) {
					umu.Lock()
					unavailable = append(unavailable, id)
					umu.Unlock()
				}
			}
		}()
	}
	wg.Wait()
	srv.CloseClientConnections() // chờ handler đang dở xong
	srv.Close()
	mu.Lock()
	defer mu.Unlock()
	for _, id := range unavailable {
		if seen[id] {
			t.Errorf("request %s server đã nhận nhưng client báo unavailable", id)
		}
	}
}
