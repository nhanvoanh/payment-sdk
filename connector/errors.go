package connector

import "errors"

var (
	ErrInvalidSignature = errors.New("connector: chữ ký không hợp lệ")
	// ErrNotSupported: đối tác không có thao tác này (dù connector cài interface tương ứng).
	ErrNotSupported = errors.New("connector: đối tác không hỗ trợ thao tác này")
	// ErrProviderUnavailable nghĩa là CHẮC CHẮN đối tác chưa xử lý (ví dụ không mở được kết nối): core
	// sẽ chốt giao dịch failed và client được tạo lại ngay. Không bao giờ trả lỗi này cho timeout hay
	// phản hồi đọc không được — khi đó đối tác có thể đã trừ tiền, hãy trả ErrUnknownOutcome.
	ErrProviderUnavailable = errors.New("connector: đối tác không phản hồi được, chắc chắn chưa xử lý")
	// ErrProviderRejected: đối tác trả lời rõ ràng là từ chối; nên dùng RejectedError để kèm mã.
	ErrProviderRejected = errors.New("connector: đối tác từ chối")
	// ErrUnknownOutcome: không biết đối tác đã xử lý hay chưa; giao dịch giữ processing để truy vấn
	// sau, không bao giờ thành failed. Mọi lỗi không thuộc các sentinel ở đây cũng được core hiểu như vậy.
	ErrUnknownOutcome   = errors.New("connector: không rõ kết quả phía đối tác")
	ErrEventNotRelevant = errors.New("connector: sự kiện không liên quan")
)

// RejectedError là lời từ chối có mã của đối tác; errors.Is với ErrProviderRejected luôn đúng.
type RejectedError struct{ Code, Message string }

func (e *RejectedError) Error() string {
	return "connector: đối tác từ chối [" + e.Code + "]: " + e.Message
}

func (e *RejectedError) Unwrap() error { return ErrProviderRejected }
