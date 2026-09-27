VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)

.PHONY: build test integration vuln

build:
	CGO_ENABLED=1 go build -ldflags "-X main.version=$(VERSION) -X main.commit=$(COMMIT)" -o bin/willet ./cmd/willet
	@go version bin/willet

test:
	go vet ./...
	go test -race ./...

integration:
	go test -tags integration -count=1 -v ./internal/microvm/

# Scans the source for known vulnerabilities in dependencies and the Go
# standard library (which Dependabot does not cover). govulncheck is pinned in
# go.mod and built with the current toolchain, so its version always matches.
vuln:
	go tool govulncheck ./...
