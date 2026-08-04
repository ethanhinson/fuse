.PHONY: build install test lint test-integration

build:
	go build -o fuse ./cmd/fuse

install:
	go install ./cmd/fuse

test:
	go test ./...

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
