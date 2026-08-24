# Local Model Works — top-level build entry points.
# The only supported commands are: make generate | make test | make build | make release

GO       ?= go
GOFLAGS  ?= -trimpath -buildvcs=false
WEB      := web
DIST     := dist
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT   ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
LDFLAGS  := -s -w -X main.Version=$(VERSION) -X main.Commit=$(COMMIT)
DOCKER   ?= docker
WORKER_TAG ?= local-model-works/agon-worker:$(COMMIT)

.PHONY: all generate gen-oapi gen-proto gen-sqlc gen-modules gen-web test test-go test-web build build-server build-agent build-cli build-runner build-web worker-image release clean

all: build

## generate: regenerate all derived code (OpenAPI, Connect/protobuf, sqlc, module registry, web types).
generate: gen-oapi gen-proto gen-sqlc gen-modules gen-web

gen-oapi:
	$(GO) run ./internal/generate/oapi gen

gen-proto:
	@mkdir -p .tools/bin
	$(GO) build -o .tools/bin/protoc-gen-go google.golang.org/protobuf/cmd/protoc-gen-go
	$(GO) build -o .tools/bin/protoc-gen-connect connectrpc.com/connect/cmd/protoc-gen-connect-go
	cd proto && PATH=$(CURDIR)/.tools/bin:$$PATH $(GO) run github.com/bufbuild/buf/cmd/buf generate

gen-sqlc:
	$(GO) run github.com/sqlc-dev/sqlc/cmd/sqlc generate

gen-modules:
	$(GO) run ./internal/generate/modules gen

gen-web:
	cd $(WEB) && npm ci --silent && npm run generate

## test: Go unit/integration tests plus web typecheck and unit tests.
test: build-web test-go test-web

test-go:
	$(GO) test ./...

test-web:
	cd $(WEB) && npm run typecheck && npm run test -- --run

## build: development binaries with the production frontend embedded in lmw-server.
build: build-web build-server build-agent build-cli build-runner

build-web:
	cd $(WEB) && npm run build

build-server: build-web
	$(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o bin/lmw-server ./cmd/lmw-server

build-agent:
	$(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o bin/lmw-agent ./cmd/lmw-agent

build-cli:
	$(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o bin/lmw ./cmd/lmw

build-runner:
	$(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o bin/lmw-agon-runner ./cmd/lmw-agon-runner

worker-image:
	@mkdir -p .build/worker
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o .build/worker/lmw-agon-runner ./cmd/lmw-agon-runner
	DOCKER_BUILDKIT=0 $(DOCKER) build --network host --file workers/agon/Dockerfile --tag $(WORKER_TAG) .

## release: cross-compiled Linux amd64/arm64 bundles under dist/ with SHA-256 sums.
# lmw-agent links NVML via cgo; amd64 builds natively, arm64 needs a cross C toolchain.
ARM64_CC ?= aarch64-linux-gnu-gcc
release: build-web
	rm -rf $(DIST)
	mkdir -p $(DIST)/lmw-linux-amd64/deploy/systemd $(DIST)/lmw-linux-arm64/deploy/systemd
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 $(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(DIST)/lmw-linux-amd64/lmw-server ./cmd/lmw-server
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 $(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(DIST)/lmw-linux-amd64/lmw ./cmd/lmw
	CGO_ENABLED=1 $(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(DIST)/lmw-linux-amd64/lmw-agent ./cmd/lmw-agent
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 $(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(DIST)/lmw-linux-amd64/lmw-agon-runner ./cmd/lmw-agon-runner
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 $(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(DIST)/lmw-linux-arm64/lmw-server ./cmd/lmw-server
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 $(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(DIST)/lmw-linux-arm64/lmw ./cmd/lmw
	@command -v $(ARM64_CC) >/dev/null || { echo "need $(ARM64_CC) for the arm64 agent (NVML cgo)"; exit 1; }
	GOOS=linux GOARCH=arm64 CGO_ENABLED=1 CC=$(ARM64_CC) $(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(DIST)/lmw-linux-arm64/lmw-agent ./cmd/lmw-agent
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 $(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(DIST)/lmw-linux-arm64/lmw-agon-runner ./cmd/lmw-agon-runner
	cp deploy/systemd/* $(DIST)/lmw-linux-amd64/deploy/systemd/
	cp deploy/systemd/* $(DIST)/lmw-linux-arm64/deploy/systemd/
	tar -C $(DIST) -czf $(DIST)/lmw-linux-amd64.tar.gz lmw-linux-amd64
	tar -C $(DIST) -czf $(DIST)/lmw-linux-arm64.tar.gz lmw-linux-arm64
	mkdir -p .build/worker
	cp $(DIST)/lmw-linux-amd64/lmw-agon-runner .build/worker/lmw-agon-runner
	$(DOCKER) buildx build --network host --platform linux/amd64 --file workers/agon/Dockerfile --output type=oci,dest=$(DIST)/lmw-agon-worker-linux-amd64.oci.tar .
	cd $(DIST) && sha256sum lmw-linux-*/lmw* lmw-linux-*.tar.gz lmw-agon-worker-linux-amd64.oci.tar > SHA256SUMS

clean:
	rm -rf bin $(DIST)
