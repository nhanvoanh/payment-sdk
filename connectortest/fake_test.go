package connectortest

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"bitbucket.org/hasaki-tech/payment-connector-sdk/connector"
	"bitbucket.org/hasaki-tech/payment-connector-sdk/paykit"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var ctx = context.Background()

func TestFake_Defaults(t *testing.T) {
	f := New("acme", paykit.MethodQR)
	assert.Equal(t, "acme", f.Name())
	assert.Equal(t, []paykit.Method{paykit.MethodQR}, f.Methods())
	var _ connector.Connector = f
	var _ connector.CapabilityReporter = f

	money := paykit.Money{Amount: 1000, Currency: "VND"}

	cr, err := f.CreatePayment(ctx, connector.CreatePaymentRequest{TransactionCode: "T1", Money: money})
	require.NoError(t, err)
	assert.Equal(t, connector.StatusPending, cr.Status)
	assert.Equal(t, "fake_T1", cr.ProviderRef)
	assert.Equal(t, paykit.NextAction{Kind: paykit.NextActionRedirect, URL: "https://pay.example/T1"}, cr.NextAction)

	ch, err := f.Charge(ctx, connector.ChargeRequest{TransactionCode: "T1", Money: money})
	require.NoError(t, err)
	assert.Equal(t, connector.StatusSucceeded, ch.Status)
	assert.Equal(t, money, ch.AmountPaid)
	assert.Equal(t, "fake_T1", ch.ProviderRef)
	assert.WithinDuration(t, time.Now(), ch.PaidAt, time.Minute)
	assert.Equal(t, time.UTC, ch.PaidAt.Location())

	q, err := f.QueryPayment(ctx, connector.QueryPaymentRequest{})
	require.NoError(t, err)
	assert.False(t, q.Found)

	cn, err := f.CancelPayment(ctx, connector.CancelRequest{})
	require.NoError(t, err)
	assert.Equal(t, connector.StatusCanceled, cn.Status)

	rf, err := f.CreateRefund(ctx, connector.RefundRequest{RefundCode: "R1", Money: money})
	require.NoError(t, err)
	assert.Equal(t, connector.StatusSucceeded, rf.Status)
	assert.Equal(t, "fake_R1", rf.ProviderRef)

	qr, err := f.QueryRefund(ctx, connector.QueryRefundRequest{})
	require.NoError(t, err)
	assert.False(t, qr.Found)

	status, body, ct := f.AckWebhook(connector.OutcomeApplied)
	assert.Equal(t, 200, status)
	assert.Equal(t, "OK", string(body))
	assert.Equal(t, "text/plain", ct)

	for _, op := range []connector.Operation{connector.OpQueryPayment, connector.OpWebhook, connector.OpRedirect,
		connector.OpCharge, connector.OpCancel, connector.OpRefund, connector.OpQueryRefund} {
		assert.True(t, connector.Supports(f, op), op)
	}
}

func TestFake_DisabledCapabilityNotSupported(t *testing.T) {
	f := New("acme")
	f.Capabilities[connector.OpCharge] = false
	f.Capabilities[connector.OpQueryPayment] = false
	f.Capabilities[connector.OpWebhook] = false
	f.Capabilities[connector.OpRedirect] = false
	f.Capabilities[connector.OpCancel] = false
	f.Capabilities[connector.OpRefund] = false
	f.Capabilities[connector.OpQueryRefund] = false

	_, ok := connector.AsCharger(f)
	assert.False(t, ok)

	_, err := f.Charge(ctx, connector.ChargeRequest{})
	assert.ErrorIs(t, err, connector.ErrNotSupported)
	_, err = f.QueryPayment(ctx, connector.QueryPaymentRequest{})
	assert.ErrorIs(t, err, connector.ErrNotSupported)
	_, err = f.CancelPayment(ctx, connector.CancelRequest{})
	assert.ErrorIs(t, err, connector.ErrNotSupported)
	_, err = f.CreateRefund(ctx, connector.RefundRequest{})
	assert.ErrorIs(t, err, connector.ErrNotSupported)
	_, err = f.QueryRefund(ctx, connector.QueryRefundRequest{})
	assert.ErrorIs(t, err, connector.ErrNotSupported)
	_, err = f.ParseWebhook(ctx, connector.RawWebhook{})
	assert.ErrorIs(t, err, connector.ErrNotSupported)
	_, err = f.ParseRedirect(ctx, connector.RawWebhook{})
	assert.ErrorIs(t, err, connector.ErrNotSupported)
	assert.Zero(t, f.Calls("charge"))
}

func TestFake_HooksAndCallLog(t *testing.T) {
	f := New("acme")
	boom := errors.New("boom")
	f.OnCreate = func(context.Context, connector.CreatePaymentRequest) (connector.CreatePaymentResult, error) {
		// Hook chạy không giữ khoá: gọi ngược Fake không được deadlock.
		assert.Equal(t, 1, f.Calls("create"))
		return connector.CreatePaymentResult{}, boom
	}
	f.OnCharge = func(context.Context, connector.ChargeRequest) (connector.ChargeResult, error) {
		return connector.ChargeResult{Status: connector.StatusFailed}, nil
	}

	_, err := f.CreatePayment(ctx, connector.CreatePaymentRequest{TransactionCode: "T1"})
	assert.ErrorIs(t, err, boom)
	ch, err := f.Charge(ctx, connector.ChargeRequest{})
	require.NoError(t, err)
	assert.Equal(t, connector.StatusFailed, ch.Status)
	_, _ = f.QueryPayment(ctx, connector.QueryPaymentRequest{})
	_, _ = f.CreateRefund(ctx, connector.RefundRequest{})
	_, _ = f.CancelPayment(ctx, connector.CancelRequest{})
	_, _ = f.QueryRefund(ctx, connector.QueryRefundRequest{})

	assert.Equal(t, []string{"create", "charge", "query", "refund", "cancel", "query_refund"}, f.CallLog())
	assert.Equal(t, 1, f.Calls("charge"))
	assert.Equal(t, 0, f.Calls("parse_webhook"))

	log := f.CallLog()
	log[0] = "x"
	assert.Equal(t, "create", f.CallLog()[0])
}

func TestFake_ParseWebhookSignature(t *testing.T) {
	f := New("acme")
	ev := connector.ProviderEvent{EventID: "e1", Kind: connector.EventKindPayment, TransactionCode: "T1", Status: connector.StatusSucceeded}
	body, err := json.Marshal(ev)
	require.NoError(t, err)

	got, err := f.ParseWebhook(ctx, connector.RawWebhook{Body: body})
	require.NoError(t, err)
	assert.Equal(t, ev, got)

	got, err = f.ParseRedirect(ctx, connector.RawWebhook{Body: body})
	require.NoError(t, err)
	assert.Equal(t, ev, got)

	_, err = f.ParseWebhook(ctx, connector.RawWebhook{Body: body, Headers: map[string][]string{"X-Fake-Signature": {"bad"}}})
	assert.ErrorIs(t, err, connector.ErrInvalidSignature)

	_, err = f.ParseWebhook(ctx, connector.RawWebhook{Body: []byte("not json")})
	assert.Error(t, err)

	assert.Equal(t, 4, f.Calls("parse_webhook"))
}

func TestFake_ConcurrentCallsCounted(t *testing.T) {
	f := New("acme")
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = f.CreatePayment(ctx, connector.CreatePaymentRequest{TransactionCode: "T"})
		}()
	}
	wg.Wait()
	assert.Equal(t, 100, f.Calls("create"))
	assert.Len(t, f.CallLog(), 100)
}
