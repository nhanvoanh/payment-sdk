package connector

import (
	"fmt"
	"regexp"
	"sort"
	"sync"

	"github.com/nhanvoanh/payment-sdk/paykit"
)

// Registry giữ các connector theo tên; an toàn cho đọc đồng thời sau khi đăng ký.
type Registry struct {
	mu       sync.RWMutex
	byName   map[string]Connector
	order    []string
	defaults map[paykit.Method]string
}

// Tên connector: chữ thường, số, gạch dưới, tối đa 32 ký tự; cùng định dạng với nhãn log/metric.
var namePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

func NewRegistry() *Registry {
	return &Registry{byName: map[string]Connector{}, defaults: map[paykit.Method]string{}}
}

func (r *Registry) Register(c Connector) error {
	if c == nil {
		return fmt.Errorf("connector: connector nil")
	}
	name := c.Name()
	if !namePattern.MatchString(name) {
		return fmt.Errorf("connector: tên %q không hợp lệ (cần %s)", name, namePattern)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.byName[name]; dup {
		return fmt.Errorf("connector: %q đã đăng ký", name)
	}
	r.byName[name] = c
	r.order = append(r.order, name)
	return nil
}

func (r *Registry) Get(name string) (Connector, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	c, ok := r.byName[name]
	return c, ok
}

// ForMethod: method có mặc định thì chỉ trả đúng connector đó khi nó đã đăng ký và khai method;
// không bao giờ rơi sang connector khác (mặc định có thể đặt trước cho connector từ xa đăng ký muộn).
// Không có mặc định thì chỉ trả khi đúng MỘT connector đã đăng ký khai method; hơn một thì không đoán
// (thứ tự đăng ký của connector từ xa là ngẫu nhiên, lệnh tiền không được đi theo nó).
func (r *Registry) ForMethod(m paykit.Method) (Connector, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if name, ok := r.defaults[m]; ok {
		c, registered := r.byName[name]
		if registered && supportsMethod(c, m) {
			return c, true
		}
		return nil, false
	}
	var found Connector
	for _, name := range r.order {
		if supportsMethod(r.byName[name], m) {
			if found != nil {
				return nil, false
			}
			found = r.byName[name]
		}
	}
	return found, found != nil
}

// SetDefault không đòi connector đã đăng ký: connector từ xa có thể đăng ký sau khi cấu hình đã đặt mặc định.
func (r *Registry) SetDefault(m paykit.Method, name string) error {
	if name == "" {
		return fmt.Errorf("connector: tên mặc định rỗng")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.defaults[m] = name
	return nil
}

// DefaultFor trả tên connector mặc định của method (đã đặt bằng SetDefault), kể cả khi nó chưa đăng ký: core
// dùng để phân biệt "đối tác có trong cấu hình mà chưa lên" (tạm thời) với "không hỗ trợ".
func (r *Registry) DefaultFor(m paykit.Method) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	name, ok := r.defaults[m]
	return name, ok
}

func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, len(r.order))
	copy(out, r.order)
	sort.Strings(out)
	return out
}

func supportsMethod(c Connector, m paykit.Method) bool {
	for _, x := range c.Methods() {
		if x == m {
			return true
		}
	}
	return false
}
