// Package tokencache giữ token truy cập của đối tác: mỗi lúc chỉ một lần lấy token chạy,
// người gọi chờ theo ctx của chính mình.
package tokencache

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

const (
	defaultSkew         = 30 * time.Second
	defaultFetchTimeout = 10 * time.Second
)

// Fetch lấy token mới từ đối tác cùng thời điểm hết hạn. Phải tôn trọng ctx: nếu không,
// FetchTimeout không cứu được và người gọi không có hạn sẽ chờ mãi.
// Token rỗng, hoặc expiresAt zero/đã qua, bị coi là lỗi.
type Fetch func(ctx context.Context) (token string, expiresAt time.Time, err error)

// Options: các trường zero dùng mặc định (Skew 30s, FetchTimeout 10s, Now = time.Now).
type Options struct {
	Skew         time.Duration // làm mới khi còn ít hơn hoặc bằng khoảng này tới hạn
	FetchTimeout time.Duration // trần thời gian một lần fetch, độc lập với ctx người gọi
	Now          func() time.Time
}

// call là một lần fetch đang chạy; tok/exp/err chỉ đọc sau khi done đóng.
type call struct {
	done chan struct{}
	tok  string
	exp  time.Time
	err  error
}

// Cache an toàn cho nhiều goroutine.
type Cache struct {
	fetch Fetch
	opts  Options

	mu       sync.Mutex
	token    string
	expires  time.Time
	inflight *call
}

// New tạo cache; fetch không được nil.
func New(fetch Fetch, opts Options) *Cache {
	if opts.Skew <= 0 {
		opts.Skew = defaultSkew
	}
	if opts.FetchTimeout <= 0 {
		opts.FetchTimeout = defaultFetchTimeout
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Cache{fetch: fetch, opts: opts}
}

// Get trả token còn hạn, hoặc lấy mới (gộp các người gọi đồng thời vào một lần fetch).
// Ctx của người gọi chỉ quyết định họ chờ bao lâu; nó không huỷ lần fetch.
func (c *Cache) Get(ctx context.Context) (string, error) {
	c.mu.Lock()
	if c.token != "" && c.opts.Now().Add(c.opts.Skew).Before(c.expires) {
		tok := c.token
		c.mu.Unlock()
		return tok, nil
	}
	cl := c.inflight
	if cl == nil {
		cl = &call{done: make(chan struct{})}
		c.inflight = cl
		go c.run(cl)
	}
	c.mu.Unlock()

	select {
	case <-cl.done:
		return cl.tok, cl.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// run chạy một lần fetch. Dù fetch panic, inflight vẫn được gỡ và done vẫn đóng,
// để cache không kẹt và người chờ nhận lỗi.
func (c *Cache) run(cl *call) {
	defer func() {
		if r := recover(); r != nil {
			cl.tok, cl.err = "", fmt.Errorf("tokencache: fetch panic: %v", r)
		}
		// Goexit (recover trả nil) và token rỗng đều rơi vào đây: không bao giờ trao ("", nil).
		if cl.err == nil && cl.tok == "" {
			cl.err = errors.New("tokencache: fetch trả token rỗng")
		}
		c.mu.Lock()
		if cl.err == nil && !cl.exp.After(c.opts.Now()) {
			cl.tok, cl.err = "", errors.New("tokencache: fetch trả token không có hạn hoặc đã hết hạn")
		}
		if cl.err == nil {
			c.token, c.expires = cl.tok, cl.exp
		}
		c.inflight = nil
		c.mu.Unlock()
		close(cl.done)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), c.opts.FetchTimeout)
	defer cancel()
	cl.tok, cl.exp, cl.err = c.fetch(ctx)
}

// Invalidate xoá token đang giữ chỉ khi nó đúng là token; lời gọi 401 cũ không xoá token mới.
func (c *Cache) Invalidate(token string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token == token {
		c.token, c.expires = "", time.Time{}
	}
}
