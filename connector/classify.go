package connector

import "errors"

// Outcome là cách core hiểu kết quả một lời gọi connector. Đây là nguồn duy nhất của phân loại:
// core (paymentsvc), bộ conformance và proxy gRPC đều theo nó.
type Outcome int

// Tên có tiền tố Call vì OutcomeRejected đã là một WebhookOutcome.
const (
	CallOK          Outcome = iota // err nil
	CallUnknown                    // không rõ đối tác đã xử lý chưa: không bao giờ chốt thất bại
	CallRejected                   // đối tác nói rõ là từ chối
	CallUnavailable                // chắc chắn chưa xử lý (ErrProviderUnavailable) hoặc ErrNotSupported
)

// Classify quy lỗi về Outcome. ErrUnknownOutcome xét trước: lỗi bọc cả nó và một sentinel khác vẫn là
// chưa rõ. Rồi ErrProviderRejected, rồi ErrProviderUnavailable/ErrNotSupported. Mọi lỗi khác (timeout, huỷ,
// giải mã sau khi đối tác đã nhận) là chưa rõ: thất bại mời client thử lại và trả tiền hai lần.
func Classify(err error) Outcome {
	switch {
	case err == nil:
		return CallOK
	case errors.Is(err, ErrUnknownOutcome):
		return CallUnknown
	case errors.Is(err, ErrProviderRejected):
		return CallRejected
	case errors.Is(err, ErrProviderUnavailable), errors.Is(err, ErrNotSupported):
		return CallUnavailable
	}
	return CallUnknown
}
