.PHONY: build install test test-race lint test-integration proto sdk-ts-test browser-test observability-validate observability-acceptance observability-race observability-compose-smoke

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

test: observability-validate
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

# sdk-ts-test runs the @fuse/sdk (TS/JS client, change 0050) node test suite: it installs
# the npm workspace deps and runs `npm test` under sdk/ts, which spawns a REAL connect-go
# server (go run ./test/server) and drives the SDK over the wire, asserting no-loss/no-dup
# from TS across a reconnect. It FAILS LOUD when node is absent (a plain `command -v node`
# guard, NOT a green skip) so a CI env that is supposed to have node never silently passes.
# The Go sdk tests run in the default `go test ./...` lane; this is the TS lane. A real
# browser (Playwright) run of observe streaming + reconnect over connect-web is a deferred
# MANUAL check — see sdk/ts/README.md "## Verify (human)".
sdk-ts-test:
	@command -v node >/dev/null 2>&1 || { echo "node required for sdk-ts-test (TS SDK lane) but not found on PATH"; exit 1; }
	cd sdk/ts && npm install && npm test

# browser-test runs the PERMANENT headless-browser reconnect lane for @fuse/sdk (change
# 0056, Task 5). It drives the REAL Wander example app (examples/wander) — importing the
# REAL @fuse/sdk over @connectrpc/connect-web — in headless chromium (playwright-go) against
# a REAL `fuse loop-serve-net` backend with a SCRIPTED LLM_GATEWAY_URL double (NEVER Claude),
# KILLS THE NETWORK mid-stream, and asserts the reply completes after a transparent reconnect
# with no-loss/no-dup. It is the enforced successor to the deferred manual real-browser proof
# recorded in sdk/ts/README.md "Verify (human)". The lane is LOUD on toolchain absence: a
# missing node/esbuild/go/chromium hard-fails (t.Fatal), never a green t.Skip, so a passing
# suite can never hide an unexercised browser path.
#
# It rides the Go toolchain (a //go:build browser test), so it needs esbuild on PATH (the
# repo npm workspace: run `npm install` first) and a playwright-installable chromium (CI's
# mcp-integration job already installs it).
browser-test:
	@command -v node >/dev/null 2>&1 || { echo "node required for browser-test (headless-browser lane) but not found on PATH"; exit 1; }
	npm install
	go test -tags browser -timeout 300s ./examples/wander/...

# Validates every reference-stack artifact and its routing/provisioning relationships
# without Docker. This is part of `make test` and the observability CI acceptance gate.
observability-validate:
	go run ./deploy/observability/validate.go

# Hermetic release gate for the loop observability stack. This never requires an
# external collector: traces are exported in-memory and metrics are scraped through
# the production handler while the Connect/runtime/SDK acceptance tests prove the
# authenticated replay boundary.
observability-acceptance: observability-validate
	go test -count=1 ./cmd/fuse -run 'TestObservabilityAcceptanceHermetic|TestObservationOutageIsBoundedAndDoesNotFailLoopAppend|TestAuthPassAndDeny|TestAuthTenantSpoofRejected|TestReconnectNoLossNoDup'
	go test -count=1 ./internal/observe -run 'TestRunnerNormalShutdownClosesSubscriptionExactlyOnce|TestFanoutOutageAttemptsEveryProjectorWithoutRetry'

# Focused permanent race lane: fanout/runner lifecycle plus the logging live-control
# mutation, expiry, reopen, and shutdown stress tests.
observability-race:
	go test -race ./internal/observe/... ./internal/runtime ./internal/agent ./internal/loopconnect ./cmd/fuse

# Optional operator smoke. It is intentionally outside CI and reports loudly when
# Docker Compose is unavailable instead of pretending the external stack was tested.
observability-compose-smoke: observability-validate
	@command -v docker >/dev/null 2>&1 || { echo "SKIP: observability Compose smoke requires docker"; exit 0; }
	@docker compose version >/dev/null 2>&1 || { echo "SKIP: observability Compose smoke requires Docker Compose v2"; exit 0; }
	docker compose -f deploy/observability/docker-compose.yml config
	docker run --rm --entrypoint promtool -v "$(CURDIR)/deploy/observability/alerts.yml:/etc/prometheus/alerts.yml:ro" prom/prometheus:v3.5.0 check rules /etc/prometheus/alerts.yml
