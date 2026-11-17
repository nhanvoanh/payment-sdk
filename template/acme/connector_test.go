package acme_test

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"bitbucket.org/hasaki-tech/payment-connector-sdk/conformance"
	"bitbucket.org/hasaki-tech/payment-connector-sdk/connector"
	"bitbucket.org/hasaki-tech/payment-connector-sdk/connectorrpc"
	"bitbucket.org/hasaki-tech/payment-connector-sdk/connectorrpc/tlstest"
	"bitbucket.org/hasaki-tech/payment-connector-sdk/connectorsdk"
	"bitbucket.org/hasaki-tech/payment-connector-sdk/paykit"
	"bitbucket.org/hasaki-tech/payment-connector-sdk/template/acme"
	"bitbucket.org/hasaki-tech/payment-connector-sdk/template/acme/acmetest"
)

var amount = paykit.Money{Amount: 150000, Currency: "VND"}

// newPair dựng connector trỏ vào đối tác giả mới; mỗi test một cặp riêng.
func newPair(tb testing.TB) (*acme.Connector, *acmetest.Partner) {
	tb.Helper()
	p := acmetest.NewPartner(tb)
	c, err := acme.New(acme.Config{BaseURL: p.URL(), MerchantID: p.MerchantID(), SecretKey: p.Secret(), Timeout: 5 * time.Second})
	if err != nil {
		tb.Fatal(err)
	}
	return c, p
}

func createReq(tx string) connector.CreatePaymentRequest {
	return connector.CreatePaymentRequest{
		TransactionCode: tx, IdempotencyKey: "idem-" + tx, Money: amount, Method: paykit.MethodWallet,
		ReturnURL: "https://shop.example/return", CancelURL: "https://shop.example/cancel",
	}
}

func TestAcme_Conformance(t *testing.T) {
	h := conformance.Harness{
		New: func(tb testing.TB) (connector.Connector, conformance.Partner) {
			c, p := newPair(tb)
			return c, p
		},
		Method:             paykit.MethodWallet,
		Amount:             amount,
		CreateCallsPartner: true,
	}
	// Check thay vì Run để khẳng định thêm: không luật nào bị bỏ. Acme không cài Charger và Redirecter
	// và conformance không có luật riêng cho hai thao tác đó, nên mọi luật đều chạy.
	rep := conformance.Check(t, h)
	for _, f := range rep.Findings {
		t.Errorf("%s: %s", f.Rule, f.Detail)
	}
	for _, s := range rep.Skipped {
		t.Errorf("luật bị bỏ: %s", s)
	}
}

func TestAcme_TokenFetchedOnceUnderLoad(t *testing.T) {
	c, p := newPair(t)
	var wg sync.WaitGroup
	errs := make(chan error, 50)
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := c.CreatePayment(context.Background(), createReq("LOAD"+string(rune('A'+i%26))+string(rune('a'+i/26))))
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
	}
	if n := p.TokenFetches(); n != 1 {
		t.Fatalf("lấy token %d lần, cần đúng 1", n)
	}
	if n := p.PaymentsCreated(); n != 50 {
		t.Fatalf("đối tác tạo %d thanh toán, cần 50", n)
	}
}

func TestAcme_401RefreshesTokenOnce(t *testing.T) {
	c, p := newPair(t)
	if _, err := c.CreatePayment(context.Background(), createReq("T401A")); err != nil {
		t.Fatal(err)
	}
	p.RevokeTokens() // token đang giữ trở thành vô hiệu phía đối tác
	res, err := c.CreatePayment(context.Background(), createReq("T401B"))
	if err != nil {
		t.Fatalf("Create sau khi token bị thu hồi: %v", err)
	}
	if res.ProviderRef == "" {
		t.Fatal("thiếu ProviderRef")
	}
	if n := p.TokenFetches(); n != 2 {
		t.Fatalf("lấy token %d lần, cần 2 (đầu và sau 401)", n)
	}
	if n, m := p.PaymentAttempts(), p.PaymentsCreated(); n != 3 || m != 2 {
		t.Fatalf("attempts=%d created=%d, cần 3 và 2 (401 không tạo gì, thử lại đúng một lần)", n, m)
	}
}

func TestAcme_Persistent401StopsAfterOneRetry(t *testing.T) {
	c, p := newPair(t)
	p.RejectAuth(true)
	_, err := c.CreatePayment(context.Background(), createReq("T401C"))
	if !errors.Is(err, connector.ErrProviderUnavailable) || errors.Is(err, connector.ErrUnknownOutcome) {
		t.Fatalf("err = %v, cần ErrProviderUnavailable (401 là từ chối trước khi xử lý)", err)
	}
	if n := p.PaymentAttempts(); n != 2 {
		t.Fatalf("gửi %d lần, cần đúng 2 (một lần thử lại)", n)
	}
	if p.PaymentsCreated() != 0 {
		t.Fatal("đối tác không được tạo gì")
	}
}

func TestAcme_TokenFailureIsUnavailable(t *testing.T) {
	c, p := newPair(t)
	p.FailTokens(true)
	_, err := c.CreatePayment(context.Background(), createReq("TTOKEN"))
	if !errors.Is(err, connector.ErrProviderUnavailable) {
		t.Fatalf("err = %v, cần ErrProviderUnavailable", err)
	}
	// Nguyên nhân (5xx của /oauth/token) không được kéo theo ErrUnknownOutcome: core xét nó trước.
	if errors.Is(err, connector.ErrUnknownOutcome) {
		t.Fatalf("err khớp cả ErrUnknownOutcome: %v", err)
	}
	if p.PaymentAttempts() != 0 {
		t.Fatal("request nghiệp vụ không được gửi khi chưa có token")
	}
}

func TestAcme_TimeoutOnCreateIsUnknown(t *testing.T) {
	c, p := newPair(t)
	p.SetMode(conformance.ModeHang)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := c.CreatePayment(ctx, createReq("TTIMEOUT"))
	if !errors.Is(err, connector.ErrUnknownOutcome) {
		t.Fatalf("err = %v, cần ErrUnknownOutcome", err)
	}
	if errors.Is(err, connector.ErrProviderUnavailable) || errors.Is(err, connector.ErrProviderRejected) {
		t.Fatalf("err không được khớp unavailable/rejected: %v", err)
	}
}

func TestAcme_5xxIsUnknown(t *testing.T) {
	c, p := newPair(t)
	p.Fail5xx(true)
	_, err := c.CreatePayment(context.Background(), createReq("T5XX"))
	if !errors.Is(err, connector.ErrUnknownOutcome) || errors.Is(err, connector.ErrProviderUnavailable) {
		t.Fatalf("err = %v, cần ErrUnknownOutcome và không phải unavailable", err)
	}
}

func TestAcme_RejectCarriesPartnerCode(t *testing.T) {
	c, p := newPair(t)
	p.SetMode(conformance.ModeReject)
	_, err := c.CreatePayment(context.Background(), createReq("TREJECT"))
	var rej *connector.RejectedError
	if !errors.As(err, &rej) || rej.Code != "declined" || rej.Message == "" {
		t.Fatalf("err = %v, cần RejectedError{Code: declined}", err)
	}
}

func TestAcme_UnknownWebhookTypeIsNotRelevant(t *testing.T) {
	c, p := newPair(t)
	raw := p.SignedWebhookOfType("payment.reminder", "TX1", "ref", connector.StatusPending, paykit.Money{})
	_, err := c.ParseWebhook(context.Background(), raw)
	if !errors.Is(err, connector.ErrEventNotRelevant) {
		t.Fatalf("err = %v, cần ErrEventNotRelevant", err)
	}
}

func TestConfigFromEnv(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	full := map[string]string{
		"ACME_BASE_URL": "https://api.acme.example", "ACME_MERCHANT_ID": "M1", "ACME_SECRET_KEY": "s3cret", "ACME_TIMEOUT": "7s",
	}
	cfg, err := acme.ConfigFromEnv(env(full))
	if err != nil {
		t.Fatal(err)
	}
	want := acme.Config{BaseURL: "https://api.acme.example", MerchantID: "M1", SecretKey: "s3cret", Timeout: 7 * time.Second}
	if cfg != want {
		t.Fatalf("cfg = %+v, cần %+v", cfg, want)
	}

	for _, name := range []string{"ACME_BASE_URL", "ACME_MERCHANT_ID", "ACME_SECRET_KEY"} {
		m := map[string]string{}
		for k, v := range full {
			if k != name {
				m[k] = v
			}
		}
		if _, err := acme.ConfigFromEnv(env(m)); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("thiếu %s: err = %v, cần nêu tên biến", name, err)
		}
	}

	bad := map[string]string{"ACME_BASE_URL": "x", "ACME_MERCHANT_ID": "M", "ACME_SECRET_KEY": "s", "ACME_TIMEOUT": "mười giây"}
	if _, err := acme.ConfigFromEnv(env(bad)); err == nil || !strings.Contains(err.Error(), "ACME_TIMEOUT") {
		t.Errorf("ACME_TIMEOUT sai: err = %v, cần nêu tên biến", err)
	}
	m := map[string]string{"ACME_BASE_URL": "https://a.example", "ACME_MERCHANT_ID": "M", "ACME_SECRET_KEY": "s"}
	if cfg, err := acme.ConfigFromEnv(env(m)); err != nil || cfg.Timeout != 0 {
		t.Errorf("không đặt ACME_TIMEOUT: cfg=%+v err=%v, cần không lỗi (New áp mặc định)", cfg, err)
	}
}

func TestNew_RejectsUnsafeConfig(t *testing.T) {
	ok := acme.Config{BaseURL: "https://api.acme.example", MerchantID: "M", SecretKey: "s"}
	if _, err := acme.New(ok); err != nil {
		t.Fatalf("cấu hình hợp lệ bị từ chối: %v", err)
	}
	for name, mutate := range map[string]func(*acme.Config){
		"không có merchant":       func(c *acme.Config) { c.MerchantID = "" },
		"không có secret":         func(c *acme.Config) { c.SecretKey = "" },
		"URL rác":                 func(c *acme.Config) { c.BaseURL = "://x" },
		"http tới host công khai": func(c *acme.Config) { c.BaseURL = "http://api.acme.example" },
		"scheme lạ":               func(c *acme.Config) { c.BaseURL = "ftp://api.acme.example" },
	} {
		cfg := ok
		mutate(&cfg)
		if _, err := acme.New(cfg); err == nil {
			t.Errorf("%s: cần lỗi", name)
		}
	}
}

// TestAcme_OverMTLS chạy connector như cmd/connector: connectorsdk.ServeListener với mTLS thật trên TCP,
// core giả gọi bằng connectorrpc.NewClient. Đi trọn: Describe, tạo thanh toán, nhận webhook.
func TestAcme_OverMTLS(t *testing.T) {
	c, p := newPair(t)
	b := tlstest.New(t)
	srvTLS, err := connectorrpc.ServerTLS(b.Server("connector.test"))
	if err != nil {
		t.Fatal(err)
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() {
		served <- connectorsdk.ServeListener(ctx, c, connectorsdk.Options{TLS: srvTLS, AllowedClients: []string{"core.test"}}, lis)
	}()
	t.Cleanup(func() {
		cancel()
		if err := <-served; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})

	cliTLS, err := connectorrpc.ClientTLS(b.Client("core.test"), "connector.test")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(credentials.NewTLS(cliTLS)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	client := connectorrpc.NewClient("acme", conn, connectorrpc.ClientOptions{})

	cctx, ccancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer ccancel()
	if _, err := client.Describe(cctx); err != nil {
		t.Fatalf("Describe: %v", err)
	}
	res, err := client.CreatePayment(cctx, createReq("TMTLS"))
	if err != nil {
		t.Fatalf("CreatePayment: %v", err)
	}
	if res.ProviderRef == "" || res.NextAction.Kind != paykit.NextActionRedirect || res.NextAction.URL == "" {
		t.Fatalf("kết quả tạo thanh toán lạ: %+v", res)
	}
	ev, err := client.ParseWebhook(cctx, p.SignedWebhook("TMTLS", res.ProviderRef, connector.StatusSucceeded, amount))
	if err != nil {
		t.Fatalf("ParseWebhook: %v", err)
	}
	if ev.TransactionCode != "TMTLS" || ev.Status != connector.StatusSucceeded || ev.EventID == "" {
		t.Fatalf("sự kiện lạ: %+v", ev)
	}
}
