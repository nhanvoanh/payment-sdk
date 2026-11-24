package connector

import (
	"context"
	"errors"
	"testing"

	"github.com/nhanvoanh/payment-sdk/paykit"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type baseConn struct {
	name    string
	methods []paykit.Method
}

func (b baseConn) Name() string             { return b.name }
func (b baseConn) Methods() []paykit.Method { return b.methods }
func (b baseConn) CreatePayment(context.Context, CreatePaymentRequest) (CreatePaymentResult, error) {
	return CreatePaymentResult{}, nil
}

type refunderConn struct{ baseConn }

func (refunderConn) CreateRefund(context.Context, RefundRequest) (RefundResult, error) {
	return RefundResult{}, nil
}

type fullConn struct{ baseConn }

func (fullConn) QueryPayment(context.Context, QueryPaymentRequest) (PaymentStatus, error) {
	return PaymentStatus{}, nil
}
func (fullConn) ParseWebhook(context.Context, RawWebhook) (ProviderEvent, error) {
	return ProviderEvent{}, nil
}
func (fullConn) AckWebhook(WebhookOutcome) (int, []byte, string) { return 200, nil, "" }
func (fullConn) ParseRedirect(context.Context, RawWebhook) (ProviderEvent, error) {
	return ProviderEvent{}, nil
}
func (fullConn) Charge(context.Context, ChargeRequest) (ChargeResult, error) {
	return ChargeResult{}, nil
}
func (fullConn) CancelPayment(context.Context, CancelRequest) (CancelResult, error) {
	return CancelResult{}, nil
}
func (fullConn) CreateRefund(context.Context, RefundRequest) (RefundResult, error) {
	return RefundResult{}, nil
}
func (fullConn) QueryRefund(context.Context, QueryRefundRequest) (RefundStatus, error) {
	return RefundStatus{}, nil
}

type reporterConn struct {
	fullConn
	ops map[Operation]bool
}

func (r reporterConn) Supports(op Operation) bool { return r.ops[op] }

func TestRejectedErrorUnwraps(t *testing.T) {
	var err error = &RejectedError{Code: "51", Message: "insufficient"}
	assert.True(t, errors.Is(err, ErrProviderRejected))
	assert.False(t, errors.Is(err, ErrUnknownOutcome))
	assert.Contains(t, err.Error(), "51")
}

func TestSupports_TypeAssertionFallback(t *testing.T) {
	c := refunderConn{baseConn{name: "a"}}
	assert.True(t, Supports(c, OpRefund))
	assert.False(t, Supports(c, OpCharge))
	r, ok := AsRefunder(c)
	assert.True(t, ok)
	assert.NotNil(t, r)
	ch, ok := AsCharger(c)
	assert.False(t, ok)
	assert.Nil(t, ch)
}

func TestSupports_ReporterWins(t *testing.T) {
	c := reporterConn{fullConn: fullConn{baseConn{name: "a"}}, ops: map[Operation]bool{OpRefund: true}}
	ch, ok := AsCharger(c)
	assert.False(t, ok)
	assert.Nil(t, ch)
	_, ok = AsRefunder(c)
	assert.True(t, ok)
	_, ok = AsPaymentQuerier(c)
	assert.False(t, ok)
	_, ok = AsWebhookReceiver(c)
	assert.False(t, ok)
	_, ok = AsRedirecter(c)
	assert.False(t, ok)
	_, ok = AsCanceler(c)
	assert.False(t, ok)
	_, ok = AsRefundQuerier(c)
	assert.False(t, ok)
}

func TestRegistry_RegisterGetNames(t *testing.T) {
	r := NewRegistry()
	require.NoError(t, r.Register(baseConn{name: "b", methods: []paykit.Method{paykit.MethodQR}}))
	require.NoError(t, r.Register(baseConn{name: "a", methods: []paykit.Method{paykit.MethodVisa}}))
	assert.Equal(t, []string{"a", "b"}, r.Names())
	c, ok := r.Get("a")
	assert.True(t, ok)
	assert.Equal(t, "a", c.Name())
	_, ok = r.Get("zzz")
	assert.False(t, ok)
}

func TestRegistry_DuplicateAndEmptyNameRejected(t *testing.T) {
	r := NewRegistry()
	require.NoError(t, r.Register(baseConn{name: "a"}))
	assert.Error(t, r.Register(baseConn{name: "a"}))
	assert.Error(t, r.Register(baseConn{name: ""}))
	assert.Equal(t, []string{"a"}, r.Names())
}

func TestRegistry_ForMethodAndSetDefault(t *testing.T) {
	r := NewRegistry()
	require.NoError(t, r.Register(baseConn{name: "first", methods: []paykit.Method{paykit.MethodQR}}))
	require.NoError(t, r.Register(baseConn{name: "second", methods: []paykit.Method{paykit.MethodQR, paykit.MethodVisa}}))

	_, ok := r.ForMethod(paykit.MethodQR)
	assert.False(t, ok, "hai connector khai qr mà không có mặc định: không đoán")
	c, ok := r.ForMethod(paykit.MethodVisa)
	require.True(t, ok, "đúng một connector khai visa thì trả nó")
	assert.Equal(t, "second", c.Name())
	_, ok = r.ForMethod(paykit.MethodPOS)
	assert.False(t, ok)

	assert.Error(t, r.SetDefault(paykit.MethodQR, ""))

	require.NoError(t, r.SetDefault(paykit.MethodQR, "second"))
	c, ok = r.ForMethod(paykit.MethodQR)
	require.True(t, ok)
	assert.Equal(t, "second", c.Name())
}
