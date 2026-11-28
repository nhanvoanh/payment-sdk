package conformance_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/nhanvoanh/payment-sdk/conformance"
	"github.com/nhanvoanh/payment-sdk/connector"
	"github.com/nhanvoanh/payment-sdk/connectortest"
	"github.com/nhanvoanh/payment-sdk/paykit"
)

// getPartner là đối tác gửi thông báo bằng HTTP GET: dữ liệu chỉ nằm trên query, chữ ký tính trên query thô (phần
// trước "&sig="), thân rỗng. Cài Tamperer: đổi trạng thái trên query, giữ nguyên chữ ký.
type getPartner struct{ *fakePartner }

func signQuery(q string) string {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(q))
	return hex.EncodeToString(m.Sum(nil))
}

func getWebhook(rawQuery string) connector.RawWebhook {
	q, _ := url.ParseQuery(rawQuery)
	return connector.RawWebhook{Query: q, RawQuery: rawQuery, RemoteIP: "203.0.113.7"}
}

func (getPartner) SignedWebhook(tx, ref string, st connector.Status, m paykit.Money) connector.RawWebhook {
	// Thứ tự cố định và "%20" thay cho "+": query thô khác bản url.Values.Encode, như đối tác thật hay làm.
	data := "tx=" + url.PathEscape(tx) + "&ref=" + url.PathEscape(ref) + "&status=" + string(st) +
		"&currency=" + m.Currency + "&amount=" + strconv.FormatInt(m.Amount, 10)
	return getWebhook(data + "&sig=" + signQuery(data))
}

func (getPartner) TamperedWebhook(raw connector.RawWebhook) connector.RawWebhook {
	data, sig, _ := strings.Cut(raw.RawQuery, "&sig=")
	data = strings.Replace(data, "&status="+string(connector.StatusSucceeded), "&status="+string(connector.StatusFailed), 1)
	return getWebhook(data + "&sig=" + sig)
}

// parseGET là ParseWebhook của connector viết đúng cho getPartner: query thô rỗng hay chữ ký sai là
// ErrInvalidSignature; verify=false bỏ qua kiểm chữ ký (connector sai).
func parseGET(verify bool) func(context.Context, connector.RawWebhook) (connector.ProviderEvent, error) {
	return func(_ context.Context, raw connector.RawWebhook) (connector.ProviderEvent, error) {
		data, sig, ok := strings.Cut(raw.RawQuery, "&sig=")
		if verify && (raw.RawQuery == "" || !ok || !hmac.Equal([]byte(sig), []byte(signQuery(data)))) {
			return connector.ProviderEvent{}, connector.ErrInvalidSignature
		}
		q, err := url.ParseQuery(data)
		if err != nil || q.Get("tx") == "" {
			return connector.ProviderEvent{}, connector.ErrInvalidSignature
		}
		amt, err := strconv.ParseInt(q.Get("amount"), 10, 64)
		if err != nil {
			return connector.ProviderEvent{}, connector.ErrInvalidSignature
		}
		return connector.ProviderEvent{
			EventID: "evt_" + q.Get("tx") + "_" + q.Get("status"), Kind: connector.EventKindPayment,
			TransactionCode: q.Get("tx"), ProviderRef: q.Get("ref"), Status: connector.Status(q.Get("status")),
			AmountPaid: paykit.Money{Amount: amt, Currency: q.Get("currency")}, Raw: []byte(raw.RawQuery),
		}, nil
	}
}

func getHarness(verify bool) conformance.Harness {
	h := harness(func(f *connectortest.Fake, _ *fakePartner) connector.Connector {
		f.OnParseWebhook = parseGET(verify)
		return f
	})
	inner := h.New
	h.New = func(tb testing.TB) (connector.Connector, conformance.Partner) {
		c, p := inner(tb)
		return c, getPartner{p.(*fakePartner)}
	}
	return h
}

// Webhook dạng GET (dữ liệu và chữ ký chỉ trên query thô) phải qua WHK-1..4 ở cả lượt gọi thẳng lẫn /grpc:
// bộ luật không được làm rơi RawQuery khi sao chép webhook trước khi đưa cho connector.
func TestCheck_GETWebhookSignedOverRawQueryPasses(t *testing.T) {
	t.Parallel()
	r := conformance.Check(t, getHarness(true))
	if len(r.Findings) != 0 {
		t.Fatalf("connector GET viết đúng không được có finding:%s", dump(r))
	}
	expectOnlyGRPCSkips(t, r)
}

// WHK-4 vẫn bắt connector GET không kiểm chữ ký: webhook bị sửa trên query phải bị phát hiện ở cả hai lượt.
func TestCheck_Catches_GETWebhookTamperedAccepted(t *testing.T) {
	t.Parallel()
	r := conformance.Check(t, getHarness(false))
	expectBoth(t, r, "WHK-4")
}
