# Mẫu connector

Một connector hoàn chỉnh, chạy được, cho đối tác tưởng tượng tên "acme", kèm đối tác giả để test. Sao chép
nó làm điểm xuất phát cho connector của bạn. Nó ngắn có chủ ý: đọc hết trong vài phút.

| File | Việc |
|---|---|
| `acme/connector.go` | Các thao tác và chỗ chọn lỗi trả về. Đọc kỹ `send`, `reply`, `tokenError`. |
| `acme/wire.go` | Hình dạng dữ liệu trên đường dây và đổi qua lại với kiểu của SDK. |
| `acme/config.go` | `Config` và `ConfigFromEnv` (`ACME_BASE_URL`, `ACME_MERCHANT_ID`, `ACME_SECRET_KEY`, `ACME_TIMEOUT`, `ACME_TIMEZONE`). |
| `acme/acmetest/partner.go` | Đối tác giả (máy chủ HTTP thật), cài `conformance.Partner`. |
| `acme/connector_test.go` | Bộ conformance, các test lỗi/token, và chạy qua mTLS thật. |
| `cmd/connector/main.go` | Tiến trình: `ConfigFromEnv` → `acme.New` → `connectorsdk.Main`. |
| `Dockerfile` | Image nhiều tầng, chạy không phải root, chỉ chứa binary. |
| `cmd/fakepartner/main.go` | Chạy đối tác giả thành tiến trình (chỉ cho phát triển, compose của core). |
| `docker-compose.yml` | Core + connector của bạn trên máy (bước 6). |

## Các bước làm connector của bạn

1. **Tạo repo từ mẫu.** Sao chép thư mục này thành gốc repo mới. Tạo `go.mod` riêng, `require` module
   SDK, và đổi đường dẫn import `bitbucket.org/hasaki-tech/payment-connector-sdk/template/...` thành
   đường dẫn module của bạn (`Dockerfile` giả định `go.mod` nằm ở gốc repo).
2. **Đổi tên `acme`.** Đổi thư mục `acme`, `acme.Name` (tên connector: khớp `^[a-z][a-z0-9_]{0,31}$`, KHÔNG
   bao giờ đổi sau khi chạy thật vì core khử trùng webhook theo tên), tiền tố biến môi trường `ACME_`, và
   `acmetest`.
3. **Cài interface.** Giữ `Connector` bắt buộc; thao tác đối tác không có thì xoá phương thức (và dòng
   `var _ connector.Xxx` tương ứng), đối tác có thêm (`Charger`, `Redirecter`) thì thêm. `connector.As…` và
   luật `ID-3` kiểm khai báo khớp cài đặt. Sửa `wire.go` theo API của đối tác thật, và sửa `acmetest` theo
   cùng giao thức: đối tác giả phải độc lập với mã connector, đừng cho nó import connector.
4. **Chạy conformance.** `cd` vào thư mục repo rồi `go test -race ./...`. `TestAcme_Conformance` chạy toàn bộ
   luật (xem bảng dưới) hai lượt: gọi thẳng và qua gRPC. Mọi luật phải qua, và không luật nào bị bỏ trừ
   luật của thao tác connector không khai.
5. **Chạy `cmd/connector` với mTLS.** Biến môi trường (không có chứng chỉ thì tiến trình từ chối chạy):

   ```
   ACME_BASE_URL=https://api.acme.example  ACME_MERCHANT_ID=...  ACME_SECRET_KEY=...
   CONNECTOR_LISTEN_ADDR=:9090
   CONNECTOR_TLS_CERT=/etc/connector/tls.crt  CONNECTOR_TLS_KEY=/etc/connector/tls.key
   CONNECTOR_TLS_CA=/etc/connector/ca.crt     CONNECTOR_ALLOWED_CLIENTS=core.internal
   ```

   Chỉ khi phát triển cục bộ mới đặt `CONNECTOR_INSECURE=true` (thay cho ba biến TLS); mặc định là TLS 1.3 và
   bắt buộc chứng chỉ client. `TestAcme_OverMTLS` là ví dụ chạy được của đúng đường này.
6. **Chạy trọn luồng với image của core.** `docker-compose.yml` ở thư mục này dựng MariaDB, `migrate`, `serve`
   (từ image core do đội nền tảng cấp) và connector của bạn (`build: .`), để một request HTTP đi hết đường
   core → gRPC → connector → đối tác. Đặt ba biến bắt buộc rồi chạy:

   ```
   export PAYMENT_CORE_IMAGE=<image của core>  CONNECTOR_NAME=<tên connector của bạn>
   export JWT_KEYS="k1:$(openssl rand -base64 32)"
   docker compose up -d --build --wait connector mariadb
   docker compose run --rm migrate && docker compose up -d serve
   docker compose run --rm -T serve client create --code dev --name Dev --scopes payment:create,payment:read
   ```

   Gỡ: `PAYMENT_CORE_IMAGE=x CONNECTOR_NAME=x JWT_KEYS=x docker compose down -v` (compose nội suy cả file nên `down` cũng cần
   ba biến bắt buộc; giá trị tuỳ ý). `PAYMENT_CORE_IMAGE` phải là image bạn đã dựng hoặc kéo về.

   rồi lấy token (`POST /v1/oauth/token`) và tạo thanh toán (`POST /v1/payments`, header `Idempotency-Key`,
   `method` là phương thức connector của bạn khai; đổi `CONNECTOR_METHOD` nếu không phải `wallet`) ở
   `127.0.0.1:8080` (đổi bằng `PAYMENT_HTTP_PORT`). Chỉ cho máy phát triển (`CONNECTOR_INSECURE=true`). Không có Kafka,
   worker, Redis: file có chú thích cách bật từng thứ. Đặt `ACME_*` (hoặc tiền tố mới của bạn) cho đối tác thật hoặc
   đối tác giả. Bước dựng connector cần BuildKit (mặc định từ Docker Engine 23 và trong Docker Desktop) và Docker
   Compose 2.17 trở lên (`additional_contexts`). Bước dựng lấy `GOPROXY` từ shell của người gọi nếu biến đó đã đặt
   (không thì `https://proxy.golang.org,direct`). Máy mà Docker không kiểm được TLS tới proxy module Go: chạy
   `go mod download` ở máy chủ rồi dựng từ cache module đó, `go.sum` vẫn kiểm hash:
   `GOPROXY=file:///gomod GOMOD_CONTEXT="$(go env GOMODCACHE)/cache/download" docker compose up -d --build ...`
   (đúng cách `make image OFFLINE_MODULES=1` của repo core làm; `Dockerfile` ở đây có tầng `gomod` tuỳ chọn cho việc
   đó). Không tắt kiểm TLS hay `GOSUMDB`. Bộ chạy thử
   của repo core, `make compose-smoke`, chạy đúng luồng này với connector mẫu và đối tác giả `cmd/fakepartner`
   (chỉ cho phát triển; không có trong image connector của bạn).

## Giao thức của "acme" (đối tác giả)

- `POST /oauth/token` `{merchant_id, secret}` → `{access_token, expires_in}` (300 giây). Mọi request
  nghiệp vụ gửi `Authorization: Bearer <token>`; connector giữ token bằng `tokencache` (50 request
  đồng thời chỉ lấy token một lần).
- `POST /v1/payments` → thanh toán `{payment_id, order_code, status, redirect_url, expires_at}`; `expires_at`
  (hạn phiên, RFC 3339) ghi theo giờ acme kèm offset (`+07:00`): connector đổi về UTC, và trả zero khi hạn ngoài
  (now + 1 phút, now + 7 ngày] mà core nhận (`sessionExpiry`, luật `PAY-8`; `acmetest` cài `conformance.ExpiryReporter`);
  `GET /v1/payments?order_code=` (404 `not_found` nếu không biết); `POST /v1/payments/{id}/cancel`.
  `POST /v1/refunds` và `GET /v1/refunds?refund_code=`. Các lệnh tạo gửi `Idempotency-Key`.
- `order_code` (mã đơn phía acme) có dạng `yymmdd_<mã giao dịch>`: ngày TẠO giao dịch ở múi giờ `ACME_TIMEZONE`
  (mặc định UTC), lấy từ `CreatedAt` core gửi — không bao giờ từ `time.Now()` (`orderCode` trong `wire.go`).
  Truy vấn dựng lại đúng mã từ `QueryPaymentRequest.CreatedAt`, kể cả khi lệnh tạo chưa nghe được trả lời (chưa
  có `payment_id`); webhook tách `yymmdd_` để trả lại mã giao dịch của core. Đổi `ACME_TIMEZONE` sau khi đã chạy
  thật thì truy vấn giao dịch cũ dựng sai mã: chỉ đặt một lần. `CreatedAt` zero (core cũ: triển khai CORE TRƯỚC, connector sau) không bao giờ thành ngày: tạo trả
  `ErrProviderUnavailable` (không gửi gì), truy vấn trả `ErrUnknownOutcome` (không bao giờ `Found=false`). Đổi định
  dạng `order_code` của connector đang chạy thì phải truy vấn thử mã định dạng cũ khi mã mới không thấy.
  Luật `ID-4`/`ID-5` kiểm điều này (`acmetest` cài `conformance.RequestRecorder`, và `conformance.RefundRecorder`
  để `ID-4` kiểm cả `refund_code` gửi lại giống hệt).
- Trạng thái thanh toán: `pending`, `paid`, `failed`, `canceled`; hoàn tiền: `pending`, `refunded`, `failed`.
  Trạng thái lạ là lỗi, không đoán.
- HTTP: 2xx là kết quả; 4xx kèm `{"error":{"code","message"}}` là từ chối rõ ràng; **401 là từ chối trước khi
  xử lý**; 5xx là lỗi của acme, không nói gì về việc đã xử lý hay chưa.
- Webhook: `POST` JSON `{event_id, type: "payment.updated", order_code, payment_id, status, ...}`, header
  `X-Acme-Signature` = HMAC-SHA256 hex của toàn bộ thân với `SecretKey`. Trả 2xx để acme dừng gửi lại,
  4xx để bỏ cuộc, 5xx để gửi lại.
- Lệnh tạo gửi `customer {id, name, email, phone, address}`; `address` (`paykit.Customer.Address`, một chuỗi có thể
  nhiều dòng, rỗng thì bỏ trường) acme dùng để chống gian lận. Mọi phản hồi thanh toán, hoàn tiền và webhook nhắc lại `customer` đã gửi lúc tạo.

## Không để dữ liệu khách vào vết

`RawExchange` (thân request/response connector trả cho core để lưu vết) không được chứa tên, email, điện
thoại, địa chỉ của khách hay dữ liệu thẻ (hợp đồng trong doc của `connector.RawExchange`, luật `LOG-1`).
Acme làm ở `exchange`: lệnh tạo che bằng khách của request **và** của phản hồi; truy vấn, huỷ, hoàn, truy vấn
hoàn không mang khách nên che bằng khách đọc từ phản hồi (kể cả thân lỗi); `ParseWebhook` che `ProviderEvent.Raw` (core lưu nó vào
`provider_events`) bằng khách trong thân webhook. Dùng `providerhttp.RedactCustomer(body, khách)` (hoặc
`RedactValues(body, giá trị...)` khi chỉ có giá trị rời): nó thay mọi dạng (thô, JSON-escape, URL-encode, khác hoa
thường, bỏ dấu; từng chữ của tên khi đứng thành từ riêng; điện thoại cả dạng `84`/`+84` và có dấu phân cách; địa
chỉ nhiều dòng cả từng dòng dài) bằng `[REDACTED]` và che số thẻ, `cvv`. Tự tách họ/tên hay đổi cách viết (bỏ
dấu từng chữ) thì che cả phần đã tách. Câu lỗi cũng không được chép thân phản hồi có dữ liệu khách.
`Config.Redact` của `providerhttp` là lớp khác: che thứ cố định (token, chữ ký) trước khi thân vào `Raw`.

**Trả `Raw` cả khi lỗi — đây là thứ cần nhất khi tranh chấp.** Mọi nhánh lỗi sau khi đã gửi (từ chối, 5xx, thân
không đọc được, `order_code` lệch, timeout giữa chừng) trả kết quả mang `Raw` đã che cùng lỗi; lời gọi hỏng giữa
chừng lấy vết từ `*providerhttp.Error` (`errors.As`). Acme gom việc này ở `exchange`, gọi ngay sau `send`. Lỗi
trước khi gửi (token, thiếu `ProviderRef`, `CreatedAt` zero) không có vết. `LOG-1` kiểm cả `Raw` ở nhánh lỗi.

## Lỗi nào trả sentinel nào, và vì sao (luật tiền)

Core chốt giao dịch **thất bại** chỉ khi nghe `ErrProviderUnavailable` hoặc `ErrProviderRejected`; sai một
lần ở đây là khách bị trừ tiền hai lần hoặc đơn bị huỷ khi đã trả. Gặp gì khác, hay không chắc, thì
`ErrUnknownOutcome`: giao dịch giữ nguyên chờ truy vấn.

| Tình huống | Trả | Vì sao |
|---|---|---|
| Lấy token lỗi (kể cả timeout, 5xx, thân hỏng) | `ErrProviderUnavailable` | Lấy token là lời gọi riêng, chưa mang thông tin về tiền. Ở lần lấy đầu, request nghiệp vụ chưa từng được gửi; ở lượt lấy token SAU một lần 401, request nghiệp vụ đã gửi nhưng đã bị acme từ chối trước khi xử lý (401). Cả hai trường hợp acme chưa xử lý gì. Nguyên nhân ghi bằng `%v`, không `%w`, để lỗi không khớp thêm `ErrUnknownOutcome`. |
| Không kết nối được tới acme (chưa gửi byte nào) | `ErrProviderUnavailable` | `providerhttp` phân loại; connector trả nguyên, chỉ thêm ngữ cảnh bằng `%w`. |
| Hết hạn hoặc đứt sau khi đã gửi | `ErrUnknownOutcome` | `providerhttp` phân loại. Acme có thể đã trừ tiền. |
| 401 lần đầu | làm mới token, gửi lại **một** lần | 401 là từ chối trước khi xử lý nên gửi lại không thể trừ tiền hai lần. Gửi cùng `Idempotency-Key`. |
| 401 lần hai | `ErrProviderUnavailable` | Chỉ đúng khi tài liệu của đối tác bảo đảm 401 là từ chối trước khi xử lý; không được giả định. Không có bảo đảm đó thì dùng `ErrUnknownOutcome`. |
| 4xx có mã thuộc tập từ chối của thao tác (bảng dưới) | `*connector.RejectedError{Code, Message}` | Tài liệu acme nói rõ đó là từ chối cuối cùng của thao tác này. Mã ngoài tập là chưa rõ: `ErrUnknownOutcome`. |
| `not_found` khi truy vấn thanh toán hoặc hoàn tiền (theo mã lỗi của acme, bất kể mã HTTP) | `Found=false`, không lỗi | Core truy vấn cả giao dịch chưa tới được acme; lỗi ở đây khiến giao dịch không bao giờ đóng được. |
| 5xx, 3xx, 4xx mã chưa ánh xạ hoặc không có mã | `ErrUnknownOutcome` | Acme (hoặc proxy giữa đường) đã trả lời nên có thể đã xử lý. Không bao giờ unavailable. |
| 2xx với thân không hiểu được, thiếu trường, trạng thái lạ | `ErrUnknownOutcome` | Tiền có thể đã đi; không đoán "thất bại". |
| Thiếu `ProviderRef` khi huỷ (ta phát hiện trước khi gửi) | `*connector.RejectedError{Code: "cannot_cancel"}`, không gọi mạng | Acme chỉ huỷ theo `ProviderRef`, không theo `TransactionCode`: giao dịch chưa có `ProviderRef` (Create chưa nghe được trả lời) thì biết chắc không huỷ được bằng thông tin được cho. Câu trả lời phải dứt khoát: với Rejected, core truy vấn lại rồi mới quyết (acme không biết giao dịch thì cho hết hạn; acme báo đã thu thì ghi đã trả; acme còn giữ thanh toán đang chờ thì không hết hạn, lượt sau quyết). Từ chối ở đây là quyết định tại chỗ, không gửi request nào tới acme (`acmetest.Partner` cài `conformance.RequestCounter` để lượt thêm của `CXL-2` kiểm được điều đó). `ErrProviderUnavailable` ở đây làm core hiểu là "không rõ" và không bao giờ cho giao dịch hết hạn (luật `CXL-3`). |
| Thiếu `ProviderRef` khi hoàn (ta phát hiện trước khi gửi) | `ErrProviderUnavailable` | Chắc chắn chưa gửi gì. |
| Phản hồi tạo thanh toán có `order_code` khác mã đã gửi | `ErrUnknownOutcome` | Không tin phản hồi về giao dịch khác. |
| Webhook sai chữ ký | `ErrInvalidSignature` | Kiểm trên byte thô trước khi đọc thân; nếu chữ ký tính trên query, dùng `RawWebhook.RawQuery` (nguyên văn), không dùng `Query` (đã giải mã, có thể thiếu cặp). |
| Webhook loại không liên quan | `ErrEventNotRelevant` | Đã xác thực nhưng không có gì để áp. |
| Huỷ thanh toán đã trả | `CancelResult{Status: connector.StatusSucceeded}`, err nil (409 `already_paid`) | Đây là câu trả lời "đã thu", không phải từ chối: core không bao giờ cho hết hạn ở lượt đó. Trả `RejectedError` thì core truy vấn lại, và nếu truy vấn của acme còn chậm (báo chờ) thì giao dịch khách đã trả bị xử lý như chưa trả (luật `CXL-4`). Lệnh huỷ của client lúc đó nhận `provider_rejected` với mã `not_canceled` (không còn `already_paid` của acme). Không bao giờ tự báo `canceled` khi acme không nói vậy. |

### Mã lỗi acme theo từng thao tác

**Cùng một mã có thể là từ chối ở thao tác này và là "đã có tiền" ở thao tác khác.** Vì vậy mỗi thao tác có tập mã riêng
(`rejectCreate`, `rejectCancel`, `rejectRefund` trong `connector.go`); chỉ thêm mã khi tài liệu của đối tác nói rõ nó là từ chối cuối cùng, chắc chắn không có tiền nào đổi chủ, cho đúng thao tác đó.

| Mã | Tạo thanh toán | Huỷ | Hoàn tiền | Truy vấn |
|---|---|---|---|---|
| `declined`, `invalid_request` | Rejected | Rejected | Rejected | Unknown |
| `already_paid` | **Unknown** (acme đã thu tiền, ví dụ lệnh tạo trước thất lạc phản hồi) | **"Đã thu"**: `Status succeeded`, err nil (không nằm trong `rejectCancel`) | **Unknown** | Unknown |
| `not_found` | Unknown | Rejected | Rejected | `Found=false` (theo mã, bất kể mã HTTP) |
| mã khác hoặc không có mã | Unknown | Unknown | Unknown | Unknown |

## Luật conformance

Chi tiết và hậu quả từng luật: `conformance/README.md` của SDK. Mã luật: `ID-1`..`ID-5` (tên, phương thức,
khai năng lực, mã phía đối tác tất định, không đoán ngày khi thiếu `CreatedAt`), `PAY-1`..`PAY-8` (tạo thanh toán: kết quả hợp lệ, `ProviderRef`, kết nối bị từ chối, treo,
từ chối rõ ràng, thân rác, gửi lại cùng khoá, `ExpiresAt` UTC trong khoảng core nhận), `QRY-1`..`QRY-4` (truy vấn), `CHG-1` (thu ngay), `CXL-1`..`CXL-4` (huỷ: không báo `canceled` khi
đã thu, khi treo hay thân rác; không đổi đối tác treo thành từ chối, kể cả khi thiếu `ProviderRef`; trả lời dứt khoát khi thiếu `ProviderRef`; đã thu thì trả "đã thu", không từ chối), `WHK-1`..`WHK-6`
(webhook: sự kiện, `EventID` ổn định và khác nhau, chữ ký, đầu vào rác, `AckWebhook` thuần), `RFD-1`..`RFD-4` (hoàn tiền), `LOG-1` (vết không chứa dữ liệu khách), `CTX-1` (tôn trọng ctx đã huỷ). Mỗi luật chạy thẳng và qua gRPC (`…/grpc`).

## Test

`go test -race ./...` chạy dưới một phút. Đối tác giả đóng kết nối sau mỗi phản hồi CHỈ để `ModeDown` luôn là
"từ chối kết nối" tất định. Với đối tác thật có giữ kết nối, đối tác sập sau khi đã có lưu lượng có thể cho
`ErrUnknownOutcome` thay vì `ErrProviderUnavailable` (hướng an toàn, chấp nhận được); `ModeHang`/`ModeReject`/`ModeGarbage` chỉ áp cho lời gọi nghiệp vụ, không áp cho
`/oauth/token` (chúng mô tả lỗi của lời gọi tiền).

## Image

`.dockerignore` loại `.git` và `.env*`, `*.pem`, `*.key` ở MỌI cấp thư mục (mẫu `**/`) khỏi bối cảnh dựng: giữ nguyên và thêm các file bí mật khác của bạn. Không đưa
chứng chỉ hay secret vào image; chúng đến qua biến môi trường và volume lúc chạy.
