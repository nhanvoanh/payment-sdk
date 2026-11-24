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

// RSAEncryptPKCS1v15 mã hoá msg bằng khoá công khai pub theo RSAES-PKCS1-v1_5 (đệm ngẫu nhiên: mỗi lần một bản mã
// khác). Trả bản mã thô; mã hoá sang dạng đối tác muốn (base64, hex...) là việc của connector. Dùng khi đối tác bắt
// buộc kiểu này (ví dụ đòi mã thanh toán khách đưa ra để quét gửi dưới dạng đã mã hoá); không dùng cho giao thức mới.
// Khoá dưới 2048 bit bị từ chối: đó là chính sách cố ý của gói, như ở đường ký. msg rỗng hay dài quá
// (khoá k byte nhận tối đa k-11 byte) là lỗi; câu lỗi không bao giờ chứa msg.
func RSAEncryptPKCS1v15(pub *rsa.PublicKey, msg []byte) ([]byte, error) {
	if pub == nil || pub.N == nil {
		return nil, errors.New("signing: khoá công khai nil")
	}
	if pub.N.BitLen() < minRSABits { // khoá dựng tay cũng không được lọt qua đường mã hoá
		return nil, fmt.Errorf("signing: khoá RSA %d bit, tối thiểu %d", pub.N.BitLen(), minRSABits)
	}
	if len(msg) == 0 {
		return nil, errors.New("signing: thông điệp cần mã hoá rỗng")
	}
	if limit := pub.Size() - 11; len(msg) > limit {
		return nil, fmt.Errorf("signing: thông điệp %d byte, tối đa %d byte với khoá %d bit", len(msg), limit, pub.N.BitLen())
	}
	ct, err := rsa.EncryptPKCS1v15(rand.Reader, pub, msg)
	if err != nil {
		return nil, fmt.Errorf("signing: mã hoá RSA lỗi: %w", err) // lỗi của crypto/rsa không chứa msg
	}
	return ct, nil
}
