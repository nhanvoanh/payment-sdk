package connector

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"bitbucket.org/hasaki-tech/payment-connector-sdk/paykit"
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
