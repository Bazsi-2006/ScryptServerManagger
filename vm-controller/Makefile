# vm-controller build targets.
# The result is one statically linked Linux executable with the web UI embedded.
#
# Builds are reproducible: the same source, VERSION and Go toolchain produce
# byte-identical binaries (no cgo, -trimpath, no VCS stamping, empty build ID).

VERSION      ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
# Pin the toolchain so every machine builds with the same compiler. Go 1.21+
# downloads it automatically; use GO_TOOLCHAIN=local to use the installed Go.
GO_TOOLCHAIN ?= go1.27.4
GO           ?= go

export GOTOOLCHAIN := $(GO_TOOLCHAIN)
export CGO_ENABLED := 0

BUILDFLAGS := -trimpath -buildvcs=false
LDFLAGS    := -s -w -buildid= -X main.version=$(VERSION)
PKG        := ./cmd/vm-controller

.PHONY: build build-amd64 build-arm64 release test test-race vet fmt check clean mock

build: ## Build for this machine into dist/vm-controller
	$(GO) build $(BUILDFLAGS) -ldflags "$(LDFLAGS)" -o dist/vm-controller $(PKG)

build-amd64:
	GOOS=linux GOARCH=amd64 $(GO) build $(BUILDFLAGS) -ldflags "$(LDFLAGS)" -o dist/vm-controller-linux-amd64 $(PKG)

build-arm64:
	GOOS=linux GOARCH=arm64 $(GO) build $(BUILDFLAGS) -ldflags "$(LDFLAGS)" -o dist/vm-controller-linux-arm64 $(PKG)

release: build-amd64 build-arm64 ## Both Linux architectures plus SHA256SUMS
	cd dist && sha256sum vm-controller-linux-amd64 vm-controller-linux-arm64 > SHA256SUMS

test:
	$(GO) test ./...

test-race: export CGO_ENABLED := 1
test-race:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

fmt:
	gofmt -l -w .

check: vet test ## What CI should run

mock: ## Run the development mock of the Vexa-VM API on 127.0.0.1:8080
	$(GO) run ./cmd/mock-vexa -api-key "$${VEXA_API_KEY:-vxa_dev_only}"

clean:
	rm -rf dist
