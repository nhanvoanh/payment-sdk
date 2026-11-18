package connectorrpc_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"bitbucket.org/hasaki-tech/payment-connector-sdk/connector"
	"bitbucket.org/hasaki-tech/payment-connector-sdk/connectorrpc"
	"bitbucket.org/hasaki-tech/payment-connector-sdk/connectorrpc/grpctest"
	"bitbucket.org/hasaki-tech/payment-connector-sdk/connectortest"
	"bitbucket.org/hasaki-tech/payment-connector-sdk/paykit"
)

func newFake() *connectortest.Fake {
	return connectortest.New("acme", paykit.MethodQR, paykit.MethodVisa)
}

func TestRoundTrip_AllNineOperations(t *testing.T) {
	f := newFake()
	var gotCreate connector.CreatePaymentRequest
	var gotCharge connector.ChargeRequest
	var gotQuery connector.QueryPaymentRequest
	var gotCancel connector.CancelRequest
	var gotRefund connector.RefundRequest
	var gotQR connector.QueryRefundRequest
	var gotRaw []connector.RawWebhook

	f.OnCreate = func(_ context.Context, r connector.CreatePaymentRequest) (connector.CreatePaymentResult, error) {
		gotCreate = r
		return connector.CreatePaymentResult{ProviderRef: "pr1", Status: connector.StatusPending,
			NextAction: paykit.NextAction{Kind: paykit.NextActionShowQR, QRData: "qr", FormFields: map[string]string{"a": "b"}},
			ExpiresAt:  t0, Raw: raw}, nil
	}
	f.OnCharge = func(_ context.Context, r connector.ChargeRequest) (connector.ChargeResult, error) {
		gotCharge = r
		return connector.ChargeResult{ProviderRef: "pr2", Status: connector.StatusSucceeded,
			AmountPaid: paykit.Money{Amount: 99, Currency: "VND"}, PaidAt: t0, Raw: raw}, nil
	}
	f.OnQuery = func(_ context.Context, r connector.QueryPaymentRequest) (connector.PaymentStatus, error) {
		gotQuery = r
		return connector.PaymentStatus{Found: true, Status: connector.StatusSucceeded, ProviderRef: "pr3",
			AmountPaid: paykit.Money{Amount: 5, Currency: "VND"}, PaidAt: t0, Card: card,
			ProviderCode: "00", ProviderMessage: "ok", Raw: raw}, nil
	}
	f.OnCancel = func(_ context.Context, r connector.CancelRequest) (connector.CancelResult, error) {
		gotCancel = r
		return connector.CancelResult{Status: connector.StatusCanceled, Raw: raw}, nil
	}
	f.OnRefund = func(_ context.Context, r connector.RefundRequest) (connector.RefundResult, error) {
		gotRefund = r
		return connector.RefundResult{ProviderRef: "rf1", Status: connector.StatusPending, Raw: raw}, nil
	}
	f.OnQueryRefund = func(_ context.Context, r connector.QueryRefundRequest) (connector.RefundStatus, error) {
		gotQR = r
		return connector.RefundStatus{Found: true, Status: connector.StatusSucceeded, ProviderRef: "rf1",
			ProviderCode: "00", ProviderMessage: "done", Raw: raw}, nil
	}
	ev := connector.ProviderEvent{EventID: "e1", Kind: connector.EventKindPayment, TransactionCode: "T1",
		ProviderRef: "pr1", Status: connector.StatusSucceeded, AmountPaid: paykit.Money{Amount: 7, Currency: "VND"},
		PaidAt: t0, Card: card, ProviderCode: "00", ProviderMessage: "ok", Raw: []byte{0xff, 0x00, 'x'}}
	f.OnParseWebhook = func(_ context.Context, r connector.RawWebhook) (connector.ProviderEvent, error) {
		gotRaw = append(gotRaw, r)
		return ev, nil
	}

	c := grpctest.Loopback(t, f)
	ctx := context.Background()
	money := paykit.Money{Amount: 1000, Currency: "VND"}

	creq := connector.CreatePaymentRequest{TransactionCode: "T1", IdempotencyKey: "k1", Money: money,
		Method: paykit.MethodQR, Customer: cust, ReturnURL: "https://r", CancelURL: "https://c", ClientIP: "1.2.3.4",
		Metadata: map[string]string{"m": "n"}, CreatedAt: tCreated}
	cres, err := c.CreatePayment(ctx, creq)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotCreate, creq) {
		t.Errorf("create req: %+v != %+v", gotCreate, creq)
	}
	wantC := connector.CreatePaymentResult{ProviderRef: "pr1", Status: connector.StatusPending,
		NextAction: paykit.NextAction{Kind: paykit.NextActionShowQR, QRData: "qr", FormFields: map[string]string{"a": "b"}},
		ExpiresAt:  t0, Raw: raw}
	if !reflect.DeepEqual(cres, wantC) {
		t.Errorf("create res: %+v", cres)
	}

	chreq := connector.ChargeRequest{TransactionCode: "T1", IdempotencyKey: "k2", Money: money, Method: paykit.MethodVisa,
		Customer: cust, ReturnURL: "r", CancelURL: "c", ClientIP: "ip", Metadata: map[string]string{"x": "y"},
		CreatedAt: tCreated}
	chres, err := c.Charge(ctx, chreq)
	if err != nil || !reflect.DeepEqual(gotCharge, chreq) {
		t.Fatalf("charge req: %+v err=%v", gotCharge, err)
	}
	wantCh := connector.ChargeResult{ProviderRef: "pr2", Status: connector.StatusSucceeded,
		AmountPaid: paykit.Money{Amount: 99, Currency: "VND"}, PaidAt: t0, Raw: raw}
	if !reflect.DeepEqual(chres, wantCh) {
		t.Errorf("charge res: %+v", chres)
	}

	qreq := connector.QueryPaymentRequest{TransactionCode: "T1", ProviderRef: "pr1", CreatedAt: tCreated}
	qres, err := c.QueryPayment(ctx, qreq)
	if err != nil || gotQuery != qreq {
		t.Fatalf("query req: %+v err=%v", gotQuery, err)
	}
	wantQ := connector.PaymentStatus{Found: true, Status: connector.StatusSucceeded, ProviderRef: "pr3",
		AmountPaid: paykit.Money{Amount: 5, Currency: "VND"}, PaidAt: t0, Card: card, ProviderCode: "00",
		ProviderMessage: "ok", Raw: raw}
	if !reflect.DeepEqual(qres, wantQ) {
		t.Errorf("query res: %+v", qres)
	}

	cnreq := connector.CancelRequest{TransactionCode: "T1", ProviderRef: "pr1", IdempotencyKey: "k3", CreatedAt: tCreated}
	cnres, err := c.CancelPayment(ctx, cnreq)
	if err != nil || gotCancel != cnreq || cnres.Status != connector.StatusCanceled || cnres.Raw != raw {
		t.Fatalf("cancel: req=%+v res=%+v err=%v", gotCancel, cnres, err)
	}

	rreq := connector.RefundRequest{RefundCode: "R1", IdempotencyKey: "k4", TransactionCode: "T1", ProviderRef: "pr1",
		Money: paykit.Money{Amount: 10, Currency: "VND"}, Reason: "why", CustomerID: "c1",
		TransactionCreatedAt: tCreated, CreatedAt: tCreated.Add(time.Minute)}
	rres, err := c.CreateRefund(ctx, rreq)
	if err != nil || gotRefund != rreq || rres.ProviderRef != "rf1" || rres.Status != connector.StatusPending || rres.Raw != raw {
		t.Fatalf("refund: req=%+v res=%+v err=%v", gotRefund, rres, err)
	}

	qrreq := connector.QueryRefundRequest{RefundCode: "R1", ProviderRef: "rf1", TransactionCode: "T1",
		CreatedAt: tCreated.Add(time.Minute), TransactionCreatedAt: tCreated}
	qrres, err := c.QueryRefund(ctx, qrreq)
	wantQR := connector.RefundStatus{Found: true, Status: connector.StatusSucceeded, ProviderRef: "rf1",
		ProviderCode: "00", ProviderMessage: "done", Raw: raw}
	if err != nil || gotQR != qrreq || qrres != wantQR {
		t.Fatalf("query refund: req=%+v res=%+v err=%v", gotQR, qrres, err)
	}

	// Webhook và redirect: byte không phải UTF-8 phải tới nguyên vẹn.
	rw := connector.RawWebhook{
		Headers:  map[string][]string{"X-Sig": {"a\xff", "b"}},
		Query:    map[string][]string{"q": {"1"}},
		Body:     []byte{0xff, 0x00, 'z'},
		RemoteIP: "9.9.9.9",
	}
	for name, parse := range map[string]func(context.Context, connector.RawWebhook) (connector.ProviderEvent, error){
		"webhook":  c.ParseWebhook,
		"redirect": c.ParseRedirect,
	} {
		got, err := parse(ctx, rw)
		if err != nil || !reflect.DeepEqual(got, ev) {
			t.Fatalf("%s: %+v err=%v", name, got, err)
		}
	}
	if len(gotRaw) != 2 || !reflect.DeepEqual(gotRaw[0], rw) || !reflect.DeepEqual(gotRaw[1], rw) {
		t.Errorf("raw webhook tới connector: %+v", gotRaw)
	}
}

func TestErrors_SentinelsSurviveWire(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want error
	}{
		{"signature", connector.ErrInvalidSignature, connector.ErrInvalidSignature},
		{"not_supported", connector.ErrNotSupported, connector.ErrNotSupported},
		{"unavailable", connector.ErrProviderUnavailable, connector.ErrProviderUnavailable},
		{"rejected", connector.ErrProviderRejected, connector.ErrProviderRejected},
		{"unknown", connector.ErrUnknownOutcome, connector.ErrUnknownOutcome},
		{"not_relevant", connector.ErrEventNotRelevant, connector.ErrEventNotRelevant},
		{"plain_is_unknown", errors.New("boom"), connector.ErrUnknownOutcome},
		{"rejected_code", &connector.RejectedError{Code: "51", Message: "no funds"}, connector.ErrProviderRejected},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			f.OnCreate = func(context.Context, connector.CreatePaymentRequest) (connector.CreatePaymentResult, error) {
				return connector.CreatePaymentResult{Raw: raw}, tc.err
			}
			f.OnParseWebhook = func(context.Context, connector.RawWebhook) (connector.ProviderEvent, error) {
				return connector.ProviderEvent{}, tc.err
			}
			c := grpctest.Loopback(t, f)

			res, err := c.CreatePayment(context.Background(), connector.CreatePaymentRequest{TransactionCode: "T"})
			if !errors.Is(err, tc.want) {
				t.Fatalf("create: %v", err)
			}
			if res.Raw != raw {
				t.Errorf("Raw phải về cả khi lỗi: %+v", res.Raw)
			}
			if _, err := c.ParseWebhook(context.Background(), connector.RawWebhook{}); !errors.Is(err, tc.want) {
				t.Fatalf("parse: %v", err)
			}
			var rej *connector.RejectedError
			if tc.name == "rejected_code" && (!errors.As(err, &rej) || rej.Code != "51") {
				t.Errorf("mất mã từ chối: %v", err)
			}
			// Lỗi nghiệp vụ không bao giờ thành ErrProviderUnavailable trừ khi connector nói rõ.
			if tc.want != connector.ErrProviderUnavailable && errors.Is(err, connector.ErrProviderUnavailable) {
				t.Errorf("suy ra unavailable: %v", err)
			}
		})
	}
}

func TestServer_MissingOperationIsNotSupported(t *testing.T) {
	// Connector chỉ cài phần bắt buộc: mọi thao tác tuỳ chọn phải là NOT_SUPPORTED qua thân phản hồi.
	srv := grpctest.New(t, connectorrpc.NewServer(minimal{}))
	c := grpctest.Client(t, srv, "min")
	ctx := context.Background()
	if _, err := c.Charge(ctx, connector.ChargeRequest{}); !errors.Is(err, connector.ErrNotSupported) {
		t.Errorf("charge: %v", err)
	}
	if _, err := c.ParseWebhook(ctx, connector.RawWebhook{}); !errors.Is(err, connector.ErrNotSupported) {
		t.Errorf("parse: %v", err)
	}
	if connector.Supports(c, connector.OpCharge) {
		t.Error("Describe không được khai charge")
	}
}

type minimal struct{}

func (minimal) Name() string             { return "min" }
func (minimal) Methods() []paykit.Method { return []paykit.Method{paykit.MethodQR} }
func (minimal) CreatePayment(context.Context, connector.CreatePaymentRequest) (connector.CreatePaymentResult, error) {
	return connector.CreatePaymentResult{Status: connector.StatusPending}, nil
}
