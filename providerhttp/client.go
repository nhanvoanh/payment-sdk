// Package providerhttp là client HTTP dùng chung cho connector gọi API đối tác.
//
// Việc quan trọng nhất của nó là phân loại lỗi đúng luật tiền. Cần CHỨNG CỨ DƯƠNG rằng chưa gửi gì
// mới trả connector.ErrProviderUnavailable (đối tác chắc chắn chưa xử lý, core chốt thất bại). Chứng
// cứ là một trong:
//   - ctx đã hết hạn trước khi bắt đầu;
//   - thân request không đọc được hoặc vượt MaxRequestBytes (chưa gửi gì);
//   - transport đã bắt đầu lấy kết nối (hook GetConn) nhưng chưa từng có kết nối nào (GotConn), không
//     hook Wrote* nào bắn và chưa có phản hồi: không phân giải được tên, từ chối kết nối, TLS hỏng...
//
// Mọi trường hợp còn lại là connector.ErrUnknownOutcome (đối tác có thể đã trừ tiền, core giữ giao
// dịch chờ truy vấn): hết hạn hay đứt sau khi đã có kết nối, thân phản hồi hỏng hoặc quá lớn, và cả
// khi không hook nào bắn. Các cờ dính qua mọi lần net/http tự thử lại. Hệ quả chấp nhận: kết nối tái
// dùng đã chết cũng cho unknown (bảo thủ).
//
// HTTP/2 bị tắt có chủ ý: khi ctx hết hạn, roundTrip của h2 trả về mà không chờ goroutine ghi, nên
// frame HEADERS có thể được ghi sau khi ta đã đọc cờ; đường HTTP/1.1 chờ vòng ghi kết thúc trước khi
// trả lỗi nên cờ đáng tin.
//
// Phản hồi HTTP nào đọc trọn thân, kể cả 4xx/5xx, là (*Response, nil): ý nghĩa của mã trạng thái do
// connector quyết. net/http vẫn tự gửi lại request có thể phát lại trên kết nối tái dùng bị đóng (GET,
// và POST có header Idempotency-Key/X-Idempotency-Key); POST không có các header đó không bao giờ bị
// gửi HAI lần sau khi đã ghi byte (request chưa ghi byte nào vẫn có thể được net/http gửi trên kết nối khác).
package providerhttp

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"bitbucket.org/hasaki-tech/payment-connector-sdk/connector"
)

const (
	defaultMaxConns = 200
	defaultMaxBytes = 1 << 20
	rawCap          = 64 << 10
)

// Config cấu hình Client.
type Config struct {
	// Timeout là hạn tổng cho MỘT lời gọi Do (kết nối, gửi, đọc hết thân); bắt buộc > 0. Ghép với
	// hạn của ctx request, lấy cái sớm hơn.
	Timeout time.Duration
	// MaxConns đặt cho cả MaxIdleConnsPerHost và MaxConnsPerHost; mặc định 200.
	MaxConns int
	// MaxResponseBytes giới hạn thân phản hồi; mặc định 1 MiB.
	MaxResponseBytes int64
	// MaxRequestBytes giới hạn thân request; mặc định 1 MiB.
	MaxRequestBytes int64
	// Redact che dữ liệu nhạy cảm trong thân trước khi vào Raw (nil: giữ nguyên). Không ảnh hưởng
	// Response.Body.
	Redact func(body []byte) []byte
	// TLSClientConfig cho đối tác cần mTLS hoặc CA riêng; được clone, và HTTP/2 vẫn tắt.
	TLSClientConfig *tls.Config
}

// Client an toàn cho nhiều goroutine dùng chung.
type Client struct {
	hc      *http.Client
	timeout time.Duration
	maxResp int64
	maxReq  int64
	redact  func([]byte) []byte
}

// Response là phản hồi đọc trọn thân.
type Response struct {
	Status int
	Header http.Header
	Body   []byte
	Raw    connector.RawExchange
}

// Error là lỗi của một lời gọi đã được phân loại. errors.Is với connector.ErrProviderUnavailable hoặc
// connector.ErrUnknownOutcome (đúng một trong hai) và với nguyên nhân gốc đều chạy.
type Error struct {
	// Raw là bản ghi lời gọi tới thời điểm lỗi (HTTPStatus = 0 nếu chưa có phản hồi).
	Raw connector.RawExchange
	err error
}

func (e *Error) Error() string { return e.err.Error() }
func (e *Error) Unwrap() error { return e.err }

// New tạo Client.
func New(cfg Config) (*Client, error) {
	return newClient(cfg, nil)
}

// newClient: rt != nil chỉ để test (inject transport); bản exported không cho gắn transport vì
// transport không bắn hook httptrace hoặc có h2 sẽ phá luật phân loại.
func newClient(cfg Config, rt http.RoundTripper) (*Client, error) {
	if cfg.Timeout <= 0 {
		return nil, errors.New("providerhttp: Timeout bắt buộc > 0")
	}
	if cfg.MaxConns <= 0 {
		cfg.MaxConns = defaultMaxConns
	}
	if cfg.MaxResponseBytes <= 0 {
		cfg.MaxResponseBytes = defaultMaxBytes
	}
	if cfg.MaxRequestBytes <= 0 {
		cfg.MaxRequestBytes = defaultMaxBytes
	}
	if rt == nil {
		// Dựng transport tường minh thay vì Clone DefaultTransport để không thừa hưởng trạng thái ẩn:
		// bản clone mang theo TLSClientConfig.NextProtos=[h2 http/1.1], khiến ta chào h2 trong ALPN trong
		// khi chỉ nói được HTTP/1.1 và mọi request tới đối tác hỗ trợ h2 sẽ hỏng.
		tc := &tls.Config{MinVersion: tls.VersionTLS12}
		if cfg.TLSClientConfig != nil {
			tc = cfg.TLSClientConfig.Clone()
		}
		// Lọc "h2" vô điều kiện; ALPN chỉ chào http/1.1.
		tc.NextProtos = []string{"http/1.1"}
		rt = &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			TLSClientConfig:       tc,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: time.Second,
			IdleConnTimeout:       90 * time.Second,
			MaxIdleConns:          cfg.MaxConns,
			MaxIdleConnsPerHost:   cfg.MaxConns,
			MaxConnsPerHost:       cfg.MaxConns,
			// Tắt HTTP/2 (xem doc gói): ForceAttemptHTTP2=false và TLSNextProto rỗng nhưng khác nil.
			ForceAttemptHTTP2: false,
			TLSNextProto:      map[string]func(string, *tls.Conn) http.RoundTripper{},
		}
	}
	return &Client{
		hc: &http.Client{
			Transport:     rt,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		timeout: cfg.Timeout,
		maxResp: cfg.MaxResponseBytes,
		maxReq:  cfg.MaxRequestBytes,
		redact:  cfg.Redact,
	}, nil
}

// Do gửi req (dùng req.Context()) và đọc trọn phản hồi. Không theo redirect, không tự thử lại.
// Thân request được đọc hết vào bộ nhớ trước khi gửi (tối đa MaxRequestBytes, trong hạn Timeout); lỗi ở
// bước này là *Error bọc ErrProviderUnavailable vì chưa gửi gì.
func (c *Client) Do(req *http.Request) (*Response, error) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(req.Context(), c.timeout)
	defer cancel()

	raw := connector.RawExchange{}
	// failUnsent: chứng cứ dương rằng chưa gửi gì.
	failUnsent := func(cause error) error {
		raw.DurationMS = time.Since(start).Milliseconds()
		return &Error{Raw: raw, err: fmt.Errorf("%w: %w", connector.ErrProviderUnavailable, cause)}
	}

	var reqBody []byte
	if req.Body != nil && req.Body != http.NoBody {
		b, err := c.readRequestBody(ctx, req.Body)
		raw.RequestBody = c.rawBody(b)
		if err != nil {
			return nil, failUnsent(err)
		}
		reqBody = b
	}
	if err := ctx.Err(); err != nil {
		return nil, failUnsent(err)
	}

	// Các cờ dính qua mọi lần thử (net/http có thể tự gửi lại trên kết nối khác), không bao giờ xoá.
	var attempted, gotConn, wrote atomic.Bool
	mark := func() { wrote.Store(true) }
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GetConn:          func(string) { attempted.Store(true) },
		GotConn:          func(httptrace.GotConnInfo) { gotConn.Store(true) },
		WroteHeaderField: func(string, []string) { mark() },
		WroteHeaders:     mark,
		WroteRequest:     func(httptrace.WroteRequestInfo) { mark() },
	})

	out := req.Clone(ctx)
	if reqBody != nil {
		out.Body = io.NopCloser(bytes.NewReader(reqBody))
		out.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(reqBody)), nil }
		out.ContentLength = int64(len(reqBody))
	}

	fail := func(cause error) error {
		raw.DurationMS = time.Since(start).Milliseconds()
		sentinel := connector.ErrUnknownOutcome
		if attempted.Load() && !gotConn.Load() && !wrote.Load() {
			sentinel = connector.ErrProviderUnavailable
		}
		return &Error{Raw: raw, err: fmt.Errorf("%w: %w", sentinel, cause)}
	}

	resp, err := c.hc.Do(out)
	if err != nil {
		return nil, fail(err)
	}
	defer resp.Body.Close()
	// Đã có phản hồi thì chắc chắn đã gửi, dù transport không báo hook.
	mark()
	raw.HTTPStatus = resp.StatusCode

	body, err := io.ReadAll(io.LimitReader(resp.Body, c.maxResp+1))
	if err == nil && int64(len(body)) > c.maxResp {
		body = body[:c.maxResp]
		err = fmt.Errorf("providerhttp: thân phản hồi vượt %d byte", c.maxResp)
	}
	raw.ResponseBody = c.rawBody(body)
	if err != nil {
		return nil, fail(err)
	}
	raw.DurationMS = time.Since(start).Milliseconds()
	return &Response{Status: resp.StatusCode, Header: resp.Header, Body: body, Raw: raw}, nil
}

// readRequestBody đọc trọn thân trong hạn ctx. Đọc ở goroutine riêng để hạn có hiệu lực kể cả khi
// reader của người gọi bị treo; khi hạn hết, goroutine đó tự kết thúc lúc reader trả về.
func (c *Client) readRequestBody(ctx context.Context, body io.ReadCloser) ([]byte, error) {
	type result struct {
		b   []byte
		err error
	}
	ch := make(chan result, 1)
	go func() {
		defer body.Close()
		b, err := io.ReadAll(io.LimitReader(body, c.maxReq+1))
		if err != nil {
			err = fmt.Errorf("providerhttp: đọc thân request: %w", err)
		} else if int64(len(b)) > c.maxReq {
			b, err = b[:c.maxReq], fmt.Errorf("providerhttp: thân request vượt %d byte", c.maxReq)
		}
		ch <- result{b, err}
	}()
	select {
	case r := <-ch:
		return r.b, r.err
	case <-ctx.Done():
		_ = body.Close() // gỡ reader đang treo để goroutine không rò
		return nil, fmt.Errorf("providerhttp: đọc thân request: %w", ctx.Err())
	}
}

// rawBody che rồi cắt còn rawCap byte (không cắt giữa một ký tự UTF-8).
func (c *Client) rawBody(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	if c.redact != nil {
		b = c.redact(append([]byte(nil), b...))
	}
	if len(b) > rawCap {
		b = b[:rawCap]
		for len(b) > 0 && !utf8.Valid(b) {
			r, size := utf8.DecodeLastRune(b)
			if r != utf8.RuneError || size != 1 {
				break
			}
			b = b[:len(b)-1]
		}
	}
	return string(b)
}
