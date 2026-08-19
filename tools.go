//go:build tools

// Package tools pins the code-generation toolchain. Versions are resolved
// through go.mod so CI can regenerate from a clean clone with no external
// binary installs.
package tools

import (
	_ "github.com/bufbuild/buf/cmd/buf"
	_ "github.com/connectrpc/protoc-gen-connect-go"
	_ "github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen"
	_ "github.com/sqlc-dev/sqlc/cmd/sqlc"
	_ "google.golang.org/protobuf/cmd/protoc-gen-go"
)
