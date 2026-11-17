package providerhttp

import "net/http"

// NewWithTransport tiêm transport cho test (bản exported không cho phép).
func NewWithTransport(cfg Config, rt http.RoundTripper) (*Client, error) { return newClient(cfg, rt) }
