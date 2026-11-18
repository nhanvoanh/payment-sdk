# conformance

Bộ luật mọi connector phải qua trước khi được nối vào core. Nó là thứ đứng giữa một connector viết sai
và tiền của khách: ví dụ connector báo "đối tác chắc chắn chưa xử lý" cho một lần timeout thì core chốt
giao dịch thất bại trong khi đối tác có thể đã trừ tiền, và khách trả tiền hai lần.

Mỗi luật chạy **hai lượt**:

- gọi thẳng connector (mã luật như bảng dưới, ví dụ `PAY-4`);
- gọi qua gRPC như core thật sẽ gọi: connector được phục vụ bằng `connectorrpc.NewServer` trên bufconn và
  gọi bằng proxy `connectorrpc.Client` (mã có hậu tố `/grpc`, ví dụ `PAY-4/grpc`). Panic phía server được
  bắt và báo thành vi phạm của luật đang chạy.

Mỗi lượt dùng một cặp connector/đối tác mới (`Harness.New`), có hạn thời gian riêng và chạy connector
trong goroutine có bắt panic: connector treo hay panic thành vi phạm của đúng luật đó, không làm treo
hay sập cả bộ. Luật thuộc thao tác connector không khai thì bị bỏ (`Skipped`) kèm lý do, không tính là
vi phạm. Nếu `Describe` qua gRPC hỏng thì `ID-1/grpc` báo lỗi và các lượt `/grpc` khác bị bỏ. Bước chuẩn
bị của một luật (tạo giao dịch ở `ModeNormal`) mà hỏng thì luật đó bị bỏ với lý do "xem PAY-1", không sinh
vi phạm trùng.

### Core hiểu lỗi thế nào

Các luật xét lỗi bằng `connector.Classify`, chính hàm core dùng (một nguồn duy nhất): `ErrUnknownOutcome` trước, rồi `ErrProviderRejected`,
rồi `ErrProviderUnavailable` **và `ErrNotSupported`** (core coi cả hai là "chắc chắn chưa xử lý" và chốt
thất bại), mọi lỗi khác là chưa rõ. Vì vậy:

- "chưa rõ theo core" = lỗi khớp `ErrUnknownOutcome`, hoặc không khớp cái nào trong `ErrProviderRejected`,
  `ErrProviderUnavailable`, `ErrNotSupported`. Đây là đòi hỏi của mọi luật đối tác treo/trả rác.
- Đối tác từ chối kết nối (`ModeDown`) thì lỗi phải khớp `ErrProviderUnavailable` và không khớp
  `ErrUnknownOutcome` hay `ErrProviderRejected`; từ chối rõ ràng (`ModeReject`) thì khớp
  `ErrProviderRejected` và không khớp `ErrUnknownOutcome`. Lỗi bọc thêm `ErrUnknownOutcome` bị core coi là
  chưa rõ, nên không đạt.

Ở lượt `/grpc` của phần "đối tác treo", proxy có hạn 300 ms + 2 s còn server bọc ctx của handler bằng hạn
300 ms, nên câu trả lời của connector (đúng hay sai) luôn tới được proxy: kết quả tất định.

`Report` tách hai loại "không chạy":

- `Skipped`: lý do nằm ở phía developer — thao tác không khai, `CreateCallsPartner` sai, bước chuẩn bị hỏng,
  `Describe` qua gRPC hỏng. Nên đọc; có thể phải xử lý.
- `NotChecked`: lượt mà bộ luật theo thiết kế không kiểm được, không bao giờ là việc của developer. Hiện có
  `WHK-6/grpc` (proxy trả ack từ bảng lấy một lần lúc Describe) và `CTX-1/grpc` (proxy gặp ctx đã huỷ thì
  trả về ngay, không gửi). `Run` chỉ `t.Log` các mục này, không tạo subtest.

## Luật

Mã luật cố định, là hợp đồng với developer connector.

| Mã | Nội dung | Vì sao (hậu quả về tiền) |
|---|---|---|
| `ID-1` | `Name()` khớp `^[a-z][a-z0-9_]{0,31}$` và không đổi giữa hai lần gọi; qua gRPC: `Describe` thành công. | Tên là khoá cấu hình và khoá khử trùng webhook; tên đổi thì một sự kiện có thể bị áp hai lần, tên sai thì core không nạp được connector. |
| `ID-2` | `Methods()` không rỗng, không trùng, có `Harness.Method`. | Core định tuyến giao dịch theo phương thức; thiếu thì khách không trả tiền được qua đối tác này. |
| `ID-3` | `Supports(op)` đúng ⇒ connector cài interface tương ứng (`connector.As…` đúng). | Khai mà không cài thì lệnh huỷ/hoàn tiền nhận `ErrNotSupported` lúc chạy. |
| `ID-4` | (cần `Harness.CreateCallsPartner` và `Partner` cài `RequestRecorder`; không cài thì `Skipped`) `CreatePayment` hai lần với CÙNG request, lần hai sau khi đồng hồ qua một ranh giới giây → mã đơn đối tác giả ghi nhận giống nhau; và mã không mang ngày hôm nay (`yyyymmdd`/`yymmdd` ở UTC hay UTC+7) khi `CreatedAt` ở ngày khác. | Connector phải dựng mã phía đối tác TẤT ĐỊNH từ `TransactionCode`/`RefundCode` + `CreatedAt` (core cấp, giá trị đã lưu, UTC, micro giây, như nhau ở mọi lần gọi lại), không từ đồng hồ lúc gọi. Không thì truy vấn/huỷ một lệnh tạo chưa rõ kết quả (chưa có `ProviderRef`) hỏi sai mã, đối tác báo không thấy và core cho hết hạn một đơn khách đã trả. |
| `PAY-1` | Create ở `ModeNormal`: không lỗi; `Status` thuộc pending, succeeded, failed, canceled, on_hold; `NextAction` redirect có URL tuyệt đối http(s), show_qr có `QRData`; `NextAction.Kind` ngoài none/redirect/show_qr/rỗng bị từ chối. | Khách không tới được trang thanh toán hoặc core không biết đã thu tiền chưa. |
| `PAY-2` | `ProviderRef` là UTF-8 hợp lệ, không ký tự điều khiển, tối đa 255 ký tự. | Dài hơn thì core cắt và gửi bản bị cắt cho đối tác ở lệnh truy vấn/hoàn sau: đối tác không nhận ra giao dịch. |
| `PAY-3` | `ModeDown` (từ chối kết nối) → lỗi khớp `ErrProviderUnavailable`, không khớp `ErrUnknownOutcome`/`ErrProviderRejected`. | Chỉ lỗi này cho core chốt thất bại ngay để khách tạo lại thanh toán; sai thì giao dịch treo. |
| `PAY-4` | `ModeHang`, ctx hạn 300 ms → trả về trong 300 ms + 2 s; lỗi "chưa rõ theo core" (xem trên; `ErrNotSupported` cũng không đạt). | Timeout là không rõ kết quả; báo "chưa xử lý" hay "bị từ chối" thì core chốt thất bại trong khi đối tác có thể đã trừ tiền: khách trả hai lần. |
| `PAY-5` | `ModeReject` → lỗi khớp `ErrProviderRejected` (nên dùng `RejectedError`), không khớp `ErrUnknownOutcome`. | Lời từ chối rõ ràng mà báo khác thì giao dịch treo chờ truy vấn thay vì báo khách ngay. |
| `PAY-6` | `ModeGarbage` (200 với thân không hiểu được) → lỗi "chưa rõ theo core". | Đối tác đã trả lời, có thể đã nhận tiền; chốt thất bại thì khách trả hai lần. |
| `PAY-7` | Create hai lần cùng `TransactionCode` và `IdempotencyKey` → không panic. | Core gửi lại cùng khoá khi không rõ kết quả; panic thì giao dịch kẹt. |
| `QRY-1` | (có `query_payment`) giao dịch đối tác không biết → `Found == false`, không lỗi. | Core truy vấn cả giao dịch chưa tới được đối tác; báo lỗi thì không bao giờ đóng được giao dịch đó. |
| `QRY-2` | Sau `SetPaymentStatus(succeeded, amount)` → `Found`, `Status == succeeded`, `AmountPaid` bằng số đã đặt hoặc 0; `Currency` rỗng hoặc ba chữ hoa. | Khách đã trả mà đơn không được xác nhận, hoặc giao dịch rơi vào lệch tiền. |
| `QRY-3` | (có `query_payment`) `ModeHang` → như `PAY-4`. | Như `PAY-4`. |
| `QRY-4` | (có `query_payment`) `ModeGarbage` → có lỗi (lỗi nào cũng được); `err == nil` là vi phạm dù `Found` là gì. | Với truy vấn, core coi mọi lỗi như nhau (truy vấn lại sau); err nil thì core tin kết quả bịa: `failed` khiến khách tạo lại thanh toán trong khi đối tác có thể đã thu, "không thấy" khiến giao dịch bị cho hết hạn. |
| `CHG-1` | (có `charge`) `ModeDown` → như `PAY-3`; `ModeHang` → như `PAY-4`; `ModeReject` → như `PAY-5`; `ModeGarbage` → như `PAY-6`. | Charge thu tiền trực tiếp: báo "chắc chắn chưa xử lý" cho một lần treo thì core chốt thất bại trong khi thẻ của khách đã bị trừ. |
| `CXL-1` | (có `cancel`) sau `SetPaymentStatus(succeeded)`, huỷ **không** được trả `StatusCanceled` kèm `err == nil`. | Core huỷ đơn trong khi khách đã bị trừ tiền. |
| `CXL-2` | (có `cancel`) `ModeHang` và `ModeGarbage` → câu trả lời hợp lệ **duy nhất** là lỗi core hiểu là "chưa rõ" (`ErrUnknownOutcome`). `err == nil` kèm trạng thái bất kỳ (kể cả `StatusPending`) là vi phạm, như `RejectedError` hay `ErrProviderUnavailable`. Lượt thêm (cần `Partner` cài `RequestCounter`, không cài thì lượt này vào `Skipped`): huỷ với `ProviderRef` rỗng ở `ModeHang`; bộ đếm được chụp ngay trước lời huỷ đó. `err == nil` kèm `StatusCanceled` luôn là vi phạm. Không request nào tới đối tác → quyết định tại chỗ: `RejectedError`, `ErrNotSupported`, `ErrProviderUnavailable` (chưa gửi gì), `nil` kèm trạng thái khác canceled đều được chấp nhận ở lượt này (ví dụ connector chỉ huỷ theo `ProviderRef`, xem `CXL-3`; riêng `ErrProviderUnavailable` vẫn bị `CXL-3` từ chối ở `ModeNormal`). Có request tới đối tác (kể cả request tới trễ tối đa 200 ms sau khi connector trả về) → như lượt có mã: chỉ "chưa rõ" hợp lệ, kể cả khi connector tự đặt timeout ngắn hơn hạn của luật. | Không ai biết đối tác đã huỷ chưa. Báo "đã huỷ" thì core huỷ đơn trong khi đối tác có thể vẫn thu tiền. Báo "bị từ chối", hoặc `nil` kèm trạng thái khác canceled (core xếp nó chung với "bị từ chối"), thì core tin đối tác nói rõ không huỷ và quyết theo một câu trả lời không có. Connector huỷ theo mã giao dịch khi thiếu `ProviderRef` hay mắc lỗi này. |
| `CXL-3` | (có `cancel`) huỷ một giao dịch đối tác **không** biết, `ProviderRef` rỗng, ở `ModeNormal` → câu trả lời dứt khoát theo core: `err == nil` (trạng thái bất kỳ), lỗi core hiểu là từ chối (nên dùng `RejectedError`, ví dụ mã `cannot_cancel`), hoặc `ErrNotSupported`. Vi phạm: lỗi core hiểu là chưa rõ, hoặc `ErrProviderUnavailable`. | Core huỷ cả giao dịch mà Create chưa nghe được trả lời (chưa có `ProviderRef`). Trả "chưa rõ" hay "không gọi được" thì core không bao giờ cho giao dịch quá hạn hết hạn: đơn kẹt tới khi có người chốt tay, và mỗi lượt quét làm cầu chì đếm thêm một lỗi. `ErrProviderUnavailable` chỉ dành cho khi đã cố gọi đối tác mà không tới. |
| `CXL-4` | (có `cancel` và `query_payment`) sau `SetPaymentStatus(succeeded)`, huỷ hợp lệ **chỉ khi** trả `err == nil` kèm `StatusSucceeded` hoặc `StatusOnHold` ("đã thu"), hoặc lỗi core hiểu là "chưa rõ" (`ErrUnknownOutcome`). Vi phạm: `err == nil` kèm trạng thái khác (`pending`, `failed`…; core xếp nó chung với "bị từ chối"), `RejectedError`, lỗi core hiểu là chắc chắn chưa xử lý (`ErrProviderUnavailable`, `ErrNotSupported`); `canceled` thuộc `CXL-1`. | Bị từ chối (hoặc `nil` kèm trạng thái khác) thì core truy vấn lại MỘT lần và quyết theo nó: truy vấn còn chậm mà báo không thấy giao dịch (và lần truy vấn đầu cũng không thấy) thì core cho giao dịch **hết hạn** dù đối tác đã thu — đơn mở lại, khách có thể trả hai lần, khoản đã thu chỉ được gắn cờ trả muộn nếu đối tác gửi webhook; truy vấn còn báo chờ thì giao dịch nằm ở `processing` (`cancel_refused_pending`). `ErrNotSupported` thì core coi như không huỷ được và cũng có thể cho hết hạn khi truy vấn không thấy. "Đã thu" thì core không bao giờ cho hết hạn ở lượt đó (`cancel_refused_collected`), lượt sau truy vấn chốt. Đối tác không phân biệt được "đã thu" khi huỷ thì connector nên truy vấn xác nhận rồi mới trả lời, hoặc trả "chưa rõ". |
| `WHK-1` | (có `webhook`) webhook hợp lệ → `EventID` khác rỗng, `Kind == payment`, `TransactionCode` và `Status` đúng. | Sự kiện áp nhầm giao dịch, hoặc khách đã trả mà đơn không được xác nhận. |
| `WHK-2` | Phân tích cùng một webhook hai lần → cùng `EventID`. | Core khử trùng theo `EventID`; đổi thì lần gửi lại bị áp như sự kiện mới. |
| `WHK-3` | Webhook của hai giao dịch khác nhau → `EventID` khác nhau. | Sự kiện thứ hai bị coi là bản trùng và bỏ qua: khách đã trả mà đơn không được xác nhận. |
| `WHK-4` | Webhook bị sửa → không được chấp nhận (`err == nil` là vi phạm). `Partner` cài `Tamperer` thì dùng bản do developer làm hỏng và đòi đúng `ErrInvalidSignature`; không cài thì lật một byte giữa thân và chỉ đòi có lỗi. | Kẻ gian giả được webhook "đã thanh toán". |
| `WHK-5` | Thân rỗng, header nil, thân 2 MiB ngẫu nhiên → trả lỗi, không panic. | Core áp một sự kiện không có thật, hoặc connector sập khi gặp request rác. |
| `WHK-6` | `AckWebhook` với cả bốn outcome: status 100–599; hai lần gọi cùng outcome ra cùng kết quả (hàm thuần). | Core hỏi một lần lúc nạp connector; không thuần thì đối tác nhận phản hồi khác ý connector. |
| `RFD-1` | (có `refund`) `ModeDown`/`ModeHang`/`ModeReject`/`ModeGarbage` → như `PAY-3`/`PAY-4`/`PAY-5`/`PAY-6`. | Như các luật tương ứng của Create, cho tiền hoàn. |
| `RFD-2` | (có `query_refund`) lệnh hoàn đối tác không biết → `Found == false`, không lỗi. | Core không đóng được lệnh hoàn chưa tới đối tác, khách không được hoàn lại. |
| `RFD-3` | (có `query_refund`) `ModeHang` (ctx hạn 300 ms) và `ModeGarbage` → có lỗi; `err == nil` là vi phạm. | `failed` bịa ra khiến core chốt hoàn thất bại và có thể hoàn tiền cho khách hai lần; "không thấy" khiến core coi lệnh hoàn chưa tới đối tác. |
| `LOG-1` | Create (và Charge nếu có) với khách mang chuỗi canh (tên có dấu, email có `+`, điện thoại đầu `0`, địa chỉ nhiều dòng), rồi (nếu có `query_payment`) truy vấn chính giao dịch đó: `RawExchange.RequestBody`, `ResponseBody` và câu lỗi không còn chuỗi canh ở dạng thô, JSON-escape (kể cả `\uXXXX`, `\/`), URL-encode (`QueryEscape`, `PathEscape`), khác hoa thường, (điện thoại) dạng `84…`/`+84…`, hay (địa chỉ) một dòng riêng của nó. Hoàn tiền không mang dữ liệu khách nên không kiểm. Cách so là gói `redact`, cùng thuật toán với `providerhttp.Redact` và core. | Vết lời gọi đối tác (bảng `provider_requests`) và log của core lưu dữ liệu cá nhân của khách. Core che lại lần nữa nhưng chỉ với giá trị nó biết: đối tác trả lời bằng cách viết khác (ví dụ tên tách họ/tên) thì chỉ connector che được. |
| `CTX-1` | Mọi thao tác gọi mạng (Create, Charge, Query, Cancel, Refund, QueryRefund đã khai) nhận ctx đã huỷ, đối tác treo → trả về trong 2 s, không panic. | Core không dừng được lời gọi khi tắt máy hay khi khách bỏ đi. |

`Harness.CreateCallsPartner == false` (Create chỉ ký tại chỗ) thì `ID-4` và `PAY-3`..`PAY-6` bị bỏ.

Mọi request bộ luật gửi mang `CreatedAt` (và `TransactionCreatedAt` ở hoàn tiền) cố định ở quá khứ như core
gửi giá trị đã lưu, nên connector dựng mã từ `CreatedAt` tìm lại được giao dịch ở truy vấn, huỷ và hoàn.

Đối tác giả nên làm như đối tác thật: nhớ khách gửi lúc tạo và nhắc lại trong phản hồi truy vấn (như
`acmetest`), để `LOG-1` kiểm được cả việc connector che phản hồi. Che bằng `providerhttp.Redact(body, giá trị...)`.

## Đối tác giả

`Partner` do developer cài, thường bằng một `httptest.Server` đóng vai đối tác:

- `SetMode`: `ModeNormal` trả lời bình thường; `ModeDown` từ chối kết nối; `ModeHang` nhận rồi im (phải
  được giải phóng khi test xong, ví dụ đóng một channel trong `tb.Cleanup`); `ModeReject` từ chối rõ ràng;
  `ModeGarbage` trả 200 với thân không hiểu được.
- `SignedWebhook`: dựng webhook hợp lệ đã ký như đối tác thật gửi.
- `SetPaymentStatus`: quyết định câu trả lời cho lần truy vấn kế tiếp của giao dịch.
- Tuỳ chọn `conformance.Tamperer` (`TamperedWebhook(raw) RawWebhook`): trả bản sao webhook đã ký với nội
  dung **được ký** bị đổi, chữ ký giữ nguyên. Nên cài, nhất là khi chữ ký không phủ toàn bộ thân: không cài
  thì `WHK-4` lật byte giữa thân, và nếu byte đó nằm ngoài phần được ký thì connector đúng cũng bị báo vi phạm.
- Tuỳ chọn `conformance.RequestRecorder` (`OrderCodes() []string`): mã đơn (mã giao dịch phía đối tác, đúng như
  connector đặt trong request) của mọi lệnh tạo thanh toán đối tác giả đã NHẬN, theo thứ tự nhận. Nên cài: không
  cài thì `ID-4` bị bỏ.
- Tuỳ chọn `conformance.RequestCounter` (`BusinessRequests() int`): số request nghiệp vụ (không tính lấy
  token) đối tác giả đã nhận từ lúc tạo. Đếm ngay khi NHẬN request, trước khi treo hay trả lời, trên MỌI
  đường nghiệp vụ (tạo, truy vấn, huỷ, hoàn…); `ModeDown` không tính. Nên
  cài: không cài thì lượt huỷ thiếu `ProviderRef` khi đối tác treo của `CXL-2` bị bỏ (`Skipped`), vì bộ luật
  không đoán bằng thời gian xem connector có gọi đối tác hay không.

Lưu ý khi viết Harness:

- `Partner` phải an toàn khi dùng đồng thời: connector treo bị bỏ mặc vẫn có thể gọi tới nó trong lúc
  luật kế tiếp chạy (mỗi lượt dùng cặp mới, nhưng goroutine cũ chỉ được giải phóng khi test xong).
- `h.New` chạy trực tiếp trên goroutine của test, không có guard: nó không được treo; lỗi thì gọi `tb.Fatal`.

## Dùng trong test của connector

Ví dụ chạy được với connector mẫu `acme` của `template/` (đổi đường dẫn import theo module của bạn):

```go
package acme_test

import (
	"testing"

	"bitbucket.org/hasaki-tech/payment-connector-sdk/conformance"
	"bitbucket.org/hasaki-tech/payment-connector-sdk/connector"
	"bitbucket.org/hasaki-tech/payment-connector-sdk/paykit"
	"bitbucket.org/hasaki-tech/payment-connector-sdk/template/acme"
	"bitbucket.org/hasaki-tech/payment-connector-sdk/template/acme/acmetest"
)

func TestConformance(t *testing.T) {
	conformance.Run(t, conformance.Harness{
		New: func(tb testing.TB) (connector.Connector, conformance.Partner) {
			partner := acmetest.NewPartner(tb) // máy chủ HTTP thật trên 127.0.0.1 đóng vai đối tác, tự dọn khi test xong
			c, err := acme.New(acme.Config{
				BaseURL:    partner.URL(),
				MerchantID: partner.MerchantID(),
				SecretKey:  partner.Secret(),
			})
			if err != nil {
				tb.Fatal(err)
			}
			return c, partner
		},
		Method:             paykit.MethodWallet, // phương thức acme khai trong Methods()
		Amount:             paykit.Money{Amount: 150000, Currency: "VND"},
		CreateCallsPartner: true,
	})
}
```

`Run` tạo một subtest cho mỗi lượt của mỗi luật (`TestConformance/PAY-4`, `TestConformance/PAY-4/grpc`…):
vi phạm là `t.Errorf` với mô tả điều thấy được, điều mong đợi và hậu quả; luật bị bỏ là `t.Skip` có lý do.
Cần kết quả để tự xử lý thì gọi `conformance.Check(t, h)`, hàm này không bao giờ đánh trượt `t`.
`Run` chạy toàn bộ `Check` trước khi tạo subtest, nên `go test -run 'TestConformance/PAY-4'` chỉ lọc phần
báo cáo, không rút ngắn thời gian chạy.
