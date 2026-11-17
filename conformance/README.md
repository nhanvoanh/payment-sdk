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
vi phạm. Nếu `Describe` qua gRPC hỏng thì `ID-1/grpc` báo lỗi và các lượt `/grpc` khác bị bỏ.

## Luật

Mã luật cố định, là hợp đồng với developer connector.

| Mã | Nội dung | Vì sao (hậu quả về tiền) |
|---|---|---|
| `ID-1` | `Name()` khớp `^[a-z][a-z0-9_]{0,31}$` và không đổi giữa hai lần gọi; qua gRPC: `Describe` thành công. | Tên là khoá cấu hình và khoá khử trùng webhook; tên đổi thì một sự kiện có thể bị áp hai lần, tên sai thì core không nạp được connector. |
| `ID-2` | `Methods()` không rỗng, không trùng, có `Harness.Method`. | Core định tuyến giao dịch theo phương thức; thiếu thì khách không trả tiền được qua đối tác này. |
| `ID-3` | `Supports(op)` đúng ⇒ connector cài interface tương ứng (`connector.As…` đúng). | Khai mà không cài thì lệnh huỷ/hoàn tiền nhận `ErrNotSupported` lúc chạy. |
| `PAY-1` | Create ở `ModeNormal`: không lỗi; `Status` thuộc pending, succeeded, failed, canceled, on_hold; `NextAction` redirect có URL tuyệt đối http(s), show_qr có `QRData`. | Khách không tới được trang thanh toán hoặc core không biết đã thu tiền chưa. |
| `PAY-2` | `ProviderRef` là UTF-8 hợp lệ, không ký tự điều khiển, tối đa 255 ký tự. | Dài hơn thì core cắt và gửi bản bị cắt cho đối tác ở lệnh truy vấn/hoàn sau: đối tác không nhận ra giao dịch. |
| `PAY-3` | `ModeDown` (từ chối kết nối) → lỗi khớp `ErrProviderUnavailable`. | Chỉ lỗi này cho core chốt thất bại ngay để khách tạo lại thanh toán; sai thì giao dịch treo. |
| `PAY-4` | `ModeHang`, ctx hạn 300 ms → trả về trong 300 ms + 2 s; có lỗi; **không** khớp `ErrProviderUnavailable` hay `ErrProviderRejected`. | Timeout là không rõ kết quả; báo "chưa xử lý" hay "bị từ chối" thì core chốt thất bại trong khi đối tác có thể đã trừ tiền: khách trả hai lần. |
| `PAY-5` | `ModeReject` → lỗi khớp `ErrProviderRejected` (nên dùng `RejectedError`). | Lời từ chối rõ ràng mà báo khác thì giao dịch treo chờ truy vấn thay vì báo khách ngay. |
| `PAY-6` | `ModeGarbage` (200 với thân không hiểu được) → có lỗi; không khớp `ErrProviderUnavailable` hay `ErrProviderRejected`. | Đối tác đã trả lời, có thể đã nhận tiền; chốt thất bại thì khách trả hai lần. |
| `PAY-7` | Create hai lần cùng `TransactionCode` và `IdempotencyKey` → không panic. | Core gửi lại cùng khoá khi không rõ kết quả; panic thì giao dịch kẹt. |
| `QRY-1` | (có `query_payment`) giao dịch đối tác không biết → `Found == false`, không lỗi. | Core truy vấn cả giao dịch chưa tới được đối tác; báo lỗi thì không bao giờ đóng được giao dịch đó. |
| `QRY-2` | Sau `SetPaymentStatus(succeeded, amount)` → `Found`, `Status == succeeded`, `AmountPaid` bằng số đã đặt hoặc 0; `Currency` rỗng hoặc ba chữ hoa. | Khách đã trả mà đơn không được xác nhận, hoặc giao dịch rơi vào lệch tiền. |
| `QRY-3` | (có `query_payment`) `ModeHang` → như `PAY-4`. | Như `PAY-4`. |
| `CXL-1` | (có `cancel`) sau `SetPaymentStatus(succeeded)`, huỷ **không** được trả `StatusCanceled` kèm `err == nil`. | Core huỷ đơn trong khi khách đã bị trừ tiền. |
| `WHK-1` | (có `webhook`) webhook hợp lệ → `EventID` khác rỗng, `Kind == payment`, `TransactionCode` và `Status` đúng. | Sự kiện áp nhầm giao dịch, hoặc khách đã trả mà đơn không được xác nhận. |
| `WHK-2` | Phân tích cùng một webhook hai lần → cùng `EventID`. | Core khử trùng theo `EventID`; đổi thì lần gửi lại bị áp như sự kiện mới. |
| `WHK-3` | Webhook của hai giao dịch khác nhau → `EventID` khác nhau. | Sự kiện thứ hai bị coi là bản trùng và bỏ qua: khách đã trả mà đơn không được xác nhận. |
| `WHK-4` | Sửa một byte giữa thân → lỗi khớp `ErrInvalidSignature`. | Kẻ gian giả được webhook "đã thanh toán". |
| `WHK-5` | Thân rỗng, header nil, thân 2 MiB ngẫu nhiên → trả lỗi, không panic. | Core áp một sự kiện không có thật, hoặc connector sập khi gặp request rác. |
| `WHK-6` | `AckWebhook` với cả bốn outcome: status 100–599; hai lần gọi cùng outcome ra cùng kết quả (hàm thuần). | Core hỏi một lần lúc nạp connector; không thuần thì đối tác nhận phản hồi khác ý connector. |
| `RFD-1` | (có `refund`) `ModeDown`/`ModeHang`/`ModeReject` → như `PAY-3`/`PAY-4`/`PAY-5`. | Như các luật tương ứng của Create, cho tiền hoàn. |
| `RFD-2` | (có `query_refund`) lệnh hoàn đối tác không biết → `Found == false`, không lỗi. | Core không đóng được lệnh hoàn chưa tới đối tác, khách không được hoàn lại. |
| `CTX-1` | Mọi thao tác gọi mạng (Create, Charge, Query, Cancel, Refund, QueryRefund đã khai) nhận ctx đã huỷ, đối tác treo → trả về trong 2 s, không panic. | Core không dừng được lời gọi khi tắt máy hay khi khách bỏ đi. |

`Harness.CreateCallsPartner == false` (Create chỉ ký tại chỗ) thì `PAY-3`..`PAY-6` bị bỏ.

## Đối tác giả

`Partner` do developer cài, thường bằng một `httptest.Server` đóng vai đối tác:

- `SetMode`: `ModeNormal` trả lời bình thường; `ModeDown` từ chối kết nối; `ModeHang` nhận rồi im (phải
  được giải phóng khi test xong, ví dụ đóng một channel trong `tb.Cleanup`); `ModeReject` từ chối rõ ràng;
  `ModeGarbage` trả 200 với thân không hiểu được.
- `SignedWebhook`: dựng webhook hợp lệ đã ký như đối tác thật gửi; chữ ký phải phủ toàn bộ thân (`WHK-4`
  sửa byte ở giữa thân).
- `SetPaymentStatus`: quyết định câu trả lời cho lần truy vấn kế tiếp của giao dịch.

## Dùng trong test của connector

```go
package acme_test

func TestConformance(t *testing.T) {
	conformance.Run(t, conformance.Harness{
		New: func(tb testing.TB) (connector.Connector, conformance.Partner) {
			partner := acmetest.NewPartner(tb) // httptest.Server đóng vai đối tác, tự dọn khi test xong
			c, err := acme.New(acme.Config{
				BaseURL:   partner.URL(),
				SecretKey: partner.Secret(),
			})
			if err != nil {
				tb.Fatal(err)
			}
			return c, partner
		},
		Method:             paykit.MethodQR,
		Amount:             paykit.Money{Amount: 150000, Currency: "VND"},
		CreateCallsPartner: true,
	})
}
```

`Run` tạo một subtest cho mỗi lượt của mỗi luật (`TestConformance/PAY-4`, `TestConformance/PAY-4/grpc`…):
vi phạm là `t.Errorf` với mô tả điều thấy được, điều mong đợi và hậu quả; luật bị bỏ là `t.Skip` có lý do.
Cần kết quả để tự xử lý thì gọi `conformance.Check(t, h)`, hàm này không bao giờ đánh trượt `t`.
