package connectorrpc

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
)

// certCheckInterval là khoảng tối thiểu giữa hai lần kiểm mtime của cặp chứng chỉ.
const certCheckInterval = 30 * time.Second

// TLSFiles là đường dẫn cặp chứng chỉ của chính mình và CA dùng để tin đối phương (định dạng PEM).
type TLSFiles struct{ CertFile, KeyFile, CAFile string }

// ServerTLS dựng cấu hình TLS 1.3 cho server, bắt buộc và kiểm chứng chỉ client theo CAFile.
// Cặp chứng chỉ được đọc lại khi file đổi (kiểm không dày hơn 30 giây); đọc lại hỏng thì giữ cặp cũ.
func ServerTLS(f TLSFiles) (*tls.Config, error) {
	return serverTLS(f, certCheckInterval, time.Now)
}

// ClientTLS dựng cấu hình TLS 1.3 cho client: kiểm chứng chỉ server theo CAFile và serverName,
// xuất trình chứng chỉ client; quy tắc đọc lại giống ServerTLS.
func ClientTLS(f TLSFiles, serverName string) (*tls.Config, error) {
	return clientTLS(f, serverName, certCheckInterval, time.Now)
}

func serverTLS(f TLSFiles, interval time.Duration, now func() time.Time) (*tls.Config, error) {
	r, pool, err := load(f, interval, now)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion:     tls.VersionTLS13,
		ClientAuth:     tls.RequireAndVerifyClientCert,
		ClientCAs:      pool,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return r.get(), nil },
	}, nil
}

func clientTLS(f TLSFiles, serverName string, interval time.Duration, now func() time.Time) (*tls.Config, error) {
	if serverName == "" {
		return nil, errors.New("connectorrpc: thiếu serverName để kiểm chứng chỉ server")
	}
	r, pool, err := load(f, interval, now)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion:           tls.VersionTLS13,
		RootCAs:              pool,
		ServerName:           serverName,
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return r.get(), nil },
	}, nil
}

func load(f TLSFiles, interval time.Duration, now func() time.Time) (*certReloader, *x509.CertPool, error) {
	pem, err := os.ReadFile(f.CAFile)
	if err != nil {
		return nil, nil, fmt.Errorf("connectorrpc: đọc CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, nil, fmt.Errorf("connectorrpc: %s không chứa chứng chỉ CA hợp lệ", f.CAFile)
	}
	r, err := newCertReloader(f, interval, now)
	if err != nil {
		return nil, nil, err
	}
	return r, pool, nil
}

type fileStamp struct {
	certMod, keyMod   time.Time
	certSize, keySize int64
}

// certReloader giữ cặp chứng chỉ hiện dùng. Bất biến: cert luôn là một cặp đã nạp thành công;
// đọc lại thất bại (file đang ghi dở, rác) không bao giờ thay nó, và sẽ thử lại ở lần kiểm sau.
type certReloader struct {
	files    TLSFiles
	interval time.Duration
	now      func() time.Time

	mu      sync.Mutex
	cert    *tls.Certificate
	stamp   fileStamp
	checked time.Time
}

func newCertReloader(f TLSFiles, interval time.Duration, now func() time.Time) (*certReloader, error) {
	r := &certReloader{files: f, interval: interval, now: now}
	st, err := r.stat()
	if err != nil {
		return nil, fmt.Errorf("connectorrpc: đọc chứng chỉ: %w", err)
	}
	c, err := tls.LoadX509KeyPair(f.CertFile, f.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("connectorrpc: nạp cặp chứng chỉ: %w", err)
	}
	r.cert, r.stamp, r.checked = &c, st, now()
	return r, nil
}

func (r *certReloader) stat() (fileStamp, error) {
	c, err := os.Stat(r.files.CertFile)
	if err != nil {
		return fileStamp{}, err
	}
	k, err := os.Stat(r.files.KeyFile)
	if err != nil {
		return fileStamp{}, err
	}
	return fileStamp{c.ModTime(), k.ModTime(), c.Size(), k.Size()}, nil
}

func (r *certReloader) get() *tls.Certificate {
	r.mu.Lock()
	defer r.mu.Unlock()
	if now := r.now(); now.Sub(r.checked) >= r.interval {
		r.checked = now
		if st, err := r.stat(); err == nil && st != r.stamp {
			if c, err := tls.LoadX509KeyPair(r.files.CertFile, r.files.KeyFile); err == nil {
				r.cert, r.stamp = &c, st
			}
		}
	}
	return r.cert
}
