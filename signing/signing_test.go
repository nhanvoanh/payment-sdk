package signing

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"
)

// Vector RFC 4231, test case 2.
func TestHMAC_RFC4231(t *testing.T) {
	key := []byte("Jefe")
	msg := []byte("what do ya want for nothing?")
	want256 := "5bdcc146bf60754e6a042426089575c75a003f089d2739839dec58b964ec3843"
	want512 := "164b7a7bfcf819e2e395fbe73b56e0a387bd64222e831fd610270cd7ea250554" +
		"9758bf75c05a994a6d034f65f8f0e6fdcaeab1a34d4a6b4b636e070a38bce737"
	if got := hex.EncodeToString(HMACSHA256(key, msg)); got != want256 {
		t.Fatalf("sha256 = %s", got)
	}
	if got := hex.EncodeToString(HMACSHA512(key, msg)); got != want512 {
		t.Fatalf("sha512 = %s", got)
	}
}

func TestEqual(t *testing.T) {
	mac := HMACSHA256([]byte("k"), []byte("m"))
	if !Equal(mac, append([]byte(nil), mac...)) {
		t.Fatal("bằng nhau mà false")
	}
	if Equal(mac, mac[:len(mac)-1]) || Equal(mac, nil) || Equal(nil, mac) || Equal(nil, nil) || Equal([]byte{}, nil) {
		t.Fatal("Equal sai ở biên")
	}
	if !EqualHex(mac, hex.EncodeToString(mac)) {
		t.Fatal("hex đúng mà false")
	}
	if !EqualBase64(mac, base64.StdEncoding.EncodeToString(mac)) {
		t.Fatal("base64 đúng mà false")
	}
}

func TestEqual_NeverPanicsOnArbitraryInput(t *testing.T) {
	mac := []byte{1, 2, 3}
	for _, s := range []string{"", "zz", "0", "%%%", "\x00\xff", "AAAA====", "é"} {
		if EqualHex(mac, s) || EqualBase64(mac, s) {
			t.Fatalf("%q không được khớp", s)
		}
	}
	if EqualHex(nil, "") || EqualBase64(nil, "") {
		t.Fatal("mac rỗng không được coi là khớp chuỗi rỗng")
	}
}

func genKey(t *testing.T, bits int) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestRSA_RoundTripAndTamper(t *testing.T) {
	k := genKey(t, 2048)
	msg := []byte("acme|order-1|1000")
	sig, err := RSASignSHA256(k, msg)
	if err != nil {
		t.Fatal(err)
	}
	if !RSAVerifySHA256(&k.PublicKey, msg, sig) {
		t.Fatal("chữ ký đúng bị từ chối")
	}
	bad := append([]byte(nil), sig...)
	bad[0] ^= 1
	if RSAVerifySHA256(&k.PublicKey, msg, bad) {
		t.Fatal("chữ ký sửa một bit vẫn qua")
	}
	if RSAVerifySHA256(&k.PublicKey, append(msg, '!'), sig) {
		t.Fatal("thông điệp đổi vẫn qua")
	}
	if RSAVerifySHA256(nil, msg, sig) || RSAVerifySHA256(&k.PublicKey, msg, nil) {
		t.Fatal("đầu vào rỗng phải false")
	}
	if _, err := RSASignSHA256(nil, msg); err == nil {
		t.Fatal("khoá nil phải lỗi")
	}
}

func pemOf(typ string, der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der})
}

func TestParseRSAPrivateKeyPEM(t *testing.T) {
	k := genKey(t, 2048)
	pkcs8, _ := x509.MarshalPKCS8PrivateKey(k)
	for name, in := range map[string][]byte{
		"pkcs1": pemOf("RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(k)),
		"pkcs8": pemOf("PRIVATE KEY", pkcs8),
	} {
		got, err := ParseRSAPrivateKeyPEM(in)
		if err != nil || !got.Equal(k) {
			t.Fatalf("%s: err=%v", name, err)
		}
	}
	weak := genKey(t, 1024)
	if _, err := ParseRSAPrivateKeyPEM(pemOf("RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(weak))); err == nil {
		t.Fatal("khoá 1024 bit phải bị từ chối")
	}
	for _, in := range [][]byte{nil, []byte("không phải pem"), pemOf("PRIVATE KEY", []byte("rác"))} {
		if _, err := ParseRSAPrivateKeyPEM(in); err == nil {
			t.Fatalf("%q phải lỗi", in)
		}
	}
}

func TestParseRSAPublicKeyPEM(t *testing.T) {
	k := genKey(t, 2048)
	pkix, _ := x509.MarshalPKIXPublicKey(&k.PublicKey)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix0(), NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour)}
	cert, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	for name, in := range map[string][]byte{
		"pkix": pemOf("PUBLIC KEY", pkix),
		"cert": pemOf("CERTIFICATE", cert),
	} {
		got, err := ParseRSAPublicKeyPEM(in)
		if err != nil || !got.Equal(&k.PublicKey) {
			t.Fatalf("%s: err=%v", name, err)
		}
	}
	weak := genKey(t, 1024)
	wpkix, _ := x509.MarshalPKIXPublicKey(&weak.PublicKey)
	if _, err := ParseRSAPublicKeyPEM(pemOf("PUBLIC KEY", wpkix)); err == nil {
		t.Fatal("khoá công khai 1024 bit phải bị từ chối")
	}
	if _, err := ParseRSAPublicKeyPEM([]byte("x")); err == nil {
		t.Fatal("rác phải lỗi")
	}
}

func pkix0() pkix.Name { return pkix.Name{CommonName: "acme"} }

func TestRSA_RejectsWeakHandBuiltKeys(t *testing.T) {
	weak := genKey(t, 1024)
	if _, err := RSASignSHA256(weak, []byte("m")); err == nil {
		t.Fatal("ký bằng khoá 1024 bit phải lỗi")
	}
	strong := genKey(t, 2048)
	sig, _ := RSASignSHA256(strong, []byte("m"))
	if RSAVerifySHA256(&weak.PublicKey, []byte("m"), sig) {
		t.Fatal("kiểm bằng khoá 1024 bit phải false")
	}
}

func TestParseRSAPrivateKeyPEM_ReportsBothErrors(t *testing.T) {
	_, err := ParseRSAPrivateKeyPEM(pemOf("PRIVATE KEY", []byte("rác")))
	if err == nil {
		t.Fatal("phải lỗi")
	}
	msg := err.Error()
	if !strings.Contains(msg, "PKCS") && !strings.Contains(msg, "x509") && !strings.Contains(msg, "asn1") {
		t.Fatalf("lỗi không nêu nguyên nhân: %v", err)
	}
	if n := strings.Count(msg, "\n"); n < 1 {
		t.Fatalf("muốn cả hai lỗi PKCS#1 và PKCS#8, got %q", msg)
	}
}
