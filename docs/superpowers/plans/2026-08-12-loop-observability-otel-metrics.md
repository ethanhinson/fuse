<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0051 — Observability for the loop — OTEL traces + Prometheus metrics + Grafana + structured logs](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0051-loop-observability-otel-metrics.md)**
<!-- docket:backlink:end -->

# Loop Observability — Implementation Plan

> **Change:** 0051 — Observability for the loop — OTEL traces + Prometheus metrics + Grafana + structured logs
>
> **Build direction:** Execute this plan task-by-task with TDD. Keep `internal/event`, `internal/runtime`, and `internal/agent` free of OpenTelemetry and Prometheus imports; concrete vendor dependencies belong only in adapter and composition packages.

## Current anchors and invariants

- `internal/event.DurableStore` carries `context.Context` and a tenant-scoped `StreamKey`; `Event.NodeID` completes the consumer-readable tenant/loop/node identity triple.
- `internal/runtime.Deps`, `LoopConfig`, and the `durableSink` adapter are the policy-free composition seams. Per-loop resources need teardown on every normal and early-return path.
- `internal/loopconnect` is the authenticated Connect edge; `cmd/fuse/loop_serve_net.go` owns the HTTP mux and service lifecycle.
- Existing event payloads contain prompts, model output, tool arguments/results, and child results. Routine telemetry must never serialize `Event.Payload` wholesale.
- Replay/live projection must subscribe first and deduplicate by `(tenant, loop, seq)` at the replay watermark. Tenant must be part of every cache/dedup key.
- Live logging configuration must be concurrency-safe and tested with concurrent readers and mutations; passing `go test -race` without such a test is insufficient.

## Task 1: Provider-neutral observation contracts and trace carrier

**Profile:** standard

**Files:**

- Create `internal/observe/contracts.go`
- Create `internal/observe/contracts_test.go`
- Modify `internal/event/event.go`
- Modify `internal/event/event_test.go`
- Modify `internal/event/store_hooks_test.go`

**TDD:**

1. Add failing contract tests for repository-owned `OperationKind`, `Outcome`, `Descriptor`, `Field`, `Observer`, `Handle`, and no-op implementations. The no-op observer must return the input context and a safe inert handle.
2. Add failing event round-trip tests for an optional repository-owned trace carrier containing only validated `traceparent` and `tracestate` strings. Preserve wire compatibility when absent.
3. Implement the contracts and carrier. Keep the package agent-free and vendor-free.
4. Extend the no-OTEL dependency gate to cover `internal/observe` and verify forbidden vendor imports remain unreachable from `internal/event`, `internal/runtime`, and `internal/agent`.

**Verify:** `go test ./internal/observe ./internal/event`

## Task 2: Payload-free structured projection and replay/live runner

**Profile:** standard

**Files:**

- Create `internal/observe/projector.go`
- Create `internal/observe/projector_test.go`
- Create `internal/observe/runner.go`
- Create `internal/observe/runner_test.go`

**TDD:**

1. Add table tests covering every canonical `event.Kind`, including `loop.parked`, and assert the derived record contains timestamp, event name, tenant/loop/node/sequence, bounded operation/outcome/error category, and never contains prompts, responses, deltas, tool args/results, authorization/trace headers, or raw error strings.
2. Add a forced-interleaving test for subscribe-first replay/live handoff. Assert exact-once projection keyed by `(tenant, loop, seq)` and prove identical loop IDs in different tenants do not collide.
3. Add projection-failure tests proving failures are counted/reported through a bounded fallback callback and never flow back into loop execution or retry the event.
4. Implement a composable projector fanout and runner with bounded process-local deduplication, explicit close/cancellation, and no goroutine leaks.

**Verify:** `go test -race ./internal/observe`

## Task 3: Structured JSON logging and concurrency-safe live controls

**Profile:** premium

**Files:**

- Create `internal/observe/logging/logger.go`
- Create `internal/observe/logging/logger_test.go`
- Create `internal/observe/logging/levels.go`
- Create `internal/observe/logging/levels_test.go`
- Create `internal/observe/logging/file.go`
- Create `internal/observe/logging/file_test.go`

**TDD:**

1. Pin the newline-delimited JSON schema and one-write-per-record behavior under concurrent writers. Stdout is the default sink.
2. Test effective level precedence `loop > tenant > global > configured default`, tenant+loop keying, bounded TTL/absolute expiry, inspect/set/remove operations, and non-suppressible audit records.
3. Add a tight concurrent read/mutate/expire/reload/reopen/shutdown test that is meaningful under `-race`.
4. Test file reopen success and open failure: validate the replacement before swapping, leave the old sink usable on failure, drain in-flight writes, close once, and reject `copytruncate` semantics in documentation/comments.
5. Implement atomic immutable level snapshots, an expiry worker with deterministic shutdown, and an idempotent stdout/file logger lifecycle.

**Verify:** `go test -race ./internal/observe/logging`

## Task 4: Deterministic cardinality policy and Prometheus adapter

**Profile:** premium

**Files:**

- Create `internal/observe/metricspolicy/policy.go`
- Create `internal/observe/metricspolicy/policy_test.go`
- Create `internal/observe/prometheus/recorder.go`
- Create `internal/observe/prometheus/recorder_test.go`
- Modify `go.mod`
- Modify `go.sum`

**TDD:**

1. Test independent finite budgets for `tenant_id`, `model`, and `tool`; pinned-value precedence; invalid over-budget pins; reserved `__overflow__`; request-order/restart/replica determinism; and at-most-one replacement when a catalog member changes.
2. Use a documented versioned stable hash over `(dimension, value)` plus salt. Reconcile only on catalog/config changes, never on request-path arrival, TTL, or LRU.
3. Register the complete `fuse_` metric catalog from the spec with exact curated labels. Add tests rejecting undeclared labels and loop/node/sequence/trace/span IDs, display names, payloads, errors, URLs, and paths.
4. Exercise event projection into counters/gauges/histograms, overflow/admitted/budget diagnostics, process-local active-state cleanup, and Prometheus exposition through an `http.Handler`.

**Verify:** `go test -race ./internal/observe/metricspolicy ./internal/observe/prometheus`

## Task 5: OpenTelemetry adapter and causal propagation

**Profile:** premium

**Files:**

- Create `internal/observe/otel/observer.go`
- Create `internal/observe/otel/observer_test.go`
- Create `internal/observe/otel/propagation.go`
- Create `internal/observe/otel/propagation_test.go`
- Modify `internal/runtime/runtime.go`
- Modify `internal/runtime/inproc.go`
- Modify `internal/agent/agent.go`
- Modify `internal/agent/spawn.go`
- Modify `internal/model/adapter.go`
- Modify `internal/loopconnect/auth.go`
- Modify `internal/loopconnect/handler.go`
- Modify `sdk/fuse/remote.go`
- Modify `go.mod`
- Modify `go.sum`

**TDD:**

1. Use the OTEL SDK test exporter to assert the hierarchy `fuse.api.request > fuse.loop > store/model attempt/tool/spawn/pubsub`, bounded attributes, error/outcome mapping, and span closure on success, error, timeout, cancellation, retry, and early returns.
2. Thread the provider-neutral `observe.Observer` through `runtime.Deps` and agent construction. Instrument only where committed events cannot recover timing; keep no-op behavior byte-compatible when disabled.
3. Extract validated W3C context at the authenticated Connect edge, inject from the Go SDK, persist only repository-owned trace carrier strings, ignore malformed/untrusted carrier values, drop baggage, and derive tenant identity only from the authenticated principal.
4. Immediately consumed work continues the parent; delayed/replayed work starts a new trace with a span link. Expose the accepted server trace ID in response metadata for correlation.
5. Bound OTLP batching/queues/timeouts/shutdown and verify exporter outage or queue saturation records drops/errors without failing or indefinitely blocking a loop.

**Verify:** `go test -race ./internal/observe/otel ./internal/runtime ./internal/agent ./internal/loopconnect ./sdk/fuse`

## Task 6: Configuration, service composition, metrics and admin HTTP surfaces

**Profile:** premium

**Files:**

- Modify `internal/config/schema.go`
- Modify `internal/config/config_test.go`
- Modify `cmd/fuse/loop_serve_net.go`
- Modify `cmd/fuse/loop_serve_net_test.go`
- Create `cmd/fuse/observability.go`
- Create `cmd/fuse/observability_test.go`

**TDD:**

1. Add configuration coverage for independently enabled signals; `/metrics` bind/access policy; OTLP endpoint/protocol/TLS/headers/batching/queue/timeouts/sampling; stdout/file logging; log level and maximum override TTL; histogram buckets; cardinality budgets/pins/catalog/hash version/salt; and exact metric label declarations.
2. Invalid explicitly enabled adapters fail startup; disabled adapters require no backend configuration. Settings requiring SDK reconstruction are startup-only; logging reload validates a complete replacement before publishing it.
3. Compose projector, metrics, tracer, and logger in `loop-serve-net`, register `/metrics` under explicit access policy, and install authenticated administration endpoints for inspect/set/remove/reload/reopen. Reuse the existing verifier/principal model and enforce tenant authorization.
4. Ensure startup failures unwind every constructed exporter, worker, file, and projector; shutdown has deterministic finite deadlines and is idempotent.
5. Test unauthorized metrics/admin access, cross-tenant control denial, audit emission, per-instance identity reporting, and SIGHUP reload/reopen behavior.

**Verify:** `go test -race ./internal/config ./cmd/fuse`

## Task 7: Reference monitoring stack, dashboard, alerts, and operator documentation

**Profile:** standard

**Files:**

- Create `deploy/observability/docker-compose.yml`
- Create `deploy/observability/prometheus.yml`
- Create `deploy/observability/otel-collector.yml`
- Create `deploy/observability/tempo.yml`
- Create `deploy/observability/grafana/provisioning/datasources/datasources.yml`
- Create `deploy/observability/grafana/provisioning/dashboards/dashboards.yml`
- Create `deploy/observability/grafana/dashboards/fuse-loop.json`
- Create `deploy/observability/alerts.yml`
- Create `docs/observability.md`
- Modify `README.md`

**TDD/validation:**

1. Add deterministic config-validation tests or scripts that parse every YAML/JSON artifact and assert the expected services, scrape target, OTLP route to Tempo, provisioned Prometheus/Tempo sources, dashboard UID, and alert-rule names.
2. Dashboard panels cover throughput/outcome/latency/active loops; model retries/timeouts; tool/spawn failures; projector/export errors/drops; log overrides; and cardinality health. Queries are restart-safe and exclude `__overflow__` from tenant-specific alerts while a companion overflow alert reports incomplete attribution.
3. Document exact worst-case series estimates (including replicas), opaque-ID/display-name separation, trace/log tenant sensitivity, stdout/file collection, safe rename-create-reopen rotation, unsupported `copytruncate`, authenticated replay, retention/access ownership, and the Grafana → Tempo → logs → replay journey.
4. Mark the Compose stack development-only; include Prometheus, Grafana, OTEL Collector, and Tempo, and intentionally omit Loki.

**Verify:** run the artifact validation test plus `docker compose -f deploy/observability/docker-compose.yml config` when Docker Compose is available.

## Task 8: End-to-end acceptance, race gate, and resource-lifecycle audit

**Profile:** premium

**Files:**

- Create `cmd/fuse/observability_acceptance_test.go`
- Create `internal/observe/lifecycle_test.go`
- Modify `.github/workflows/integration.yml`
- Modify `Makefile`

**TDD:**

1. Add a hermetic acceptance lane using real Connect/SDK/runtime/event/projector/Prometheus and an in-memory OTEL exporter. Start a tenant-scoped loop with model/tool/spawn activity; assert metrics, nested spans, correlated payload-free JSON logs, trace metadata, and authenticated replay identity.
2. Add exporter/projector outage tests proving the loop remains available and bounded.
3. Add lifecycle tests for normal shutdown and each partial-initialization failure point; assert every goroutine, timer, sink, exporter, and projector closes exactly once.
4. Add a permanent race lane covering structured logging controls and observation fanout. Keep external Compose smoke as an opt-in target with loud skip/failure reporting, not as the sole acceptance proof.
5. Run formatting, vet, the full unit suite, and the focused race suite. Record the exact commands for docket build evidence.

**Verify:** `go test ./...`, `go test -race ./internal/observe/... ./internal/runtime ./internal/agent ./internal/loopconnect ./cmd/fuse`, and `go vet ./...`.

## Definition of done

- Disabled observability is a no-op and requires no monitoring backend.
- Provider-neutral core packages contain no vendor observability imports.
- Routine telemetry is demonstrably payload-free and tenant-safe.
- Prometheus labels are curated, deterministic, bounded, and expose overflow health.
- Trace context flows SDK → authenticated API → loop/model/tool/spawn/store/pubsub, with delayed work linked rather than parented.
- Structured logging, live controls, reload, file reopen, and shutdown pass concurrency/race tests.
- The reference stack and dashboard are source-controlled, parseable, and documented as non-production.
- The full suite and focused race suite are green, and build evidence points to the final branch HEAD.
