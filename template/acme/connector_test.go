package acme_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
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
	// Check thay vì Run để khẳng định thêm: chỉ luật của thao tác acme không khai bị bỏ.
	// Sao chép mẫu: bỏ hay thêm interface thì cập nhật notDeclared.
	rep := conformance.Check(t, h)
	for _, f := range rep.Findings {
		t.Errorf("%s: %s", f.Rule, f.Detail)
	}
	notDeclared := []string{"connector không khai thao tác charge", "connector không khai thao tác redirect"}
	for _, s := range rep.Skipped {
		_, reason, _ := strings.Cut(s, ": ")
		if !slices.Contains(notDeclared, reason) {
			t.Errorf("luật bị bỏ không vì thao tác chưa khai: %s", s)
		}
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

	blank := map[string]string{"ACME_BASE_URL": "https://a.example", "ACME_MERCHANT_ID": "M", "ACME_SECRET_KEY": " \n"}
	if _, err := acme.ConfigFromEnv(env(blank)); err == nil || !strings.Contains(err.Error(), "ACME_SECRET_KEY") {
		t.Errorf("secret chỉ có khoảng trắng: err = %v, cần nêu ACME_SECRET_KEY", err)
	}
	padded := map[string]string{"ACME_BASE_URL": " https://a.example\n", "ACME_MERCHANT_ID": "M", "ACME_SECRET_KEY": "s3cret\n"}
	if cfg, err := acme.ConfigFromEnv(env(padded)); err != nil || cfg.SecretKey != "s3cret" || cfg.BaseURL != "https://a.example" {
		t.Errorf("khoảng trắng thừa phải bị cắt: cfg=%+v err=%v", cfg, err)
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
		served <- connectorsdk.ServeListener(ctx, c, connectorsdk.Options{TLS: srvTLS, AllowedClients: []string{"core.test"}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}, lis)
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

// stub dựng một acme tối giản tự viết để test những phản hồi đối tác giả chuẩn không tạo ra.
// token và pay là handler của /oauth/token và /v1/payments; trả số request nghiệp vụ đã tới.
func stub(t *testing.T, token, pay http.HandlerFunc) (*acme.Connector, func() int) {
	t.Helper()
	var mu sync.Mutex
	hits := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth/token", token)
	mux.HandleFunc("/v1/", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		pay(w, r)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c, err := acme.New(acme.Config{BaseURL: srv.URL, MerchantID: "M", SecretKey: "s", Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return c, func() int { mu.Lock(); defer mu.Unlock(); return hits }
}

func goodToken(w http.ResponseWriter, _ *http.Request) {
	_, _ = w.Write([]byte(`{"access_token":"tok","expires_in":300}`))
}

func TestAcme_TokenFailuresAreUnavailableAndSendNothing(t *testing.T) {
	cases := map[string]struct {
		token http.HandlerFunc
		ctx   time.Duration
	}{
		"timeout": {func(_ http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body) // đọc hết thân để server phát hiện client bỏ đi
			<-r.Context().Done()
		}, 200 * time.Millisecond},
		"thân rác": {func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("<<rác>>")) }, 5 * time.Second},
		"5xx":      {func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) }, 5 * time.Second},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c, hits := stub(t, tc.token, func(http.ResponseWriter, *http.Request) {})
			ctx, cancel := context.WithTimeout(context.Background(), tc.ctx)
			defer cancel()
			_, err := c.CreatePayment(ctx, createReq("TTOK"))
			if !errors.Is(err, connector.ErrProviderUnavailable) || errors.Is(err, connector.ErrUnknownOutcome) {
				t.Fatalf("err = %v, cần ErrProviderUnavailable và không khớp ErrUnknownOutcome", err)
			}
			if hits() != 0 {
				t.Fatal("request nghiệp vụ không được gửi")
			}
		})
	}
}

func TestAcme_UnmappedClientErrorCodeIsUnknown(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		body   string
	}{
		"409 đang xử lý":      {409, `{"error":{"code":"processing","message":"đang xử lý"}}`},
		"429 giới hạn tốc độ": {429, `{"error":{"code":"rate_limited","message":"chậm lại"}}`},
		"4xx không có mã":     {400, `<html>bad gateway</html>`},
	} {
		t.Run(name, func(t *testing.T) {
			c, _ := stub(t, goodToken, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			_, err := c.CreatePayment(context.Background(), createReq("TUNMAPPED"))
			if !errors.Is(err, connector.ErrUnknownOutcome) || errors.Is(err, connector.ErrProviderRejected) {
				t.Fatalf("err = %v, cần ErrUnknownOutcome và không phải Rejected", err)
			}
		})
	}
}

func errorStub(t *testing.T, status int, code string) *acme.Connector {
	t.Helper()
	c, _ := stub(t, goodToken, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error":{"code":"` + code + `","message":"m"}}`))
	})
	return c
}

// already_paid khi TẠO nghĩa là acme đã thu tiền (lệnh trước bị thất lạc phản hồi): không bao giờ Rejected.
func TestAcme_AlreadyPaidMeaningDependsOnOperation(t *testing.T) {
	ctx := context.Background()
	_, err := errorStub(t, 409, "already_paid").CreatePayment(ctx, createReq("TAP"))
	if !errors.Is(err, connector.ErrUnknownOutcome) || errors.Is(err, connector.ErrProviderRejected) {
		t.Errorf("tạo: err = %v, cần ErrUnknownOutcome", err)
	}
	_, err = errorStub(t, 409, "already_paid").CreateRefund(ctx, connector.RefundRequest{RefundCode: "R", ProviderRef: "p", Money: amount})
	if !errors.Is(err, connector.ErrUnknownOutcome) || errors.Is(err, connector.ErrProviderRejected) {
		t.Errorf("hoàn: err = %v, cần ErrUnknownOutcome", err)
	}
	_, err = errorStub(t, 409, "already_paid").CancelPayment(ctx, connector.CancelRequest{ProviderRef: "p"})
	var rej *connector.RejectedError
	if !errors.As(err, &rej) || rej.Code != "already_paid" {
		t.Errorf("huỷ: err = %v, cần RejectedError already_paid", err)
	}
}

// Thiếu ProviderRef thì acme không huỷ được (chỉ huỷ theo payment_id): trả lời dứt khoát cannot_cancel,
// không gọi mạng. Core sẽ truy vấn lại rồi mới quyết hết hạn hay không.
func TestAcme_CancelWithoutProviderRefIsRejectedWithoutCall(t *testing.T) {
	c, hits := stub(t, goodToken, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) })
	_, err := c.CancelPayment(context.Background(), connector.CancelRequest{TransactionCode: "TNOREF", IdempotencyKey: "k"})
	var rej *connector.RejectedError
	if !errors.As(err, &rej) || rej.Code != "cannot_cancel" || errors.Is(err, connector.ErrProviderUnavailable) || errors.Is(err, connector.ErrUnknownOutcome) {
		t.Fatalf("err = %v, cần đúng RejectedError{Code: cannot_cancel}", err)
	}
	if hits() != 0 {
		t.Fatal("không được gọi acme khi thiếu ProviderRef")
	}
}

// Hoàn tiền: not_found (giao dịch gốc không có ở acme) và declined là từ chối cuối cùng, chưa hoàn gì.
func TestAcme_RefundRejectCodes(t *testing.T) {
	for _, tc := range []struct {
		status int
		code   string
	}{{404, "not_found"}, {422, "declined"}} {
		_, err := errorStub(t, tc.status, tc.code).CreateRefund(context.Background(),
			connector.RefundRequest{RefundCode: "R", ProviderRef: "p", Money: amount})
		var rej *connector.RejectedError
		if !errors.As(err, &rej) || rej.Code != tc.code || errors.Is(err, connector.ErrUnknownOutcome) {
			t.Errorf("%s: err = %v, cần RejectedError{Code: %s}", tc.code, err, tc.code)
		}
	}
}

func TestAcme_QueryNotFoundByCodeAnyHTTPStatus(t *testing.T) {
	ctx := context.Background()
	st, err := errorStub(t, 400, "not_found").QueryPayment(ctx, connector.QueryPaymentRequest{TransactionCode: "X"})
	if err != nil || st.Found {
		t.Errorf("QueryPayment: found=%v err=%v, cần Found=false không lỗi", st.Found, err)
	}
	rs, err := errorStub(t, 400, "not_found").QueryRefund(ctx, connector.QueryRefundRequest{RefundCode: "X"})
	if err != nil || rs.Found {
		t.Errorf("QueryRefund: found=%v err=%v, cần Found=false không lỗi", rs.Found, err)
	}
	_, err = errorStub(t, 400, "declined").QueryPayment(ctx, connector.QueryPaymentRequest{TransactionCode: "X"})
	if !errors.Is(err, connector.ErrUnknownOutcome) || errors.Is(err, connector.ErrProviderRejected) {
		t.Errorf("truy vấn với mã khác: err = %v, cần ErrUnknownOutcome", err)
	}
}

func TestAcme_MismatchedOrderCodeIsUnknown(t *testing.T) {
	c, _ := stub(t, goodToken, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"payment_id":"p1","order_code":"KHAC","status":"pending","redirect_url":"https://pay.acme.example/c/p1"}`))
	})
	_, err := c.CreatePayment(context.Background(), createReq("TMISMATCH"))
	if !errors.Is(err, connector.ErrUnknownOutcome) {
		t.Fatalf("err = %v, cần ErrUnknownOutcome", err)
	}
}

func TestAcme_MalformedSignatureIsInvalid(t *testing.T) {
	c, p := newPair(t)
	raw := p.SignedWebhook("TSIG", "ref", connector.StatusSucceeded, amount)
	for _, sig := range []string{"", "không-phải-hex", "00"} {
		raw.Headers = map[string][]string{"X-Acme-Signature": {sig}}
		if _, err := c.ParseWebhook(context.Background(), raw); !errors.Is(err, connector.ErrInvalidSignature) {
			t.Errorf("chữ ký %q: err = %v, cần ErrInvalidSignature", sig, err)
		}
	}
}

func TestAcme_AmbiguousSignatureHeaderIsInvalid(t *testing.T) {
	c, p := newPair(t)
	raw := p.SignedWebhook("TDUP", "ref", connector.StatusSucceeded, amount)
	good := raw.Headers["X-Acme-Signature"][0]
	raw.Headers = map[string][]string{"X-Acme-Signature": {good}, "x-acme-signature": {good}}
	if _, err := c.ParseWebhook(context.Background(), raw); !errors.Is(err, connector.ErrInvalidSignature) {
		t.Fatalf("err = %v, cần ErrInvalidSignature khi có hai header chữ ký", err)
	}
	raw.Headers = map[string][]string{"x-acme-signature": {good}} // một header, khác hoa/thường: hợp lệ
	if _, err := c.ParseWebhook(context.Background(), raw); err != nil {
		t.Fatalf("header chữ ký viết thường phải hợp lệ: %v", err)
	}
}
