package connector

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/nhanvoanh/payment-sdk/paykit"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRegister_NilAndBadName(t *testing.T) {
	r := NewRegistry()
	assert.Error(t, r.Register(nil))
	for _, bad := range []string{"", "A", "a-b", "1a", strings.Repeat("a", 33)} {
		assert.Error(t, r.Register(baseConn{name: bad}), "tên %q phải bị từ chối", bad)
	}
	require.NoError(t, r.Register(baseConn{name: "acme_2"}))
	require.NoError(t, r.Register(baseConn{name: strings.Repeat("a", 32)}))
}

func TestForMethod_ReservedDefaultDoesNotFallBack(t *testing.T) {
	r := NewRegistry()
	require.NoError(t, r.SetDefault(paykit.MethodQR, "later"))
	require.NoError(t, r.Register(baseConn{name: "other", methods: []paykit.Method{paykit.MethodQR}}))

	_, ok := r.ForMethod(paykit.MethodQR)
	assert.False(t, ok, "mặc định chưa đăng ký không được rơi sang connector khác")

	require.NoError(t, r.Register(baseConn{name: "later", methods: []paykit.Method{paykit.MethodQR}}))
	c, ok := r.ForMethod(paykit.MethodQR)
	require.True(t, ok)
	assert.Equal(t, "later", c.Name())

	// Mặc định trỏ tới connector không khai method thì không có kết quả.
	require.NoError(t, r.SetDefault(paykit.MethodVisa, "later"))
	_, ok = r.ForMethod(paykit.MethodVisa)
	assert.False(t, ok)
}

func TestRegistry_ConcurrentReadersAndLateRegister(t *testing.T) {
	r := NewRegistry()
	require.NoError(t, r.SetDefault(paykit.MethodQR, "late_0"))
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				r.Get("late_0")
				r.ForMethod(paykit.MethodQR)
				r.Names()
			}
		}()
	}
	for i := 0; i < 50; i++ {
		require.NoError(t, r.Register(baseConn{name: fmt.Sprintf("late_%d", i), methods: []paykit.Method{paykit.MethodQR}}))
	}
	close(stop)
	wg.Wait()
	assert.Len(t, r.Names(), 50)
}

// Không có mặc định: chọn theo thứ tự đăng ký là chọn theo connector nào Describe xong trước (ngẫu nhiên
// theo pod, theo lần khởi động), nên lệnh tiền sẽ đi sang đối tác tuỳ hứng. Hơn một ứng viên thì không trả gì.
func TestForMethod_AmbiguousWithoutDefaultReturnsNothing(t *testing.T) {
	for _, order := range [][]string{{"a", "b"}, {"b", "a"}} {
		r := NewRegistry()
		for _, n := range order {
			require.NoError(t, r.Register(baseConn{name: n, methods: []paykit.Method{paykit.MethodQR}}))
		}
		_, ok := r.ForMethod(paykit.MethodQR)
		assert.False(t, ok, "thứ tự %v", order)

		require.NoError(t, r.SetDefault(paykit.MethodQR, "b"))
		c, ok := r.ForMethod(paykit.MethodQR)
		require.True(t, ok)
		assert.Equal(t, "b", c.Name(), "mặc định tường minh gỡ mơ hồ")
	}

	r := NewRegistry()
	require.NoError(t, r.Register(baseConn{name: "a", methods: []paykit.Method{paykit.MethodQR}}))
	require.NoError(t, r.Register(baseConn{name: "b", methods: []paykit.Method{paykit.MethodVisa}}))
	c, ok := r.ForMethod(paykit.MethodQR)
	require.True(t, ok, "đúng một ứng viên thì trả nó")
	assert.Equal(t, "a", c.Name())
}

func TestRegistry_DefaultForEvenWhenUnregistered(t *testing.T) {
	r := NewRegistry()
	_, ok := r.DefaultFor(paykit.MethodQR)
	if ok {
		t.Fatal("chưa đặt mặc định")
	}
	if err := r.SetDefault(paykit.MethodQR, "acme"); err != nil {
		t.Fatal(err)
	}
	if name, ok := r.DefaultFor(paykit.MethodQR); !ok || name != "acme" {
		t.Fatalf("DefaultFor = %q, %v", name, ok)
	}
}
