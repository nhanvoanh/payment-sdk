# Dựng nhiều tầng: tầng cuối chỉ có một binary tĩnh, chạy bằng người dùng không phải root.
# Dockerfile này viết cho repo SAU KHI sao chép mẫu: go.mod ở thư mục gốc repo, bối cảnh dựng là thư mục gốc đó.
# Mạng chỉ dùng ở tầng dựng để tải các module Go đã ghim bằng go.sum; image cuối không chứa gì khác.
FROM golang:1.24 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/connector ./cmd/connector

# Base tối giản: không shell, không trình quản lý gói; tài khoản nonroot (uid 65532) có sẵn.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/connector /connector
USER nonroot:nonroot
# Cổng mặc định của connectorsdk (CONNECTOR_LISTEN_ADDR).
EXPOSE 9090
ENTRYPOINT ["/connector"]
