package connector

import (
	"fmt"
	"sort"
	"sync"

	"bitbucket.org/hasaki-tech/payment-connector-sdk/paykit"
)

// Registry giữ các connector theo tên; an toàn cho đọc đồng thời sau khi đăng ký.
type Registry struct {
	mu       sync.RWMutex
	byName   map[string]Connector
	order    []string
	defaults map[paykit.Method]string
}

func NewRegistry() *Registry {
	return &Registry{byName: map[string]Connector{}, defaults: map[paykit.Method]string{}}
}

func (r *Registry) Register(c Connector) error {
	name := c.Name()
	if name == "" {
		return fmt.Errorf("connector: tên rỗng")
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

// ForMethod trả connector mặc định của method nếu có, không thì connector đăng ký đầu tiên hỗ trợ nó.
func (r *Registry) ForMethod(m paykit.Method) (Connector, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if name, ok := r.defaults[m]; ok {
		return r.byName[name], true
	}
	for _, name := range r.order {
		if supportsMethod(r.byName[name], m) {
			return r.byName[name], true
		}
	}
	return nil, false
}

func (r *Registry) SetDefault(m paykit.Method, name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.byName[name]
	if !ok {
		return fmt.Errorf("connector: %q chưa đăng ký", name)
	}
	if !supportsMethod(c, m) {
		return fmt.Errorf("connector: %q không hỗ trợ method %q", name, m)
	}
	r.defaults[m] = name
	return nil
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
