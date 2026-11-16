# Server Manager - build targets.
# The result is one statically linked executable with the web UI embedded.

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
GOFLAGS := -trimpath
PKG     := ./cmd/server-manager

.PHONY: build build-amd64 build-arm64 release test test-race vet fmt check clean

build: ## Build for the current machine into dist/
	CGO_ENABLED=0 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o dist/server-manager $(PKG)

build-amd64:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o dist/server-manager-linux-amd64 $(PKG)

build-arm64:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o dist/server-manager-linux-arm64 $(PKG)

release: build-amd64 build-arm64 ## Build both Linux architectures with checksums
	cd dist && sha256sum server-manager-linux-amd64 server-manager-linux-arm64 > SHA256SUMS

test:
	go test ./...

test-race:
	go test -race ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

check: vet test ## What CI should run

clean:
	rm -rf dist
