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
go get github.com/nhanvoanh/payment-sdk@v0.1.0
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
