.PHONY: build install test test-race lint test-integration proto

# Version is stamped into the binary via -ldflags. It defaults to `git describe`
# (tags + short SHA + dirty marker) and falls back to the source default when git
# is unavailable. Override explicitly with: make build VERSION=1.2.3
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null)
VERSION_PKG := github.com/ethanhinson/fuse/internal/version
LDFLAGS := $(if $(VERSION),-X $(VERSION_PKG).Version=$(VERSION))

build:
	go build -ldflags "$(LDFLAGS)" -o fuse ./cmd/fuse

install:
	go install -ldflags "$(LDFLAGS)" ./cmd/fuse

test:
	go test ./...

# test-race runs the suite under the race detector. The platform's core value is
# concurrent multi-agent execution, so the race build must be a first-class,
# routinely-run target (and is wired into CI). See the P0 review findings.
test-race:
	go test -race ./...

lint:
	go vet ./...

# Integration harness (change 0008): brings up the Docker Compose stack
# (mcp-everything + mock-oauth2), runs the //go:build integration suite in
# internal/mcp/, then tears the stack down even if the tests fail. Docker- and
# Playwright-dependent scenarios skip gracefully when those are unavailable.
test-integration:
	docker compose -f internal/mcp/testdata/docker-compose.yml up -d
	go test -tags integration -v -timeout 300s ./internal/mcp/... ; \
	  status=$$? ; \
	  docker compose -f internal/mcp/testdata/docker-compose.yml down -v ; \
	  exit $$status

# proto regenerates the committed loop.* wire stubs (Go + TS) from the .proto
# contract (change 0055, fuse.loop.v1). Generate-and-commit: run this after editing
# proto/fuse/loop/v1/*.proto and commit the result. Requires buf + protoc-gen-go +
# protoc-gen-connect-go on PATH and the TS plugin installed under proto/
# (cd proto && npm ci).
proto:
	./proto/generate.sh
