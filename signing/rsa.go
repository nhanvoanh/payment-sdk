package signing

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
)

// minRSABits: khoá ngắn hơn bị từ chối ngay khi phân tích, không để lọt vào đường ký.
const minRSABits = 2048

var errNoPEM = errors.New("signing: không có khối PEM")

// ParseRSAPrivateKeyPEM đọc khoá riêng RSA dạng PKCS#1 hoặc PKCS#8; từ chối khoá dưới 2048 bit.
func ParseRSAPrivateKeyPEM(data []byte) (*rsa.PrivateKey, error) {
	blk, _ := pem.Decode(data)
	if blk == nil {
		return nil, errNoPEM
	}
	key, err := x509.ParsePKCS1PrivateKey(blk.Bytes)
	if err != nil {
		k8, err8 := x509.ParsePKCS8PrivateKey(blk.Bytes)
		if err8 != nil {
			return nil, fmt.Errorf("signing: khoá riêng không đọc được: %w", errors.Join(err, err8))
		}
		var ok bool
		if key, ok = k8.(*rsa.PrivateKey); !ok {
			return nil, errors.New("signing: khoá riêng không phải RSA")
		}
	}
	if key.N.BitLen() < minRSABits {
		return nil, fmt.Errorf("signing: khoá RSA %d bit, tối thiểu %d", key.N.BitLen(), minRSABits)
	}
	return key, nil
}

// ParseRSAPublicKeyPEM đọc khoá công khai RSA dạng PKIX hoặc chứng chỉ X.509; từ chối khoá dưới 2048 bit.
func ParseRSAPublicKeyPEM(data []byte) (*rsa.PublicKey, error) {
	blk, _ := pem.Decode(data)
	if blk == nil {
		return nil, errNoPEM
	}
	var pub any
	if cert, err := x509.ParseCertificate(blk.Bytes); err == nil {
		pub = cert.PublicKey
	} else {
		k, err := x509.ParsePKIXPublicKey(blk.Bytes)
		if err != nil {
			return nil, fmt.Errorf("signing: khoá công khai không đọc được: %w", err)
		}
		pub = k
	}
	key, ok := pub.(*rsa.PublicKey)
	if !ok {
		return nil, errors.New("signing: khoá công khai không phải RSA")
	}
	if key.N.BitLen() < minRSABits {
		return nil, fmt.Errorf("signing: khoá RSA %d bit, tối thiểu %d", key.N.BitLen(), minRSABits)
	}
	return key, nil
}

// RSASignSHA256 ký SHA-256 của msg theo PKCS#1 v1.5.
func RSASignSHA256(key *rsa.PrivateKey, msg []byte) ([]byte, error) {
	if key == nil || key.N == nil {
		return nil, errors.New("signing: khoá riêng nil")
	}
	if key.N.BitLen() < minRSABits { // khoá dựng tay cũng không được lọt qua đường ký
		return nil, fmt.Errorf("signing: khoá RSA %d bit, tối thiểu %d", key.N.BitLen(), minRSABits)
	}
	sum := sha256.Sum256(msg)
	return rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
}

// RSAVerifySHA256 kiểm chữ ký PKCS#1 v1.5 trên SHA-256 của msg; mọi đầu vào xấu và khoá dưới 2048 bit đều cho false.
func RSAVerifySHA256(key *rsa.PublicKey, msg, sig []byte) bool {
	if key == nil || key.N == nil || key.N.BitLen() < minRSABits {
		return false
	}
	sum := sha256.Sum256(msg)
	return rsa.VerifyPKCS1v15(key, crypto.SHA256, sum[:], sig) == nil
}
