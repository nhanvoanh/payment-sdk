// Package signing gom các phép ký và kiểm chữ ký dùng chung cho connector.
package signing

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
)

// HMACSHA256 trả HMAC-SHA256 của msg dưới khoá key.
func HMACSHA256(key, msg []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(msg)
	return m.Sum(nil)
}

// HMACSHA512 trả HMAC-SHA512 của msg dưới khoá key.
func HMACSHA512(key, msg []byte) []byte {
	m := hmac.New(sha512.New, key)
	m.Write(msg)
	return m.Sum(nil)
}

// Equal so sánh hai MAC trong thời gian hằng (độ dài khác nhau thì false ngay).
// Một trong hai rỗng thì false: hai chữ ký rỗng không được coi là khớp.
func Equal(a, b []byte) bool {
	return len(a) > 0 && len(b) > 0 && subtle.ConstantTimeCompare(a, b) == 1
}

// EqualHex so mac với chữ ký dạng hex. Chuỗi không giải mã được, hoặc mac rỗng, thì false.
func EqualHex(mac []byte, hexSig string) bool {
	sig, err := hex.DecodeString(hexSig)
	return err == nil && Equal(mac, sig)
}

// EqualBase64 so mac với chữ ký base64 chuẩn. Chuỗi không giải mã được, hoặc mac rỗng, thì false.
func EqualBase64(mac []byte, b64Sig string) bool {
	sig, err := base64.StdEncoding.DecodeString(b64Sig)
	return err == nil && Equal(mac, sig)
}
