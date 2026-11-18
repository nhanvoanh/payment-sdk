package connectorrpc_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bitbucket.org/hasaki-tech/payment-connector-sdk/connector"
	"bitbucket.org/hasaki-tech/payment-connector-sdk/connectorpb"
	"bitbucket.org/hasaki-tech/payment-connector-sdk/connectorrpc"
)

func TestErrorRoundTrip(t *testing.T) {
	sentinels := map[connectorpb.ErrorKind]error{
		connectorpb.ErrorKind_ERROR_KIND_INVALID_SIGNATURE:    connector.ErrInvalidSignature,
		connectorpb.ErrorKind_ERROR_KIND_NOT_SUPPORTED:        connector.ErrNotSupported,
		connectorpb.ErrorKind_ERROR_KIND_PROVIDER_UNAVAILABLE: connector.ErrProviderUnavailable,
		connectorpb.ErrorKind_ERROR_KIND_PROVIDER_REJECTED:    connector.ErrProviderRejected,
		connectorpb.ErrorKind_ERROR_KIND_UNKNOWN_OUTCOME:      connector.ErrUnknownOutcome,
		connectorpb.ErrorKind_ERROR_KIND_EVENT_NOT_RELEVANT:   connector.ErrEventNotRelevant,
	}
	for kind, sentinel := range sentinels {
		t.Run(kind.String(), func(t *testing.T) {
			for _, in := range []error{sentinel, fmt.Errorf("bọc: %w", sentinel)} {
				pb := connectorrpc.ErrorToProto(in)
				require.NotNil(t, pb)
				assert.Equal(t, kind, pb.Kind)
				got := connectorrpc.ErrorFromProto(wire(t, pb))
				assert.ErrorIs(t, got, sentinel)
				for k, other := range sentinels {
					if k != kind {
						assert.NotErrorIs(t, got, other)
					}
				}
			}
		})
	}
}

func TestErrorRoundTrip_RejectedKeepsCodeAndMessage(t *testing.T) {
	in := &connector.RejectedError{Code: "51", Message: "không đủ số dư"}
	pb := connectorrpc.ErrorToProto(fmt.Errorf("ngoài: %w", in))
	assert.Equal(t, connectorpb.ErrorKind_ERROR_KIND_PROVIDER_REJECTED, pb.Kind)
	assert.Equal(t, "51", pb.Code)
	assert.Equal(t, "không đủ số dư", pb.Message)

	got := connectorrpc.ErrorFromProto(wire(t, pb))
	var rej *connector.RejectedError
	require.True(t, errors.As(got, &rej))
	assert.Equal(t, "51", rej.Code)
	assert.Equal(t, "không đủ số dư", rej.Message)
	assert.ErrorIs(t, got, connector.ErrProviderRejected)
}

func TestErrorToProto_PlainErrorIsUnknownOutcome(t *testing.T) {
	pb := connectorrpc.ErrorToProto(errors.New("boom"))
	assert.Equal(t, connectorpb.ErrorKind_ERROR_KIND_UNKNOWN_OUTCOME, pb.Kind)
	assert.Equal(t, "boom", pb.Message)
	assert.ErrorIs(t, connectorrpc.ErrorFromProto(pb), connector.ErrUnknownOutcome)
}

func TestErrorToProto_PrecedenceUnknownFirst(t *testing.T) {
	both := fmt.Errorf("%w; %w", connector.ErrUnknownOutcome, connector.ErrProviderUnavailable)
	assert.Equal(t, connectorpb.ErrorKind_ERROR_KIND_UNKNOWN_OUTCOME, connectorrpc.ErrorToProto(both).Kind)
	got := connectorrpc.ErrorFromProto(connectorrpc.ErrorToProto(both))
	assert.ErrorIs(t, got, connector.ErrUnknownOutcome)
	assert.NotErrorIs(t, got, connector.ErrProviderUnavailable)

	rejUnavail := fmt.Errorf("%w; %w", connector.ErrProviderRejected, connector.ErrProviderUnavailable)
	assert.Equal(t, connectorpb.ErrorKind_ERROR_KIND_PROVIDER_REJECTED, connectorrpc.ErrorToProto(rejUnavail).Kind)

	unavailNotSup := fmt.Errorf("%w; %w", connector.ErrProviderUnavailable, connector.ErrNotSupported)
	assert.Equal(t, connectorpb.ErrorKind_ERROR_KIND_PROVIDER_UNAVAILABLE, connectorrpc.ErrorToProto(unavailNotSup).Kind)
}

func TestErrorProto_Nil(t *testing.T) {
	assert.Nil(t, connectorrpc.ErrorToProto(nil))
	assert.NoError(t, connectorrpc.ErrorFromProto(nil))
}

func TestErrorFromProto_UnspecifiedOrUnknownKindIsUnknownOutcome(t *testing.T) {
	for _, k := range []connectorpb.ErrorKind{0, 99, -3} {
		err := connectorrpc.ErrorFromProto(&connectorpb.ConnectorError{Kind: k, Message: "m"})
		require.Error(t, err, k)
		assert.ErrorIs(t, err, connector.ErrUnknownOutcome, k)
		assert.NotErrorIs(t, err, connector.ErrProviderUnavailable)
		assert.NotErrorIs(t, err, connector.ErrProviderRejected)
	}
}

func TestErrorFromProto_MessageNotDuplicated(t *testing.T) {
	err := connectorrpc.ErrorFromProto(connectorrpc.ErrorToProto(connector.ErrProviderUnavailable))
	assert.Equal(t, connector.ErrProviderUnavailable.Error(), err.Error())
}

// ErrorToProto có thứ tự xét riêng (thêm chữ ký sai, sự kiện không liên quan) nhưng mọi tổ hợp sentinel đi
// qua dây phải được core phân loại đúng như lỗi gốc: một khác biệt là connector nói một đằng, core hiểu một nẻo.
func TestErrorToProto_AgreesWithClassify(t *testing.T) {
	sentinels := []error{connector.ErrUnknownOutcome, &connector.RejectedError{Code: "c", Message: "m"},
		connector.ErrProviderUnavailable, connector.ErrNotSupported, connector.ErrInvalidSignature, connector.ErrEventNotRelevant}
	for mask := 0; mask < 1<<len(sentinels); mask++ {
		errs := []error{errors.New("gốc")}
		for i, s := range sentinels {
			if mask&(1<<i) != 0 {
				errs = append(errs, s)
			}
		}
		in := errors.Join(errs...)
		got := connectorrpc.ErrorFromProto(wire(t, connectorrpc.ErrorToProto(in)))
		assert.Equal(t, connector.Classify(in), connector.Classify(got), "tổ hợp %b: %v", mask, in)
	}
}
