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
  trả về ngay, không gửi); và `ID-4` khi connector tự trả lại kết quả theo khoá idempotency nên đối tác giả
  không nhận đủ request để so. `Run` chỉ `t.Log` các mục này, không tạo subtest.

## Luật

Mã luật cố định, là hợp đồng với developer connector.

| Mã | Nội dung | Vì sao (hậu quả về tiền) |
|---|---|---|
| `ID-1` | `Name()` khớp `^[a-z][a-z0-9_]{0,31}$` và không đổi giữa hai lần gọi; qua gRPC: `Describe` thành công. | Tên là khoá cấu hình và khoá khử trùng webhook; tên đổi thì một sự kiện có thể bị áp hai lần, tên sai thì core không nạp được connector. |
| `ID-2` | `Methods()` không rỗng, không trùng, có `Harness.Method`. | Core định tuyến giao dịch theo phương thức; thiếu thì khách không trả tiền được qua đối tác này. |
| `ID-3` | `Supports(op)` đúng ⇒ connector cài interface tương ứng (`connector.As…` đúng). | Khai mà không cài thì lệnh huỷ/hoàn tiền nhận `ErrNotSupported` lúc chạy. |
| `ID-4` | (cần `Harness.CreateCallsPartner` và `Partner` cài `RequestRecorder`; không cài thì `Skipped`) Bốn lần `CreatePayment`: hai lần liên tiếp CÙNG request → cùng mã đơn (bắt số ngẫu nhiên, nano giây, bộ đếm); chờ ~1,1 giây (CHỈ lượt thẳng; lượt `/grpc` không chờ) rồi lần ba cùng request → đúng mã lần một (bắt mã mang ngày đúng nhưng giờ-phút-giây từ đồng hồ); mã không mang ngày hôm nay (`yyyymmdd`/`yymmdd` ở UTC hay UTC+7) khi `CreatedAt` ở ngày khác; lần bốn cùng request nhưng `CreatedAt` lệch ba ngày → nếu mã lần một mang ngày của `CreatedAt` (dạng nhận ra được) thì mã lần bốn phải mang ngày mới (mã không mang ngày nhận ra được thì không xét). (có `refund` và `Partner` cài `RefundRecorder`; không cài thì `Skipped` với lý do) hai lần `CreateRefund` cùng request, một trước một sau lúc chờ → cùng mã hoàn. Đối tác giả không nhận đủ request (connector tự trả lại kết quả theo khoá idempotency) → `NotChecked` kèm lý do. Lượt `/grpc` không bắt được giây của đồng hồ trong mã. | Connector phải dựng mã phía đối tác TẤT ĐỊNH từ `TransactionCode`/`RefundCode` + `CreatedAt` (core cấp, giá trị đã lưu, UTC, micro giây, như nhau ở mọi lần gọi lại), không từ đồng hồ lúc gọi. Không thì truy vấn/huỷ một lệnh tạo chưa rõ kết quả (chưa có `ProviderRef`) hỏi sai mã, đối tác báo không thấy và core cho hết hạn một đơn khách đã trả. |
| `ID-5` | (mọi connector) `CreatePayment` với `CreatedAt` zero → `ErrProviderUnavailable` (và đối tác giả có `RequestRecorder` không nhận request nào), hoặc thành công với mã KHÔNG mang ngày 0001-01-01 (`010101`/`00010101`) ở chỗ mà mã của lệnh tạo có `CreatedAt` mang ngày (cùng dạng, tính từ đầu hay từ cuối mã; chuỗi `010101` có sẵn ở chỗ khác như hằng số hay mã giao dịch không tính; cần `RequestRecorder` để dò); lỗi khác là vi phạm. (có `query_payment`) truy vấn với `CreatedAt` zero một giao dịch đã tạo, đối tác báo đã thu → `Found` đúng hoặc `ErrUnknownOutcome`; `Found=false` hay lỗi khác là vi phạm. Bước chuẩn bị (tạo bình thường) hỏng thì bỏ, xem `PAY-1`. | `CreatedAt` zero nghĩa là core cũ chưa gửi trường này (triển khai sai thứ tự). Đoán ngày thì lệnh tạo gửi `010101_<mã>`; sau khi nâng core, truy vấn hỏi mã với ngày thật, đối tác báo không thấy và core cho hết hạn một đơn khách đã trả. |
| `PAY-1` | Create ở `ModeNormal`: không lỗi; `Status` thuộc pending, succeeded, failed, canceled, on_hold; `NextAction` redirect có URL tuyệt đối http(s), show_qr có `QRData`; `NextAction.Kind` ngoài none/redirect/show_qr/rỗng bị từ chối. | Khách không tới được trang thanh toán hoặc core không biết đã thu tiền chưa. |
| `PAY-2` | `ProviderRef` là UTF-8 hợp lệ, không ký tự điều khiển, tối đa 255 ký tự. | Dài hơn thì core cắt và gửi bản bị cắt cho đối tác ở lệnh truy vấn/hoàn sau: đối tác không nhận ra giao dịch. |
| `PAY-3` | `ModeDown` (từ chối kết nối) → lỗi khớp `ErrProviderUnavailable`, không khớp `ErrUnknownOutcome`/`ErrProviderRejected`. | Chỉ lỗi này cho core chốt thất bại ngay để khách tạo lại thanh toán; sai thì giao dịch treo. |
| `PAY-4` | `ModeHang`, ctx hạn 300 ms → trả về trong 300 ms + 2 s; lỗi "chưa rõ theo core" (xem trên; `ErrNotSupported` cũng không đạt). | Timeout là không rõ kết quả; báo "chưa xử lý" hay "bị từ chối" thì core chốt thất bại trong khi đối tác có thể đã trừ tiền: khách trả hai lần. |
| `PAY-5` | `ModeReject` → lỗi khớp `ErrProviderRejected` (nên dùng `RejectedError`), không khớp `ErrUnknownOutcome`. | Lời từ chối rõ ràng mà báo khác thì giao dịch treo chờ truy vấn thay vì báo khách ngay. |
| `PAY-6` | `ModeGarbage` (200 với thân không hiểu được) → lỗi "chưa rõ theo core". | Đối tác đã trả lời, có thể đã nhận tiền; chốt thất bại thì khách trả hai lần. |
| `PAY-7` | Create hai lần cùng `TransactionCode` và `IdempotencyKey` → không panic. | Core gửi lại cùng khoá khi không rõ kết quả; panic thì giao dịch kẹt. |
| `PAY-8` | Create ở `ModeNormal`: `ExpiresAt` zero (đối tác không báo hạn) hợp lệ; khác zero thì phải trong (now + 1 phút, now + 7 ngày] tính từ lúc gọi, `Location()` là UTC (chỉ lượt thẳng; gRPC luôn chở UTC), và (`Partner` cài `ExpiryReporter`) lệch dưới 1 giây so với hạn đối tác giả đã đặt. Không cài `ExpiryReporter` thì chỉ kiểm khoảng và luật vào `Skipped` (khi không có vi phạm) với lý do nêu `ExpiryReporter`. Create lỗi thì bỏ, xem `PAY-1`. | Core chỉ nhận hạn trong khoảng đó và bỏ hạn ngoài khoảng (phiên dùng hạn mặc định của core). Đọc giờ địa phương của đối tác như UTC thì hạn lệch hàng giờ mà vẫn trong khoảng: core cho phiên hết hạn khi khách còn đang trả, hoặc giữ đơn quá lâu. |
| `QRY-1` | (có `query_payment`) giao dịch đối tác không biết → `Found == false`, không lỗi. | Core truy vấn cả giao dịch chưa tới được đối tác; báo lỗi thì không bao giờ đóng được giao dịch đó. |
| `QRY-2` | Sau `SetPaymentStatus(succeeded, amount)` → `Found`, `Status == succeeded`, `AmountPaid` bằng số đã đặt hoặc 0; `Currency` rỗng hoặc ba chữ hoa. | Khách đã trả mà đơn không được xác nhận, hoặc giao dịch rơi vào lệch tiền. |
| `QRY-3` | (có `query_payment`) `ModeHang` → như `PAY-4`. | Như `PAY-4`. |
| `QRY-4` | (có `query_payment`) `ModeGarbage` → có lỗi (lỗi nào cũng được); `err == nil` là vi phạm dù `Found` là gì. | Với truy vấn, core coi mọi lỗi như nhau (truy vấn lại sau); err nil thì core tin kết quả bịa: `failed` khiến khách tạo lại thanh toán trong khi đối tác có thể đã thu, "không thấy" khiến giao dịch bị cho hết hạn. |
| `CHG-1` | (có `charge`) `ModeDown` → như `PAY-3`; `ModeHang` → như `PAY-4`; `ModeReject` → như `PAY-5`; `ModeGarbage` → như `PAY-6`. | Charge thu tiền trực tiếp: báo "chắc chắn chưa xử lý" cho một lần treo thì core chốt thất bại trong khi thẻ của khách đã bị trừ. |
| `CHG-2` | (có `charge`) Charge mang mã thanh toán (từ `Harness.ChargeCode`; không có thì mã canh của bộ luật) ở mọi chế độ của đối tác giả (`ModeNormal`, `ModeDown`, `ModeHang` với ctx hạn 300 ms, `ModeReject`, `ModeGarbage`), rồi truy vấn giao dịch đó nếu có khai truy vấn. Mã (thô, JSON-escape, URL-encode, khác hoa thường, base64 Std/URL có hay không đệm kể cả nằm giữa một khối base64 lớn hơn, hex thường/hoa) không được xuất hiện trong `RawExchange`, câu lỗi, `ProviderRef`, `NextAction`, mã/lời nhắn của đối tác, và log của connector (`LogReporter`). Charge mang mã ở `ModeNormal` mà lỗi thì lượt thành công và lượt truy vấn không kiểm được: luật vào `Skipped` kèm lý do. | Mã thanh toán khách đưa ra để quét mang quyền trừ tiền của khách; core lưu `Raw`, `ProviderRef`, `NextAction` và ghi câu lỗi vào log. Lỗi vận chuyển của `net/http` (`ModeDown`, `ModeHang`) mang nguyên URL kèm query: đối tác nhận mã trên URL thì câu lỗi mang mã. |
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
| `RFD-4` | (có `query_refund` và `refund`) `CreateRefund` ở `ModeNormal` thành công, rồi `QueryRefund` cùng `RefundCode`/`CreatedAt`/`TransactionCreatedAt` và `ProviderRef` **rỗng** → `Found == true` (trạng thái bất kỳ). `Found == false` kèm `err == nil` là vi phạm; lỗi khác `ErrUnknownOutcome` cũng vậy. `ErrUnknownOutcome` không bị tính là vi phạm vì đó là cách báo một lần đối tác lỗi tạm thời (hết giờ, mất kết nối), KHÔNG phải một cách thiết kế: connector trả lỗi cho mọi truy vấn thiếu `ProviderRef` trượt `RFD-2` (`TestCheck_Catches_QueryRefundUnknownWithoutProviderRef`). `CreateRefund` lỗi thì luật vào `Skipped`, xem `RFD-1`. | Core truy vấn lệnh hoàn mà nó mất kết quả tạo (chưa có `ProviderRef`); refund còn `new` từ 30 phút mà truy vấn trả "không thấy" bị chốt `failed`/`not_sent` và số tiền đang giữ được nhả. Connector chỉ truy vấn theo mã của đối tác mà trả "không thấy" khi thiếu `ProviderRef` thì lệnh hoàn đối tác đã nhận bị chốt thất bại: khách có thể được hoàn hai lần. Chỉ có hai cách đạt: (a) truy vấn theo mã dựng tất định từ `RefundCode` + `CreatedAt` (`ID-4`), nên truy vấn được khi `ProviderRef` rỗng; (b) không khai `query_refund` (`OpQueryRefund`): `RFD-2`/`RFD-3`/`RFD-4` vào `Skipped` (`TestCheck_RefundQueryUndeclared_SkipsAndPasses`), core không truy vấn lệnh hoàn và refund kẹt `new` không bao giờ được tự chốt mà chờ người vận hành. |
| `LOG-1` | Khách mang chuỗi canh (tên có dấu `Huỳnh Đoàn Canaryname`, email có `+`, điện thoại đầu `0`, địa chỉ nhiều dòng). Create (và Charge nếu có) mang khách đó; rồi trên chính giao dịch đó, mỗi thao tác đã khai: truy vấn, `ParseWebhook` của webhook đối tác giả ký, hoàn, truy vấn hoàn, huỷ; cuối cùng Create (và Charge) mang khách đó ở `ModeReject` và `ModeGarbage`, để kiểm `Raw` ở nhánh LỖI (connector nên trả `Raw` cả khi lỗi, xem `connector.RawExchange`). `RawExchange.RequestBody`, `ResponseBody`, câu lỗi và `ProviderEvent.Raw` (được phép rỗng) không được còn dữ liệu khách theo ĐÚNG luật của `providerhttp.RedactCustomer` (gói `redact`, cùng thuật toán với core): giá trị trọn ở mọi dạng (thô, JSON-escape kể cả `\uXXXX` hoa/thường và lồng hai lần, URL-encode, khác hoa thường, bỏ dấu); điện thoại ở mọi đầu số và có dấu phân cách; thành phần địa chỉ dài hoặc có chữ số; chữ có dấu của tên đứng riêng (`"Đoàn"`); chữ bỏ dấu/không dấu của tên khi đứng liền chữ khác của tên (`VAN AN`) hoặc là toàn bộ giá trị của trường có tên gợi ý người (`{"last":"HUYNH"}`, `{"middle":"DOAN"}`, `first=CANARYNAME`). Connector che bằng `RedactCustomer` thì qua. | Vết lời gọi đối tác (bảng `provider_requests`), `provider_events` và log của core lưu dữ liệu cá nhân của khách. Core che lại lần nữa với cùng luật nhưng chỉ với khách nó biết. |
| `CTX-1` | Mọi thao tác gọi mạng (Create, Charge, Query, Cancel, Refund, QueryRefund đã khai) nhận ctx đã huỷ, đối tác treo → trả về trong 2 s, không panic. | Core không dừng được lời gọi khi tắt máy hay khi khách bỏ đi. |

`Harness.CreateCallsPartner == false` (Create chỉ ký tại chỗ) thì `ID-4` và `PAY-3`..`PAY-6` bị bỏ (`ID-5` vẫn chạy).

Mọi request bộ luật gửi mang `CreatedAt` (và `TransactionCreatedAt` ở hoàn tiền) cố định ở quá khứ như core
gửi giá trị đã lưu, nên connector dựng mã từ `CreatedAt` tìm lại được giao dịch ở truy vấn, huỷ và hoàn.

**Hợp đồng `CreatedAt` khi triển khai.** Thứ tự là CORE TRƯỚC, connector sau. Connector gặp thời điểm cần dùng
bằng zero (core cũ) KHÔNG BAO GIỜ đoán ngày hay lấy đồng hồ: lệnh tạo/thu trả `ErrProviderUnavailable` (chưa
gửi gì; ví dụ "core chưa gửi CreatedAt — nâng core trước connector"); truy vấn, huỷ, hoàn, truy vấn hoàn trả
`ErrUnknownOutcome`, không bao giờ `Found=false` hay lời từ chối. Thao tác không dùng thời điểm (ví dụ huỷ theo
`ProviderRef`) thì bỏ qua nó. Connector ĐỔI định dạng mã đơn phía đối tác (ví dụ thêm ngày) phải có bước chuyển
tiếp: mã mới "không tìm thấy" thì truy vấn thử mã định dạng cũ trước khi trả `Found=false`, cho tới khi mọi giao
dịch tạo bằng định dạng cũ đã kết thúc.

Đối tác giả nên làm như đối tác thật: nhớ khách gửi lúc tạo và nhắc lại trong phản hồi truy vấn (như
`acmetest`), để `LOG-1` kiểm được cả việc connector che phản hồi. Giới hạn cấu trúc: `LOG-1` chỉ thấy những gì
đối tác giả của chính tác giả connector trả về — đối tác giả không nhắc lại khách (hay nhắc theo cách khác đối tác thật)
thì `LOG-1` không bắt được connector quên che. Che bằng `providerhttp.RedactCustomer(body, khách)` hoặc `RedactValues(body, giá trị...)`.

### Mã thanh toán (`ChargeRequest.PaymentCode`)

Ở thanh toán tại quầy, thu ngân quét mã thanh toán khách đưa ra trên thiết bị của khách; core chuyển nó cho
`Charge` trong `ChargeRequest.PaymentCode` (thêm ở `v0.2.0`). Mã sống ngắn, dùng một lần và mang quyền trừ tiền
của khách: coi như thông tin xác thực. Chỉ `Charge` mang nó; một lần thu "chưa rõ" được chốt bằng truy vấn, không
bao giờ bằng một lần `Charge` thứ hai.

- **Mã rỗng** nghĩa là client không gửi hoặc core cũ hơn `v0.2.0`. Connector cần mã mà nhận rỗng thì trả
  `&connector.RejectedError{Code: "payment_code_required"}` và KHÔNG gọi đối tác; không trả
  `ErrProviderUnavailable` (lỗi đó tính vào bộ ngắt mạch, trong khi đây là lỗi của client). Thứ tự triển khai:
  CORE TRƯỚC, connector sau.
- **`Harness.ChargeCode`** (`func(p Partner) string`): đặt thì mọi `ChargeRequest` bộ luật dựng mang mã nó trả,
  một lần gọi cho mỗi request (mã dùng một lần); trả mã đối tác giả sẽ chấp nhận, dài ít nhất 8 ký tự. Connector
  từ chối Charge khi thiếu mã PHẢI đặt hook này: không có hook, mọi Charge ngoài `CHG-2` mang mã rỗng như core cũ,
  connector từ chối tại chỗ và `CHG-1` hỏng (`ModeDown` đòi `ErrProviderUnavailable` mà nhận lời từ chối). Hook
  chạy trong goroutine của luật, không phải goroutine của test: không gọi `tb.Fatal`/`FailNow`; panic thành vi phạm
  của luật đang chạy. Không đặt thì connector hiện có thấy đúng các request như ở `v0.1.0`; riêng `CHG-2` dùng mã
  canh của bộ luật.
- **`conformance.LogReporter`** (`ConnectorLogs() string`, tuỳ chọn trên `Partner`): mọi thứ connector đã ghi log
  từ lúc `Harness.New` dựng nó, lấy từ một bộ đệm mà developer nối vào logger của connector. Không cài thì `CHG-2`
  chỉ kiểm giá trị trả về, và luật vào `Skipped` (khi không có vi phạm) với lý do nêu `LogReporter`.
- Vi phạm của mọi luật không bao giờ in mã thanh toán đã trao cho connector (kể cả khi connector nhắc lại nó
  trong câu lỗi hay giá trị panic): finding của `CHG-2` chỉ nêu thao tác, tình huống và chỗ lộ.

Giới hạn cấu trúc, như `LOG-1`: `CHG-2` chỉ thấy những gì đối tác giả của chính tác giả connector trả về. Đối tác
giả nên nhắc lại mã thô nó nhận được (sau khi giải mã, nếu đối tác thật giải mã) trong phản hồi như một đối tác
thật có thể làm; không nhắc lại thì `CHG-2` không bắt được connector quên che. Che bằng
`providerhttp.RedactValues(body, mã)` trước khi trả.

`CHG-2` nhận ra mã thô, JSON-escape, URL-encode, khác hoa thường, base64 (Std/URL, có hay không đệm, cả khi mã nằm
giữa một khối base64 lớn hơn) và hex (thường/hoa). Nó KHÔNG thấy: mã bị tách nhóm bằng dấu phân cách (`CFPA YCOD …`)
hay cắt thành mảnh; mã đã mã hoá bằng khoá (ví dụ `signing.RSAEncryptPKCS1v15`) hay băm; các mã hoá khác (base32,
nén, base64 lồng hai lần…); log khi `Partner` không cài `LogReporter`; và mọi thứ đối tác giả không nhắc lại. Qua
được `CHG-2` không thay cho việc đọc lại code: mọi chỗ connector đưa dữ liệu ra (Raw, lỗi, log) phải không mang mã ở
dạng nào.

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
- Tuỳ chọn `conformance.ExpiryReporter` (`PaymentExpiry(transactionCode) (time.Time, bool)`): hạn phiên đối tác
  giả đã đặt vào phản hồi tạo của giao dịch. Nên cài, và cho đối tác giả ghi hạn theo đúng định dạng/múi giờ của
  đối tác thật (ví dụ có offset `+07:00` như `acmetest`): không cài thì `PAY-8` chỉ kiểm khoảng, không bắt được
  connector đọc sai múi giờ, và luật vào `Skipped`.
- Tuỳ chọn `conformance.RefundRecorder` (`RefundCodes() []string`): mã hoàn (đúng như connector đặt trong request)
  của mọi lệnh hoàn đối tác giả đã NHẬN, theo thứ tự nhận. Nên cài khi connector khai `refund`: không cài thì lượt
  mã hoàn của `ID-4` bị bỏ (`Skipped`). Tách khỏi `RequestRecorder` để Partner cũ không phải đổi.
- Tuỳ chọn `conformance.LogReporter` (`ConnectorLogs() string`): log connector đã ghi từ lúc `Harness.New`. Nên
  cài khi connector khai `charge`: không cài thì phần log của `CHG-2` bị bỏ (`Skipped`). Xem mục mã thanh toán.
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

	"github.com/nhanvoanh/payment-sdk/conformance"
	"github.com/nhanvoanh/payment-sdk/connector"
	"github.com/nhanvoanh/payment-sdk/paykit"
	"github.com/nhanvoanh/payment-sdk/template/acme"
	"github.com/nhanvoanh/payment-sdk/template/acme/acmetest"
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
