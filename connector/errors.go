package connector

import "errors"

var (
	ErrInvalidSignature    = errors.New("connector: chữ ký không hợp lệ")
	ErrNotSupported        = errors.New("connector: đối tác không hỗ trợ thao tác này")
	ErrProviderUnavailable = errors.New("connector: đối tác không phản hồi được, chắc chắn chưa xử lý")
	ErrProviderRejected    = errors.New("connector: đối tác từ chối")
	// ErrUnknownOutcome: không biết đối tác đã xử lý hay chưa; không bao giờ được coi là thất bại.
	ErrUnknownOutcome   = errors.New("connector: không rõ kết quả phía đối tác")
	ErrEventNotRelevant = errors.New("connector: sự kiện không liên quan")
)

// RejectedError là lời từ chối có mã của đối tác; errors.Is với ErrProviderRejected luôn đúng.
type RejectedError struct{ Code, Message string }

func (e *RejectedError) Error() string {
	return "connector: đối tác từ chối [" + e.Code + "]: " + e.Message
}

func (e *RejectedError) Unwrap() error { return ErrProviderRejected }
