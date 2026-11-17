package tokencache

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newClock() *clock { return &clock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)} }

func TestGet_SingleFlight(t *testing.T) {
	clk := newClock()
	var calls atomic.Int32
	release := make(chan struct{})
	c := New(func(_ context.Context) (string, time.Time, error) {
		calls.Add(1)
		<-release
		return "tok", clk.now().Add(time.Hour), nil
	}, Options{Now: clk.now})

	const n = 100
	var wg sync.WaitGroup
	res := make(chan string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tok, err := c.Get(context.Background())
			if err != nil {
				t.Error(err)
			}
			res <- tok
		}()
	}
	// Chờ đến khi fetch đã bắt đầu rồi mới nhả; các goroutine còn lại đều gom vào cùng lần chạy.
	for calls.Load() == 0 {
		runtime.Gosched()
	}
	close(release)
	wg.Wait()
	close(res)
	for tok := range res {
		if tok != "tok" {
			t.Fatalf("token = %q", tok)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("fetch %d lần, muốn 1", got)
	}
	// Đã có token: không fetch nữa.
	if _, err := c.Get(context.Background()); err != nil || calls.Load() != 1 {
		t.Fatalf("lần sau phải dùng cache, calls=%d err=%v", calls.Load(), err)
	}
}

func TestGet_RefreshesBeforeExpiryBySkew(t *testing.T) {
	clk := newClock()
	var calls atomic.Int32
	c := New(func(_ context.Context) (string, time.Time, error) {
		n := calls.Add(1)
		return map[int32]string{1: "a", 2: "b"}[n], clk.now().Add(time.Minute), nil
	}, Options{Skew: 30 * time.Second, Now: clk.now})

	if tok, _ := c.Get(context.Background()); tok != "a" {
		t.Fatalf("tok=%q", tok)
	}
	clk.add(29 * time.Second) // còn 31s > skew
	if tok, _ := c.Get(context.Background()); tok != "a" || calls.Load() != 1 {
		t.Fatalf("chưa tới skew mà fetch lại: tok=%q calls=%d", tok, calls.Load())
	}
	clk.add(time.Second) // còn đúng 30s = skew: coi là sắp hết hạn
	if tok, _ := c.Get(context.Background()); tok != "b" || calls.Load() != 2 {
		t.Fatalf("tới skew phải làm mới: tok=%q calls=%d", tok, calls.Load())
	}
}

func TestGet_DefaultSkewIs30s(t *testing.T) {
	clk := newClock()
	var calls atomic.Int32
	c := New(func(_ context.Context) (string, time.Time, error) {
		calls.Add(1)
		return "t", clk.now().Add(time.Minute), nil
	}, Options{Now: clk.now})
	mustGet(t, c)
	clk.add(31 * time.Second)
	mustGet(t, c)
	if calls.Load() != 2 {
		t.Fatalf("calls=%d, muốn 2", calls.Load())
	}
}

func TestGet_ErrorNotCached(t *testing.T) {
	clk := newClock()
	boom := errors.New("boom")
	var calls atomic.Int32
	c := New(func(_ context.Context) (string, time.Time, error) {
		if calls.Add(1) == 1 {
			return "", time.Time{}, boom
		}
		return "ok", clk.now().Add(time.Hour), nil
	}, Options{Now: clk.now})
	if _, err := c.Get(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("err=%v", err)
	}
	if tok, err := c.Get(context.Background()); err != nil || tok != "ok" {
		t.Fatalf("lần sau phải thử lại: %q %v", tok, err)
	}
}

func TestGet_CallerCancelDoesNotCancelFetch(t *testing.T) {
	clk := newClock()
	started := make(chan struct{})
	release := make(chan struct{})
	var fetchCtxErr atomic.Value
	var calls atomic.Int32
	c := New(func(ctx context.Context) (string, time.Time, error) {
		calls.Add(1)
		close(started)
		<-release
		fetchCtxErr.Store(errBox{ctx.Err()})
		return "tok", clk.now().Add(time.Hour), nil
	}, Options{Now: clk.now})

	ctx1, cancel1 := context.WithCancel(context.Background())
	done1 := make(chan error, 1)
	go func() { _, err := c.Get(ctx1); done1 <- err }()
	<-started

	// Người chờ thứ hai cùng gom vào lần fetch đang chạy.
	done2 := make(chan string, 1)
	go func() { tok, _ := c.Get(context.Background()); done2 <- tok }()

	cancel1()
	if err := <-done1; !errors.Is(err, context.Canceled) {
		t.Fatalf("người gọi 1 phải nhận ctx.Err, got %v", err)
	}
	close(release)
	if tok := <-done2; tok != "tok" {
		t.Fatalf("người gọi 2 tok=%q", tok)
	}
	if e := fetchCtxErr.Load().(errBox).err; e != nil {
		t.Fatalf("ctx của fetch bị huỷ: %v", e)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls=%d", calls.Load())
	}
	// Token đã được lưu cho cả người đã bỏ đi.
	if tok, _ := c.Get(context.Background()); tok != "tok" || calls.Load() != 1 {
		t.Fatal("token phải nằm trong cache")
	}
}

type errBox struct{ err error }

func TestGet_FetchTimeoutBoundsFetch(t *testing.T) {
	clk := newClock()
	c := New(func(ctx context.Context) (string, time.Time, error) {
		<-ctx.Done()
		return "", time.Time{}, ctx.Err()
	}, Options{FetchTimeout: 10 * time.Millisecond, Now: clk.now})
	if _, err := c.Get(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v", err)
	}
}

func TestGet_FetchPanicDoesNotWedgeCache(t *testing.T) {
	clk := newClock()
	var calls atomic.Int32
	c := New(func(_ context.Context) (string, time.Time, error) {
		if calls.Add(1) == 1 {
			panic("kaboom")
		}
		return "ok", clk.now().Add(time.Hour), nil
	}, Options{Now: clk.now})
	if _, err := c.Get(context.Background()); err == nil {
		t.Fatal("panic phải thành lỗi cho người chờ")
	}
	if tok, err := c.Get(context.Background()); err != nil || tok != "ok" {
		t.Fatalf("lần sau phải thử lại: %q %v", tok, err)
	}
}

func TestInvalidate_OnlyMatchingToken(t *testing.T) {
	clk := newClock()
	var calls atomic.Int32
	c := New(func(_ context.Context) (string, time.Time, error) {
		n := calls.Add(1)
		return map[int32]string{1: "old", 2: "new", 3: "newer"}[n], clk.now().Add(time.Hour), nil
	}, Options{Now: clk.now})
	mustGet(t, c) // old
	c.Invalidate("other")
	if tok, _ := c.Get(context.Background()); tok != "old" || calls.Load() != 1 {
		t.Fatal("Invalidate sai token không được xoá")
	}
	c.Invalidate("old")
	if tok, _ := c.Get(context.Background()); tok != "new" {
		t.Fatalf("tok=%q", tok)
	}
	c.Invalidate("old") // lời gọi 401 cũ đến muộn
	if tok, _ := c.Get(context.Background()); tok != "new" || calls.Load() != 2 {
		t.Fatalf("token mới bị xoá nhầm: tok=%q calls=%d", tok, calls.Load())
	}
}

func TestGet_ZeroOrPastExpiryNotCached(t *testing.T) {
	clk := newClock()
	for name, exp := range map[string]time.Time{"zero": {}, "past": clk.now().Add(-time.Second)} {
		var calls atomic.Int32
		c := New(func(_ context.Context) (string, time.Time, error) {
			calls.Add(1)
			return "t", exp, nil
		}, Options{Now: clk.now})
		for i := 0; i < 2; i++ {
			if tok, err := c.Get(context.Background()); err != nil || tok != "t" {
				t.Fatalf("%s: %q %v", name, tok, err)
			}
		}
		if calls.Load() != 2 {
			t.Fatalf("%s: calls=%d, muốn 2 (không lưu)", name, calls.Load())
		}
	}
}

func TestGet_NoGoroutineLeak(t *testing.T) {
	clk := newClock()
	before := runtime.NumGoroutine()
	for i := 0; i < 20; i++ {
		c := New(func(_ context.Context) (string, time.Time, error) {
			return "t", clk.now().Add(time.Hour), nil
		}, Options{Now: clk.now})
		mustGet(t, c)
	}
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		runtime.Gosched()
		time.Sleep(time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > before {
		t.Fatalf("rò goroutine: %d -> %d", before, n)
	}
}

func mustGet(t *testing.T, c *Cache) {
	t.Helper()
	if _, err := c.Get(context.Background()); err != nil {
		t.Fatal(err)
	}
}
