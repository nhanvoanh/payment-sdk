package connectorrpc

import (
	"errors"
	"fmt"

	"bitbucket.org/hasaki-tech/payment-connector-sdk/connector"
	"bitbucket.org/hasaki-tech/payment-connector-sdk/connectorpb"
)

// ErrorToProto xếp lỗi của connector vào ConnectorError. Thứ tự xét giống paymentsvc.classify:
// ErrUnknownOutcome trước, rồi ErrProviderRejected, rồi ErrProviderUnavailable/ErrNotSupported.
// Lỗi không thuộc sentinel nào là không rõ kết quả, không bao giờ suy ra thất bại.
func ErrorToProto(err error) *connectorpb.ConnectorError {
	if err == nil {
		return nil
	}
	out := &connectorpb.ConnectorError{Message: valid(err.Error())}
	switch {
	case errors.Is(err, connector.ErrUnknownOutcome):
		out.Kind = connectorpb.ErrorKind_ERROR_KIND_UNKNOWN_OUTCOME
	case errors.Is(err, connector.ErrProviderRejected):
		out.Kind = connectorpb.ErrorKind_ERROR_KIND_PROVIDER_REJECTED
		var rej *connector.RejectedError
		if errors.As(err, &rej) {
			out.Code, out.Message = valid(rej.Code), valid(rej.Message)
		}
	case errors.Is(err, connector.ErrProviderUnavailable):
		out.Kind = connectorpb.ErrorKind_ERROR_KIND_PROVIDER_UNAVAILABLE
	case errors.Is(err, connector.ErrNotSupported):
		out.Kind = connectorpb.ErrorKind_ERROR_KIND_NOT_SUPPORTED
	case errors.Is(err, connector.ErrInvalidSignature):
		out.Kind = connectorpb.ErrorKind_ERROR_KIND_INVALID_SIGNATURE
	case errors.Is(err, connector.ErrEventNotRelevant):
		out.Kind = connectorpb.ErrorKind_ERROR_KIND_EVENT_NOT_RELEVANT
	default:
		out.Kind = connectorpb.ErrorKind_ERROR_KIND_UNKNOWN_OUTCOME
	}
	return out
}

// remoteError là lỗi do connector ở xa báo; errors.Is khớp đúng sentinel của kind.
type remoteError struct {
	sentinel error
	msg      string
}

func (e *remoteError) Error() string {
	if e.msg == "" {
		return e.sentinel.Error()
	}
	return e.msg
}

func (e *remoteError) Unwrap() error { return e.sentinel }

// ErrorFromProto dựng lại lỗi phía core. Kind không đặt hoặc lạ là không rõ kết quả, vì ta không biết
// connector có ý nói "chắc chắn chưa xử lý" hay không.
func ErrorFromProto(e *connectorpb.ConnectorError) error {
	if e == nil {
		return nil
	}
	switch e.GetKind() {
	case connectorpb.ErrorKind_ERROR_KIND_PROVIDER_REJECTED:
		return &connector.RejectedError{Code: e.GetCode(), Message: e.GetMessage()}
	case connectorpb.ErrorKind_ERROR_KIND_INVALID_SIGNATURE:
		return &remoteError{connector.ErrInvalidSignature, e.GetMessage()}
	case connectorpb.ErrorKind_ERROR_KIND_NOT_SUPPORTED:
		return &remoteError{connector.ErrNotSupported, e.GetMessage()}
	case connectorpb.ErrorKind_ERROR_KIND_PROVIDER_UNAVAILABLE:
		return &remoteError{connector.ErrProviderUnavailable, e.GetMessage()}
	case connectorpb.ErrorKind_ERROR_KIND_UNKNOWN_OUTCOME:
		return &remoteError{connector.ErrUnknownOutcome, e.GetMessage()}
	case connectorpb.ErrorKind_ERROR_KIND_EVENT_NOT_RELEVANT:
		return &remoteError{connector.ErrEventNotRelevant, e.GetMessage()}
	}
	return fmt.Errorf("%w: loại lỗi %d không hiểu được: %s", connector.ErrUnknownOutcome, int32(e.GetKind()), e.GetMessage())
}
