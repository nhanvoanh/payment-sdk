# Payment SDK cho connector

Module Go `github.com/nhanvoanh/payment-sdk`: thứ một connector đối tác cần để nói chuyện với core thanh toán. Repo
này chỉ chứa SDK; core ở repo riêng, connector của từng đối tác cũng ở repo riêng. Người viết connector đọc được repo
này mà không cần thấy core.

| Gói | Việc |
|---|---|
| `connector` | Interface `Connector` (và `Refunder`, `Canceler`, `Charger`, `Redirecter`…), kiểu request/kết quả, lỗi chuẩn (`ErrUnknownOutcome`, `ErrProviderUnavailable`, `RejectedError`…). |
| `paykit` | Từ vựng chung: tiền, phương thức, khách. |
| `connectorsdk` | `Serve`/`Main`: chạy connector thành server gRPC + mTLS mà core gọi. |
| `connectorrpc`, `connectorpb` | Hợp đồng gRPC (`proto/`) và proxy hai phía. `connectorpb` là code sinh: không sửa tay. |
| `providerhttp`, `signing`, `tokencache`, `redact` | Gọi HTTP tới đối tác và phân loại lỗi, ký/kiểm chữ ký, giữ token, che dữ liệu khách. |
| `conformance` | Bộ luật mọi connector phải qua (`conformance/README.md`). |
| `connectortest`, `connectorrpc/grpctest`, `connectorrpc/tlstest` | Bộ-test: chỉ dùng trong file `_test.go`. |
| `template` | Connector mẫu "acme" chạy được, kèm đối tác giả: điểm xuất phát cho connector mới (`template/README.md`). |

## Dùng SDK trong repo connector

```
go get github.com/nhanvoanh/payment-sdk@v0.3.0
```

rồi import gói cần dùng, ví dụ `github.com/nhanvoanh/payment-sdk/connector`. `go.mod` của connector chỉ có
`require github.com/nhanvoanh/payment-sdk vX.Y.Z`, không `replace` trỏ vào bản sao cục bộ.

### Tải module riêng tư

Repo này riêng tư, nên `go` không lấy được nó qua proxy công khai (`proxy.golang.org`) hay kiểm qua `sum.golang.org`.
Cần hai thứ:

1. `GOPRIVATE=github.com/nhanvoanh/*`: `go` tải thẳng bằng git và không hỏi proxy, sumdb công khai cho các module
   này. Hash vẫn được ghi và kiểm bằng `go.sum` như mọi module khác.
2. git lấy repo qua SSH (khoá SSH có quyền đọc repo) thay cho HTTPS. Đặt bằng biến môi trường của git, chỉ cho lệnh
   đang chạy, không đụng cấu hình toàn máy:

```
GOPRIVATE='github.com/nhanvoanh/*' \
GIT_CONFIG_COUNT=1 \
GIT_CONFIG_KEY_0='url.git@github.com:nhanvoanh/payment-sdk.git.insteadOf' \
GIT_CONFIG_VALUE_0='https://github.com/nhanvoanh/payment-sdk' \
go mod download github.com/nhanvoanh/payment-sdk
```

(`go get …@vX.Y.Z` hay `go build` dùng cùng các biến đó.) Muốn đặt lâu dài thì tuỳ máy: `go env -w GOPRIVATE=…` và
`git config --global url."git@github.com:".insteadOf https://github.com/` cũng được, nhưng không bắt buộc; biến môi
trường như trên là đủ, và CI nên dùng cách đó.

Khi đã có module trong cache (`go env GOMODCACHE`), các lệnh sau chạy được không cần mạng. Container dựng image không
có khoá SSH: tải ở máy chủ rồi cho bước dựng đọc cache module đó (tầng `gomod` của `template/Dockerfile`, `GOPROXY=file:///gomod`);
`go.sum` vẫn kiểm hash. Không tắt kiểm TLS hay `GOSUMDB`, không đặt `GOINSECURE`/`GONOSUMCHECK`.

## Phiên bản

- Tag dạng `vMAJOR.MINOR.PATCH` (ví dụ `v0.1.0`), tag có chú thích, đặt trên `main`.
- Đổi hợp đồng (interface hay kiểu của `connector`/`paykit`, `.proto`, luật conformance, hành vi `connectorsdk` mà
  core dựa vào) phải lên ít nhất một MINOR mới. Sửa lỗi không đổi hợp đồng: PATCH.
- Không bao giờ dời hay đặt lại một tag đã đẩy lên: `go.sum` của mọi repo đã dùng nó ghi hash theo nội dung, tag đổi
  nội dung thì `go` báo lỗi kiểm hash. Cần sửa thì ra tag mới.

`v0.2.0` (thêm, không đổi gì của `v0.1.0`; triển khai core trước, connector sau):

- `connector.ChargeRequest.PaymentCode`: mã thanh toán khách đưa ra để quét, trên dây là `payment_code = 11` của `ChargeRequest`.
- Luật `CHG-2` (mã thanh toán không lộ ra `Raw`, câu lỗi, `ProviderRef`, `NextAction`, log), `Harness.ChargeCode`, `conformance.LogReporter`.
- `signing.RSAEncryptPKCS1v15` cho đối tác đòi gửi dữ liệu mã hoá RSA PKCS#1 v1.5.

`v0.3.0` (thêm, không đổi gì của `v0.2.0`, trừ việc `connectorsdk.Main` giờ báo lỗi lúc khởi động nếu `OTEL_TRACES_EXPORTER` hoặc biến giao thức OTLP có giá trị lạ; triển khai bên nào trước cũng được: lời gọi chưa mang trace thì chỉ là không có span):

- Gói mới `oteltrace`: `Setup` dựng OpenTelemetry tracing (mặc định TẮT), `HasParent`, `LogAttrs`, `NewLogHandler`; bộ test
  `oteltrace/oteltracetest` (`Install`, `Leaks`, chỉ dùng trong `_test.go`).
- `connectorrpc.TracingDialOption(connector)` (client, cho core) và `connectorrpc.TracingServerOption()` (server).
- `providerhttp`: span `HTTP <METHOD>` cho mỗi lời gọi đối tác, chỉ khi ctx đã có span cha.
- `connectorsdk.Main` đọc `OTEL_*` và `CONNECTOR_ENV`, tự gọi `oteltrace.Setup` và xả span khi tắt; dòng log `rpc` có
  `trace_id`/`span_id` khi lời gọi có trace. Chi tiết ở mục "Tracing (OpenTelemetry)".

## Tracing (OpenTelemetry)

Mặc định TẮT: `OTEL_TRACES_EXPORTER` không đặt hoặc `none` thì không có exporter và provider toàn cục không bị đụng tới.
`connectorsdk.Main` gọi `oteltrace.Setup` cho connector; service đặt `service.name` là `payment-connector-<tên>`,
`CONNECTOR_ENV` thành `deployment.environment.name`. Chỉ `OTEL_TRACES_EXPORTER` và các biến giao thức OTLP có giá trị lạ mới làm tiến trình báo lỗi lúc khởi động; giá trị không hợp lệ của biến khác (sampler, `OTEL_RESOURCE_ATTRIBUTES`) chỉ log WARN rồi dùng mặc định.
Collector chết hay exporter lỗi không bao giờ làm hỏng lời gọi hay chặn khởi động; lỗi exporter chỉ log WARN
`otel export error` tối đa một lần mỗi phút.

| Biến | Ý nghĩa |
|---|---|
| `OTEL_TRACES_EXPORTER` | `none` (mặc định), `otlp`, `console` (in ra stderr, cho máy phát triển). |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | Địa chỉ collector (chuẩn OTel, exporter tự đọc). |
| `OTEL_EXPORTER_OTLP_TRACES_PROTOCOL`, rồi `OTEL_EXPORTER_OTLP_PROTOCOL` | `grpc` (mặc định) hoặc `http/protobuf`; biến riêng cho trace thắng biến chung. |
| `OTEL_TRACES_SAMPLER`, `OTEL_TRACES_SAMPLER_ARG` | Lấy mẫu theo chuẩn OTel, ví dụ `parentbased_traceidratio` và `0.2`. |
| `OTEL_SERVICE_NAME`, `OTEL_RESOURCE_ATTRIBUTES` | Ghi đè tên service và thuộc tính resource mà SDK đặt. |
| `CONNECTOR_ENV` | Giá trị của `deployment.environment.name` (rỗng thì bỏ thuộc tính). |

Cấu hình mẫu:

```bash
# Private cloud: SigNoz
OTEL_TRACES_EXPORTER=otlp
OTEL_EXPORTER_OTLP_ENDPOINT=http://signoz-otel-collector:4317
OTEL_EXPORTER_OTLP_PROTOCOL=grpc
OTEL_TRACES_SAMPLER=parentbased_traceidratio
OTEL_TRACES_SAMPLER_ARG=0.2

# AWS: ADOT collector (sidecar/daemon) -> X-Ray / CloudWatch
OTEL_TRACES_EXPORTER=otlp
OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4317

# Máy phát triển
OTEL_TRACES_EXPORTER=console
```

Quy tắc "chỉ tạo span khi đã có span cha":

- Phía client, `TracingDialOption` chỉ tạo span cho RPC nghiệp vụ khi ctx đã mang span context hợp lệ; span mang
  `payment.connector=<tên>` và trace context đi theo header `traceparent` của gRPC.
- Phía server, `TracingServerOption` chỉ tạo span khi metadata đến mang `traceparent` hợp lệ. Connector tự nó không bao
  giờ mở trace mới.
- `Describe` và health không bao giờ tạo span, ở cả hai phía.
- `providerhttp` chỉ tạo span `HTTP <METHOD>` khi ctx đã có span cha (`oteltrace.HasParent`). Thuộc tính:
  `http.request.method`, `server.address`, `server.port`, `url.path`, `http.response.status_code`, kích thước thân
  request/response và `payment.call.outcome`. Không gửi `traceparent` cho đối tác.
- Log: `oteltrace.LogAttrs(ctx)` và `oteltrace.NewLogHandler` thêm `trace_id`/`span_id` khi ctx có span; dưới
  `logger.WithGroup(...)` hai trường này nằm trong nhóm. Dòng log `rpc` của `connectorsdk` tự mang chúng.

KHÔNG BAO GIỜ ghi vào span: thân request/response, query của URL, header, dữ liệu khách, mã thanh toán, MAC, nội dung
thông điệp gRPC, và câu lỗi của đối tác. Test của từng gói quét thuộc tính bằng giá trị canh (`oteltracetest.Leaks`).

## Phát triển SDK

```
go build ./... && go vet ./... && go test -race ./...
golangci-lint run ./...
make proto          # sinh lại connectorpb bằng plugin ghim trong tools/ (module riêng); cần protoc 29.3
make proto-check    # sinh lại rồi đòi connectorpb đã commit trùng khớp
```

`arch_test.go` giữ luật cấu trúc: SDK chỉ import thư viện chuẩn, chính nó và module khai trong `go.mod` (không bao
giờ import core, `go.mod` không có `replace`); `redact` chỉ dùng thư viện chuẩn; gói bộ-test chỉ vào file `_test.go`
(ngoại lệ hẹp: `template/cmd/fakepartner` được dùng `acmetest`). `.golangci.yml` giữ cùng luật bộ-test bằng depguard.

`names_test.go` quét mọi file văn bản tìm tên đối tác thật. Danh sách lấy từ biến `SDK_FORBIDDEN_NAMES` (phẩy, không
phân biệt hoa thường) do CI đặt từ biến bí mật; nó không nằm trong repo, và test chỉ in đường dẫn, không in từ khoá.
Biến rỗng thì test bỏ qua.
