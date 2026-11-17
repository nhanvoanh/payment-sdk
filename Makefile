# Sinh code gRPC bằng plugin build từ sdk/tools (phiên bản ghim), không dùng plugin cài sẵn trên máy.
PROTO_BIN := $(CURDIR)/tools/bin
PROTO_SRC := proto/hasaki/payment/connector/v1/connector.proto
MODULE := bitbucket.org/hasaki-tech/payment-connector-sdk

.PHONY: proto proto-check

proto:
	cd tools && go build -o $(PROTO_BIN)/protoc-gen-go google.golang.org/protobuf/cmd/protoc-gen-go
	cd tools && go build -o $(PROTO_BIN)/protoc-gen-go-grpc google.golang.org/grpc/cmd/protoc-gen-go-grpc
	protoc --plugin=protoc-gen-go=$(PROTO_BIN)/protoc-gen-go \
		--plugin=protoc-gen-go-grpc=$(PROTO_BIN)/protoc-gen-go-grpc \
		-I proto \
		--go_out=. --go_opt=module=$(MODULE) \
		--go-grpc_out=. --go-grpc_opt=module=$(MODULE) \
		$(PROTO_SRC)

# Sinh lại rồi đòi code đã commit trùng khớp (kể cả file mới chưa theo dõi).
proto-check: proto
	@test -z "$$(git status --porcelain -- connectorpb)" || { git status --short -- connectorpb; echo "connectorpb lệch so với .proto: chạy make -C sdk proto rồi commit"; exit 1; }
