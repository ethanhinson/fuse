.PHONY: build install egress-forwarder egress-datapath test test-race lint test-integration proto sdk-ts-test browser-test observability-validate observability-acceptance observability-race observability-compose-smoke

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

# egress-forwarder builds the IN-CONTAINER half of egress control (change 0064):
# the small relay fuse bind-mounts into a `--network none` sandbox so that
# curl/git/pip can address the host-side proxy over loopback. See
# cmd/fuse-egress-forward.
#
# Three properties of this target are load-bearing, not stylistic:
#
#   - GOOS=linux, always. The artifact runs inside the CONTAINER, not on the
#     host, so it is cross-compiled even when `make build` is producing a darwin
#     binary right beside it.
#   - CGO_ENABLED=0, so the result is static: no libc, no dynamic loader, no
#     dependency on anything in the operator's image. The image is not asked to
#     cooperate (alpine:3.20 has no socat), which is the entire reason fuse
#     ships this rather than shelling out to something.
#   - One artifact per architecture, named by it. The right one is chosen by the
#     architecture of the IMAGE the sandbox runs, which is a deployment fact —
#     hence the separate target rather than a step inside `build`.
#
# fuse FINDS the artifact itself at startup (cmd/fuse/sandbox.go), looking beside
# its own executable and nowhere else — a mount source must not be config- or
# model-selectable — in this order:
#
#   <dir of the fuse binary>/dist/fuse-egress-forward-linux-<arch>   (a checkout:
#       `make build` + `make egress-forwarder` put both here already)
#   <dir of the fuse binary>/fuse-egress-forward-linux-<arch>        (a `go
#       install`ed fuse: copy the artifact next to ~/go/bin/fuse)
#
# <arch> is the host's, since every container runtime defaults to the host
# platform. Without the artifact, `egress.mode: enforce` is still safe — it is
# deny-all, since the floor is on and no hole is opened — and fuse says so loudly
# on stderr at startup rather than looking like a broken network.
EGRESS_FORWARDER_ARCHS ?= amd64 arm64

egress-forwarder:
	@mkdir -p dist
	@for arch in $(EGRESS_FORWARDER_ARCHS); do \
	  echo "building dist/fuse-egress-forward-linux-$$arch"; \
	  CGO_ENABLED=0 GOOS=linux GOARCH=$$arch go build -trimpath \
	    -ldflags "-s -w $(LDFLAGS)" \
	    -o dist/fuse-egress-forward-linux-$$arch ./cmd/fuse-egress-forward || exit 1; \
	done

test: observability-validate
	go test ./...

# egress-datapath (change 0064): the end-to-end egress datapath check against a
# REAL container runtime. It builds the arch-matched forwarder artifact, then
# runs the `egress_datapath`-tagged, GOOS=linux-only test that starts a
# --network none container and proves a declared destination is reachable
# through the mounted socket + forwarder while an undeclared one is refused.
# Native-Linux Docker only: Docker Desktop for macOS cannot relay a host UNIX
# socket bind-mounted across its VM file-sharing layer, so the test file does
# not build off linux and this target is a no-op there.
egress-datapath: egress-forwarder
	go test -tags egress_datapath -run TestEgressDatapathEndToEnd -v -count=1 -timeout 300s ./internal/tools/sandbox/

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
