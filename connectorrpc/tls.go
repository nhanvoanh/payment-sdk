package connectorrpc

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"
)

// certCheckInterval là khoảng tối thiểu giữa hai lần kiểm mtime của cặp chứng chỉ.
const certCheckInterval = 30 * time.Second

// TLSFiles là đường dẫn cặp chứng chỉ của chính mình và CA dùng để tin đối phương (định dạng PEM).
type TLSFiles struct{ CertFile, KeyFile, CAFile string }

// ServerTLS dựng cấu hình TLS 1.3 cho server, bắt buộc và kiểm chứng chỉ client theo CAFile.
// Cặp chứng chỉ được đọc lại khi file đổi (kiểm không dày hơn 30 giây); đọc lại hỏng thì giữ cặp cũ
// và ghi một dòng Warn qua slog.Default(). CA đọc một lần; đổi CA cần khởi động lại.
func ServerTLS(f TLSFiles) (*tls.Config, error) {
	return serverTLS(f, certCheckInterval, time.Now, nil)
}

// ClientTLS dựng cấu hình TLS 1.3 cho client: kiểm chứng chỉ server theo CAFile và serverName,
// xuất trình chứng chỉ client; quy tắc đọc lại giống ServerTLS. CA đọc một lần; đổi CA cần khởi động lại.
func ClientTLS(f TLSFiles, serverName string) (*tls.Config, error) {
	return clientTLS(f, serverName, certCheckInterval, time.Now, nil)
}

func serverTLS(f TLSFiles, interval time.Duration, now func() time.Time, log *slog.Logger) (*tls.Config, error) {
	r, pool, err := load(f, interval, now, log)
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

func clientTLS(f TLSFiles, serverName string, interval time.Duration, now func() time.Time, log *slog.Logger) (*tls.Config, error) {
	if serverName == "" {
		return nil, errors.New("connectorrpc: thiếu serverName để kiểm chứng chỉ server")
	}
	r, pool, err := load(f, interval, now, log)
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

func load(f TLSFiles, interval time.Duration, now func() time.Time, log *slog.Logger) (*certReloader, *x509.CertPool, error) {
	pem, err := os.ReadFile(f.CAFile)
	if err != nil {
		return nil, nil, fmt.Errorf("connectorrpc: đọc CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, nil, fmt.Errorf("connectorrpc: %s không chứa chứng chỉ CA hợp lệ", f.CAFile)
	}
	r, err := newCertReloader(f, interval, now, log)
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
	log      *slog.Logger // nil: slog.Default() tại lúc ghi

	mu      sync.Mutex
	cert    *tls.Certificate
	stamp   fileStamp
	checked time.Time
	warned  string // khoá của trạng thái hỏng đã báo, để không lặp lại mỗi lần kiểm
}

func newCertReloader(f TLSFiles, interval time.Duration, now func() time.Time, log *slog.Logger) (*certReloader, error) {
	r := &certReloader{files: f, interval: interval, now: now, log: log}
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
		r.refresh()
	}
	return r.cert
}

// refresh thử nạp lại khi file đổi. Hỏng thì giữ cặp cũ và báo Warn một lần cho mỗi trạng thái file hỏng.
func (r *certReloader) refresh() {
	st, err := r.stat()
	if err != nil {
		r.warn("stat:"+err.Error(), err)
		return
	}
	if st == r.stamp {
		return
	}
	c, err := tls.LoadX509KeyPair(r.files.CertFile, r.files.KeyFile)
	if err != nil {
		r.warn(fmt.Sprintf("load:%v", st), err)
		return
	}
	r.cert, r.stamp, r.warned = &c, st, ""
}

func (r *certReloader) warn(key string, err error) {
	if r.warned == key {
		return
	}
	r.warned = key
	log := r.log
	if log == nil {
		log = slog.Default()
	}
	// Chỉ ghi đường dẫn và lỗi, không bao giờ nội dung khoá hay chứng chỉ.
	log.Warn("đọc lại chứng chỉ thất bại, giữ cặp cũ", "cert", r.files.CertFile, "key", r.files.KeyFile, "error", err)
}
