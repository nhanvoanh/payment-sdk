// Package tlstest sinh CA và chứng chỉ ECDSA tạm cho test mTLS. Chỉ dùng trong test.
package tlstest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nhanvoanh/payment-sdk/connectorrpc"
)

// Bundle là một CA cùng thư mục tạm chứa các chứng chỉ nó đã cấp.
type Bundle struct {
	t      testing.TB
	dir    string
	ca     *x509.Certificate
	caKey  *ecdsa.PrivateKey
	caFile string
	n      int
}

// New sinh CA mới trong thư mục tạm của t.
func New(t testing.TB) *Bundle {
	t.Helper()
	key := newKey(t)
	tmpl := &x509.Certificate{
		SerialNumber:          serial(t),
		Subject:               pkix.Name{CommonName: "tlstest CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("tlstest: %v", err)
	}
	ca, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("tlstest: %v", err)
	}
	b := &Bundle{t: t, dir: t.TempDir(), ca: ca, caKey: key}
	b.caFile = filepath.Join(b.dir, "ca.pem")
	write(t, b.caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	return b
}

// Server cấp chứng chỉ có SAN DNS dnsName; CAFile là CA của bundle (để server kiểm client).
func (b *Bundle) Server(dnsName string) connectorrpc.TLSFiles { return b.issue(dnsName) }

// Client cấp chứng chỉ client có SAN DNS dnsName; CAFile là CA của bundle (để client kiểm server).
func (b *Bundle) Client(dnsName string) connectorrpc.TLSFiles { return b.issue(dnsName) }

// OtherCA trả bundle với CA hoàn toàn khác, để dựng đối tượng không được tin.
func (b *Bundle) OtherCA() *Bundle { return New(b.t) }

func (b *Bundle) issue(dnsName string) connectorrpc.TLSFiles {
	b.t.Helper()
	b.n++
	key := newKey(b.t)
	tmpl := &x509.Certificate{
		SerialNumber: serial(b.t),
		Subject:      pkix.Name{CommonName: dnsName},
		DNSNames:     []string{dnsName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, b.ca, &key.PublicKey, b.caKey)
	if err != nil {
		b.t.Fatalf("tlstest: %v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		b.t.Fatalf("tlstest: %v", err)
	}
	dir := filepath.Join(b.dir, fmt.Sprintf("leaf%d", b.n))
	if err := os.Mkdir(dir, 0o700); err != nil {
		b.t.Fatalf("tlstest: %v", err)
	}
	f := connectorrpc.TLSFiles{CertFile: filepath.Join(dir, "cert.pem"), KeyFile: filepath.Join(dir, "key.pem"), CAFile: b.caFile}
	write(b.t, f.CertFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	write(b.t, f.KeyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
	return f
}

func newKey(t testing.TB) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("tlstest: %v", err)
	}
	return k
}

func serial(t testing.TB) *big.Int {
	t.Helper()
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		t.Fatalf("tlstest: %v", err)
	}
	return n
}

func write(t testing.TB, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("tlstest: %v", err)
	}
}
