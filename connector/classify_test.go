package connector

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want Outcome
	}{
		{"nil", nil, CallOK},
		{"unknown", ErrUnknownOutcome, CallUnknown},
		{"unknown bọc", fmt.Errorf("x: %w", ErrUnknownOutcome), CallUnknown},
		{"rejected", ErrProviderRejected, CallRejected},
		{"RejectedError", &RejectedError{Code: "c"}, CallRejected},
		{"RejectedError bọc", fmt.Errorf("x: %w", &RejectedError{Code: "c"}), CallRejected},
		{"unavailable", ErrProviderUnavailable, CallUnavailable},
		{"unavailable bọc", fmt.Errorf("x: %w", ErrProviderUnavailable), CallUnavailable},
		{"not supported", ErrNotSupported, CallUnavailable},
		{"unknown thắng rejected", errors.Join(&RejectedError{Code: "c"}, ErrUnknownOutcome), CallUnknown},
		{"unknown thắng unavailable", errors.Join(ErrProviderUnavailable, ErrUnknownOutcome), CallUnknown},
		{"unknown thắng not supported", errors.Join(ErrNotSupported, ErrUnknownOutcome), CallUnknown},
		{"rejected thắng unavailable", errors.Join(ErrProviderUnavailable, ErrProviderRejected), CallRejected},
		{"rejected thắng not supported", errors.Join(ErrNotSupported, ErrProviderRejected), CallRejected},
		{"lỗi thường", errors.New("boom"), CallUnknown},
		{"hết hạn", context.DeadlineExceeded, CallUnknown},
		{"huỷ", context.Canceled, CallUnknown},
		{"chữ ký sai", ErrInvalidSignature, CallUnknown},
		{"sự kiện không liên quan", ErrEventNotRelevant, CallUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { assert.Equal(t, c.want, Classify(c.err)) })
	}
}
