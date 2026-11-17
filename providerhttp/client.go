// Package providerhttp là client HTTP dùng chung cho connector gọi API đối tác.
//
// Việc quan trọng nhất của nó là phân loại lỗi đúng luật tiền:
//   - request CHƯA ghi byte nào ra kết nối (không phân giải được tên, từ chối kết nối, TLS hỏng,
//     ctx đã hết trước khi gửi...) → lỗi bọc connector.ErrProviderUnavailable: đối tác chắc chắn
//     chưa xử lý, core chốt thất bại.
//   - mọi lỗi từ lúc đã ghi (hết hạn, đứt kết nối, thân phản hồi hỏng hoặc quá lớn...) → lỗi bọc
//     connector.ErrUnknownOutcome: đối tác có thể đã trừ tiền, core giữ giao dịch chờ truy vấn.
//
// Khi nghi ngờ luôn chọn ErrUnknownOutcome. Phản hồi HTTP nào đọc trọn thân, kể cả 4xx/5xx, là
// (*Response, nil): ý nghĩa của mã trạng thái do connector quyết.
package providerhttp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"bitbucket.org/hasaki-tech/payment-connector-sdk/connector"
)

const (
	defaultMaxConns         = 200
	defaultMaxResponseBytes = 1 << 20
	rawCap                  = 64 << 10
)

// Config cấu hình Client.
type Config struct {
	// Timeout là hạn tổng cho MỘT lời gọi Do (kết nối, gửi, đọc hết thân); bắt buộc > 0. Ghép với
	// hạn của ctx request, lấy cái sớm hơn.
	Timeout time.Duration
	// MaxConns đặt cho cả MaxIdleConnsPerHost và MaxConnsPerHost; mặc định 200.
	MaxConns int
	// MaxResponseBytes giới hạn thân phản hồi và cả thân request; mặc định 1 MiB.
	MaxResponseBytes int64
	// Redact che dữ liệu nhạy cảm trong thân trước khi vào Raw (nil: giữ nguyên). Không ảnh hưởng
	// Response.Body.
	Redact func(body []byte) []byte
	// Transport chỉ để test. Phải gọi các hook httptrace.ClientTrace trong ctx của request; transport
	// không gọi hook sẽ khiến mọi lỗi bị xem là chưa gửi.
	Transport http.RoundTripper
}

// Client an toàn cho nhiều goroutine dùng chung.
type Client struct {
	hc      *http.Client
	timeout time.Duration
	max     int64
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
	if cfg.Timeout <= 0 {
		return nil, errors.New("providerhttp: Timeout bắt buộc > 0")
	}
	if cfg.MaxConns <= 0 {
		cfg.MaxConns = defaultMaxConns
	}
	if cfg.MaxResponseBytes <= 0 {
		cfg.MaxResponseBytes = defaultMaxResponseBytes
	}
	rt := cfg.Transport
	if rt == nil {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.MaxIdleConns = cfg.MaxConns
		tr.MaxIdleConnsPerHost = cfg.MaxConns
		tr.MaxConnsPerHost = cfg.MaxConns
		rt = tr
	}
	return &Client{
		hc: &http.Client{
			Transport:     rt,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		timeout: cfg.Timeout,
		max:     cfg.MaxResponseBytes,
		redact:  cfg.Redact,
	}, nil
}

// Do gửi req (dùng req.Context()) và đọc trọn phản hồi. Không theo redirect, không tự thử lại.
// Thân request được đọc hết vào bộ nhớ trước khi gửi (tối đa MaxResponseBytes); vượt thì trả lỗi thường,
// không bọc sentinel, và không gửi gì.
func (c *Client) Do(req *http.Request) (*Response, error) {
	var reqBody []byte
	if req.Body != nil && req.Body != http.NoBody {
		b, err := io.ReadAll(io.LimitReader(req.Body, c.max+1))
		req.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("providerhttp: đọc thân request: %w", err)
		}
		if int64(len(b)) > c.max {
			return nil, fmt.Errorf("providerhttp: thân request vượt %d byte", c.max)
		}
		reqBody = b
	}

	ctx, cancel := context.WithTimeout(req.Context(), c.timeout)
	defer cancel()

	// wrote: đã có byte của request ra kết nối ở BẤT KỲ lần thử nào (net/http có thể tự gửi lại trên
	// kết nối khác); một khi đã đặt thì không bao giờ xoá.
	var wrote atomic.Bool
	mark := func() { wrote.Store(true) }
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
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

	start := time.Now()
	raw := connector.RawExchange{RequestBody: c.rawBody(reqBody)}
	fail := func(cause error) error {
		raw.DurationMS = time.Since(start).Milliseconds()
		sentinel := connector.ErrUnknownOutcome
		if !wrote.Load() {
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

	body, err := io.ReadAll(io.LimitReader(resp.Body, c.max+1))
	if err == nil && int64(len(body)) > c.max {
		body = body[:c.max]
		err = fmt.Errorf("providerhttp: thân phản hồi vượt %d byte", c.max)
	}
	raw.ResponseBody = c.rawBody(body)
	if err != nil {
		return nil, fail(err)
	}
	raw.DurationMS = time.Since(start).Milliseconds()
	return &Response{Status: resp.StatusCode, Header: resp.Header, Body: body, Raw: raw}, nil
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
