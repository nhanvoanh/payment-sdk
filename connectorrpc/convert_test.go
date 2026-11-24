package connectorrpc_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/nhanvoanh/payment-sdk/connector"
	"github.com/nhanvoanh/payment-sdk/connectorpb"
	"github.com/nhanvoanh/payment-sdk/connectorrpc"
	"github.com/nhanvoanh/payment-sdk/paykit"
)

// wire đưa message qua marshal/unmarshal thật, để bắt lỗi chuỗi không hợp lệ UTF-8 hay trường mất.
func wire[T proto.Message](t *testing.T, m T) T {
	t.Helper()
	b, err := proto.Marshal(m)
	require.NoError(t, err)
	out := m.ProtoReflect().New().Interface().(T)
	require.NoError(t, proto.Unmarshal(b, out))
	return out
}

var (
	t0 = time.Date(2026, 11, 18, 9, 30, 15, 123456789, time.UTC)
	// tCreated: thời điểm tạo như core gửi (UTC, micro giây), sát nửa đêm để lệch múi giờ đổi ngày.
	tCreated = time.Date(2026, 11, 18, 23, 59, 59, 999999000, time.UTC)
	card     = &paykit.CardMasked{Brand: "visa", Last4: "4242", Holder: "NGUYỄN VĂN A"}
	raw      = connector.RawExchange{RequestBody: "req ✓", ResponseBody: "phản hồi", HTTPStatus: 201, DurationMS: 42}
	cust     = paykit.Customer{ID: "c1", Name: "Trần Thị Bé", Email: "a@acme.test", Phone: "+84901234567",
		Address: "12 Lê Lợi\r\nPhường Bến Nghé\tQuận 1\n"}
	meta = map[string]string{"k": "v", "tên": "giá trị"}
)

func TestStatus_RoundTrip(t *testing.T) {
	for _, s := range []connector.Status{
		connector.StatusPending, connector.StatusSucceeded, connector.StatusFailed,
		connector.StatusCanceled, connector.StatusOnHold,
	} {
		got, err := connectorrpc.StatusFromProto(connectorrpc.StatusToProto(s))
		require.NoError(t, err, s)
		assert.Equal(t, s, got)
	}
}

func TestStatusFromProto_UnknownIsUnknownOutcome(t *testing.T) {
	for _, v := range []connectorpb.Status{0, 99, -1} {
		_, err := connectorrpc.StatusFromProto(v)
		assert.ErrorIs(t, err, connector.ErrUnknownOutcome, v)
	}
	assert.Equal(t, connectorpb.Status_STATUS_UNSPECIFIED, connectorrpc.StatusToProto("lạ"))
}

func TestCreatePaymentRequest_RoundTrip(t *testing.T) {
	full := connector.CreatePaymentRequest{
		TransactionCode: "TX1", IdempotencyKey: "idem", Money: paykit.Money{Amount: 1_000_000, Currency: "VND"},
		Method: paykit.MethodQR, Customer: cust, ReturnURL: "https://r", CancelURL: "https://c",
		ClientIP: "1.2.3.4", Metadata: meta, CreatedAt: tCreated,
	}
	for name, in := range map[string]connector.CreatePaymentRequest{
		"đầy đủ": full, "zero": {}, "metadata rỗng": {Metadata: map[string]string{}},
	} {
		t.Run(name, func(t *testing.T) {
			got := connectorrpc.CreatePaymentRequestFromProto(wire(t, connectorrpc.CreatePaymentRequestToProto(in)))
			if len(in.Metadata) == 0 {
				in.Metadata = nil // proto không phân biệt map nil và rỗng
			}
			assert.Equal(t, in, got)
		})
	}
}

func TestChargeRequest_RoundTrip(t *testing.T) {
	in := connector.ChargeRequest{
		TransactionCode: "TX1", IdempotencyKey: "idem", Money: paykit.Money{Amount: 5, Currency: "USD"},
		Method: paykit.MethodVisa, Customer: cust, ReturnURL: "u1", CancelURL: "u2", ClientIP: "::1", Metadata: meta,
		CreatedAt: tCreated,
	}
	assert.Equal(t, in, connectorrpc.ChargeRequestFromProto(wire(t, connectorrpc.ChargeRequestToProto(in))))
	assert.Equal(t, connector.ChargeRequest{}, connectorrpc.ChargeRequestFromProto(wire(t, connectorrpc.ChargeRequestToProto(connector.ChargeRequest{}))))
	assert.Equal(t, connector.ChargeRequest{}, connectorrpc.ChargeRequestFromProto(nil))
}

func TestSimpleRequests_RoundTrip(t *testing.T) {
	q := connector.QueryPaymentRequest{TransactionCode: "T", ProviderRef: "P", CreatedAt: tCreated}
	assert.Equal(t, q, connectorrpc.QueryPaymentRequestFromProto(wire(t, connectorrpc.QueryPaymentRequestToProto(q))))
	c := connector.CancelRequest{TransactionCode: "T", ProviderRef: "P", IdempotencyKey: "I", CreatedAt: tCreated}
	assert.Equal(t, c, connectorrpc.CancelRequestFromProto(wire(t, connectorrpc.CancelRequestToProto(c))))
	r := connector.RefundRequest{
		RefundCode: "R", IdempotencyKey: "I", TransactionCode: "T", ProviderRef: "P",
		Money: paykit.Money{Amount: 9, Currency: "VND"}, Reason: "khách đổi ý", CustomerID: "c",
		TransactionCreatedAt: tCreated, CreatedAt: tCreated.Add(time.Hour),
	}
	assert.Equal(t, r, connectorrpc.RefundRequestFromProto(wire(t, connectorrpc.RefundRequestToProto(r))))
	qr := connector.QueryRefundRequest{RefundCode: "R", ProviderRef: "P", TransactionCode: "T",
		CreatedAt: tCreated.Add(time.Hour), TransactionCreatedAt: tCreated}
	assert.Equal(t, qr, connectorrpc.QueryRefundRequestFromProto(wire(t, connectorrpc.QueryRefundRequestToProto(qr))))
	assert.Equal(t, connector.RefundRequest{}, connectorrpc.RefundRequestFromProto(nil))
}

func TestCreatePaymentResult_RoundTrip(t *testing.T) {
	for name, in := range map[string]connector.CreatePaymentResult{
		"đầy đủ": {
			ProviderRef: "P", Status: connector.StatusPending, ExpiresAt: t0, Raw: raw,
			NextAction: paykit.NextAction{
				Kind: paykit.NextActionRedirect, URL: "https://x", QRData: "qr",
				FormFields: map[string]string{"a": "b"},
			},
		},
		"zero thời gian, form nil": {ProviderRef: "P", Status: connector.StatusSucceeded},
		"form rỗng":                {Status: connector.StatusFailed, NextAction: paykit.NextAction{FormFields: map[string]string{}}},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := connectorrpc.CreatePaymentResultFromProto(wire(t, connectorrpc.CreatePaymentResultToProto(in)))
			require.NoError(t, err)
			if len(in.NextAction.FormFields) == 0 {
				in.NextAction.FormFields = nil
			}
			assert.Equal(t, in, got)
			assert.True(t, got.ExpiresAt.Equal(in.ExpiresAt))
		})
	}
	// Zero value: Status rỗng không phải trạng thái hợp lệ nên phải ra lỗi.
	_, err := connectorrpc.CreatePaymentResultFromProto(wire(t, connectorrpc.CreatePaymentResultToProto(connector.CreatePaymentResult{})))
	assert.ErrorIs(t, err, connector.ErrUnknownOutcome)
}

func TestChargeResult_RoundTrip(t *testing.T) {
	in := connector.ChargeResult{
		ProviderRef: "P", Status: connector.StatusSucceeded, AmountPaid: paykit.Money{Amount: 77, Currency: "VND"},
		PaidAt: t0, NextAction: paykit.NextAction{Kind: paykit.NextActionNone}, Raw: raw,
	}
	got, err := connectorrpc.ChargeResultFromProto(wire(t, connectorrpc.ChargeResultToProto(in)))
	require.NoError(t, err)
	assert.Equal(t, in, got)
}

func TestPaymentStatus_RoundTrip(t *testing.T) {
	for name, in := range map[string]connector.PaymentStatus{
		"đầy đủ": {
			Found: true, Status: connector.StatusSucceeded, ProviderRef: "P",
			AmountPaid: paykit.Money{Amount: 1, Currency: "VND"}, PaidAt: t0, Card: card,
			ProviderCode: "00", ProviderMessage: "thành công", Raw: raw,
		},
		"Card nil":       {Found: true, Status: connector.StatusPending},
		"Card rỗng":      {Found: true, Status: connector.StatusPending, Card: &paykit.CardMasked{}},
		"không tìm thấy": {Found: false, Raw: raw},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := connectorrpc.PaymentStatusFromProto(wire(t, connectorrpc.PaymentStatusToProto(in)))
			require.NoError(t, err)
			assert.Equal(t, in, got)
		})
	}
	// found=true mà status không đặt thì không rõ kết quả.
	_, err := connectorrpc.PaymentStatusFromProto(&connectorpb.QueryPaymentResponse{Found: true})
	assert.ErrorIs(t, err, connector.ErrUnknownOutcome)
	// found=false thì status lạ cũng bị bỏ qua.
	got, err := connectorrpc.PaymentStatusFromProto(&connectorpb.QueryPaymentResponse{Found: false, Status: 99})
	require.NoError(t, err)
	assert.False(t, got.Found)
}

func TestCancelResult_RoundTrip(t *testing.T) {
	in := connector.CancelResult{Status: connector.StatusCanceled, Raw: raw}
	got, err := connectorrpc.CancelResultFromProto(wire(t, connectorrpc.CancelResultToProto(in)))
	require.NoError(t, err)
	assert.Equal(t, in, got)
	_, err = connectorrpc.CancelResultFromProto(&connectorpb.CancelPaymentResponse{})
	assert.ErrorIs(t, err, connector.ErrUnknownOutcome)
}

func TestRefundResult_RoundTrip(t *testing.T) {
	in := connector.RefundResult{ProviderRef: "RP", Status: connector.StatusPending, Raw: raw}
	got, err := connectorrpc.RefundResultFromProto(wire(t, connectorrpc.RefundResultToProto(in)))
	require.NoError(t, err)
	assert.Equal(t, in, got)
	_, err = connectorrpc.RefundResultFromProto(&connectorpb.CreateRefundResponse{Status: 99})
	assert.ErrorIs(t, err, connector.ErrUnknownOutcome)
}

func TestRefundStatus_RoundTrip(t *testing.T) {
	in := connector.RefundStatus{Found: true, Status: connector.StatusSucceeded, ProviderRef: "P", ProviderCode: "c", ProviderMessage: "m", Raw: raw}
	got, err := connectorrpc.RefundStatusFromProto(wire(t, connectorrpc.RefundStatusToProto(in)))
	require.NoError(t, err)
	assert.Equal(t, in, got)

	nf := connector.RefundStatus{Found: false}
	got, err = connectorrpc.RefundStatusFromProto(wire(t, connectorrpc.RefundStatusToProto(nf)))
	require.NoError(t, err)
	assert.Equal(t, nf, got)

	_, err = connectorrpc.RefundStatusFromProto(&connectorpb.QueryRefundResponse{Found: true})
	assert.ErrorIs(t, err, connector.ErrUnknownOutcome)
}

func TestProviderEvent_RoundTrip(t *testing.T) {
	for name, in := range map[string]connector.ProviderEvent{
		"đầy đủ": {
			EventID: "e1", Kind: connector.EventKindPayment, TransactionCode: "T", RefundCode: "R", ProviderRef: "P",
			Status: connector.StatusSucceeded, AmountPaid: paykit.Money{Amount: 3, Currency: "VND"}, PaidAt: t0,
			Card: card, ProviderCode: "00", ProviderMessage: "ok", Raw: []byte{0, 0xff, 'x'},
		},
		"tối thiểu": {EventID: "e", Kind: connector.EventKindRefund, Status: connector.StatusFailed},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := connectorrpc.ProviderEventFromProto(wire(t, connectorrpc.ProviderEventToProto(in)))
			require.NoError(t, err)
			assert.Equal(t, in, got)
		})
	}
	_, err := connectorrpc.ProviderEventFromProto(&connectorpb.ProviderEvent{EventId: ""})
	assert.ErrorIs(t, err, connector.ErrUnknownOutcome)
}

func TestRawWebhook_RoundTrip(t *testing.T) {
	in := connector.RawWebhook{
		Headers: map[string][]string{
			"X-Sig":   {"a", "b", "a"},
			"Tên":     {"giá trị"},
			"":        {""},
			"k\xff1":  {"v\xff", "\x00", ""},
			"k\xfe1":  {"khác"}, // chỉ khác khoá trên ở một byte hỏng
			"nul\x00": {"z"},
			"rỗng":    {},
		},
		Query:    map[string][]string{"q": {"1", "2", "3"}, "\xff": {"\xff\xff"}},
		RawQuery: "q=1&q=2&q=3&sig=a+b%2Bc&x=%zz;y=1&\xff=\xff\xff",
		Body:     []byte{0, 1, 2, 0xff},
		RemoteIP: "10.0.0.1",
	}
	got := connectorrpc.RawWebhookFromProto(wire(t, connectorrpc.RawWebhookToProto(in)))
	assert.Equal(t, in, got) // so từng byte và thứ tự giá trị trong khoá
	assert.Len(t, got.Headers, len(in.Headers), "khoá khác nhau không bị gộp")
	assert.Equal(t, []string{"v\xff", "\x00", ""}, got.Headers["k\xff1"])
	assert.Equal(t, []string{"khác"}, got.Headers["k\xfe1"])
	assert.Equal(t, []string{"a", "b", "a"}, got.Headers["X-Sig"])
	assert.Equal(t, connector.RawWebhook{}, connectorrpc.RawWebhookFromProto(wire(t, connectorrpc.RawWebhookToProto(connector.RawWebhook{}))))
	assert.Equal(t, connector.RawWebhook{}, connectorrpc.RawWebhookFromProto(nil))
}

func TestParseResultFromProto(t *testing.T) {
	ok := &connectorpb.ProviderEvent{EventId: "e", Kind: "payment", Status: connectorpb.Status_STATUS_SUCCEEDED}
	type resp struct {
		err *connectorpb.ConnectorError
		ev  *connectorpb.ProviderEvent
	}
	kind := func(k connectorpb.ErrorKind) *connectorpb.ConnectorError { return &connectorpb.ConnectorError{Kind: k} }
	cases := []struct {
		name string
		in   resp
		want error // nil: thành công
	}{
		{"ok", resp{nil, ok}, nil},
		{"chữ ký sai, event nil", resp{kind(connectorpb.ErrorKind_ERROR_KIND_INVALID_SIGNATURE), nil}, connector.ErrInvalidSignature},
		{"không liên quan, event nil", resp{kind(connectorpb.ErrorKind_ERROR_KIND_EVENT_NOT_RELEVANT), nil}, connector.ErrEventNotRelevant},
		{"event nil không lỗi", resp{nil, nil}, connector.ErrUnknownOutcome},
		{"status lạ", resp{nil, &connectorpb.ProviderEvent{EventId: "e", Status: 99}}, connector.ErrUnknownOutcome},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w, werr := connectorrpc.ParseWebhookResultFromProto(&connectorpb.ParseWebhookResponse{Error: c.in.err, Event: c.in.ev})
			r, rerr := connectorrpc.ParseRedirectResultFromProto(&connectorpb.ParseRedirectResponse{Error: c.in.err, Event: c.in.ev})
			if c.want == nil {
				require.NoError(t, werr)
				require.NoError(t, rerr)
				assert.Equal(t, "e", w.EventID)
				assert.Equal(t, w, r)
				return
			}
			assert.ErrorIs(t, werr, c.want)
			assert.ErrorIs(t, rerr, c.want)
			if c.want != connector.ErrUnknownOutcome {
				assert.NotErrorIs(t, werr, connector.ErrUnknownOutcome)
				assert.NotErrorIs(t, rerr, connector.ErrUnknownOutcome)
			}
		})
	}
	_, err := connectorrpc.ParseWebhookResultFromProto(nil)
	assert.ErrorIs(t, err, connector.ErrUnknownOutcome)
	_, err = connectorrpc.ParseRedirectResultFromProto(nil)
	assert.ErrorIs(t, err, connector.ErrUnknownOutcome)
}

func TestTimeToProto_OutOfRangeIsUnset(t *testing.T) {
	for _, y := range []int{0, 10000} {
		ts := time.Date(y, 1, 1, 0, 0, 0, 0, time.UTC)
		assert.Nil(t, connectorrpc.TimeToProto(ts), y)
		res := connectorrpc.PaymentStatusToProto(connector.PaymentStatus{Found: true, Status: connector.StatusSucceeded, PaidAt: ts})
		_, err := proto.Marshal(res)
		require.NoError(t, err)
		got, err := connectorrpc.PaymentStatusFromProto(wire(t, res))
		require.NoError(t, err)
		assert.True(t, got.PaidAt.IsZero())
	}
	assert.NotNil(t, connectorrpc.TimeToProto(t0))
}

func TestRawExchange_Truncates(t *testing.T) {
	const limit = 64 << 10
	big := strings.Repeat("a", limit+100)
	got := connectorrpc.RawExchangeToProto(connector.RawExchange{RequestBody: big, ResponseBody: big})
	for _, b := range []string{got.RequestBody, got.ResponseBody} {
		assert.True(t, strings.HasSuffix(b, "…[cắt]"))
		assert.Equal(t, limit+len("…[cắt]"), len(b))
	}
	exact := strings.Repeat("b", limit)
	got = connectorrpc.RawExchangeToProto(connector.RawExchange{RequestBody: exact})
	assert.Equal(t, exact, got.RequestBody, "đúng 64 KiB thì không cắt")

	// Cắt giữa ký tự nhiều byte phải lùi về ranh giới ký tự để chuỗi vẫn hợp lệ UTF-8.
	multi := strings.Repeat("a", limit-1) + "ế" + "tail"
	got = connectorrpc.RawExchangeToProto(connector.RawExchange{RequestBody: multi})
	_, err := proto.Marshal(got)
	require.NoError(t, err)
	// ký tự "ế" (3 byte) bắt đầu ở vị trí limit-1 nên bị cắt đôi: chỉ giữ phần 'a' phía trước.
	assert.Equal(t, strings.Repeat("a", limit-1)+"…[cắt]", got.RequestBody)
}

func TestToProto_InvalidUTF8DoesNotBreakMarshal(t *testing.T) {
	bad := "x\xffy"
	res := connectorrpc.CreatePaymentResultToProto(connector.CreatePaymentResult{
		ProviderRef: bad, Status: connector.StatusPending, Raw: connector.RawExchange{ResponseBody: bad},
	})
	_, err := proto.Marshal(res)
	require.NoError(t, err)
	assert.Equal(t, "x�y", res.ProviderRef)
}

func TestProviderEventToProto_InvalidUTF8EventIDIsHashed(t *testing.T) {
	// Thay bằng U+FFFD sẽ làm hai EventID hỏng khác nhau đụng khoá khử trùng; dùng đúng quy tắc băm của core.
	a := connectorrpc.ProviderEventToProto(connector.ProviderEvent{EventID: "e\xff1", Status: connector.StatusPending})
	b := connectorrpc.ProviderEventToProto(connector.ProviderEvent{EventID: "e\xfe1", Status: connector.StatusPending})
	assert.True(t, strings.HasPrefix(a.EventId, "sha256:"))
	assert.NotEqual(t, a.EventId, b.EventId)
	again := connectorrpc.ProviderEventToProto(connector.ProviderEvent{EventID: "e\xff1", Status: connector.StatusPending})
	assert.Equal(t, a.EventId, again.EventId)
}

func TestResultFromProto_ErrorAndNilResponse(t *testing.T) {
	_, err := connectorrpc.CreatePaymentResultFromProto(nil)
	assert.ErrorIs(t, err, connector.ErrUnknownOutcome)

	resp := &connectorpb.ChargeResponse{
		Error: &connectorpb.ConnectorError{Kind: connectorpb.ErrorKind_ERROR_KIND_PROVIDER_REJECTED, Code: "51"},
		Raw:   &connectorpb.RawExchange{HttpStatus: 402},
	}
	res, err := connectorrpc.ChargeResultFromProto(resp)
	var rej *connector.RejectedError
	require.True(t, errors.As(err, &rej))
	assert.Equal(t, "51", rej.Code)
	assert.Equal(t, 402, res.Raw.HTTPStatus, "Raw vẫn được trả kèm lỗi để lưu vết")
}

func TestProtocolVersion(t *testing.T) {
	assert.Equal(t, 1, connectorrpc.ProtocolVersion)
}

func TestWebhookOutcomes_FourKeys(t *testing.T) {
	assert.ElementsMatch(t,
		[]connector.WebhookOutcome{connector.OutcomeApplied, connector.OutcomeDuplicate, connector.OutcomeIgnored, connector.OutcomeRejected},
		connectorrpc.WebhookOutcomes)
}
