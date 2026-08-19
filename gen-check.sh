#!/usr/bin/env bash
set -eo pipefail
export PATH=/home/workbench/Projects/personal/local-model-works/.tools/bin:/home/workbench/sdk/go/bin:/usr/local/sbin:/usr/bin:/usr/sbin:/bin:/sbin
cd /home/workbench/Projects/personal/local-model-works

# Proto generation must precede tidy: the server package imports the
# generated agentv1 package, and the plugins are part of the module itself.
echo "=== gen-proto ==="
mkdir -p .tools/bin
go build -o .tools/bin/protoc-gen-go google.golang.org/protobuf/cmd/protoc-gen-go
go build -o .tools/bin/protoc-gen-connect connectrpc.com/connect/cmd/protoc-gen-connect-go
( cd proto && go run github.com/bufbuild/buf/cmd/buf generate )

echo "=== go mod tidy ==="
go mod tidy

echo "=== gen-oapi ==="
go run ./internal/generate/oapi gen

echo "=== gen-sqlc ==="
go run github.com/sqlc-dev/sqlc/cmd/sqlc generate

echo "=== gen-modules ==="
go run ./internal/generate/modules gen

echo "=== web lock ==="
( cd web && npm install --package-lock-only --silent || true )

echo "=== go build ./... ==="
go build ./...

echo ===DONE===
