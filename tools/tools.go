//go:build tools

// Package tools ghim phiên bản plugin sinh code proto. Là module riêng để module SDK
// không phụ thuộc generator.
package tools

import (
	_ "google.golang.org/grpc/cmd/protoc-gen-go-grpc"
	_ "google.golang.org/protobuf/cmd/protoc-gen-go"
)
