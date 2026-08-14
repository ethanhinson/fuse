<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0061 — Wire observability into local run paths (fuse shell + one-shot + runtime bindings)](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0061-observe-local-run-paths.md)**
<!-- docket:backlink:end -->

# Wire observability into local run paths — design (change #0061)

## Context

Change #0051 built the loop observability stack (OTEL traces + Prometheus metrics
+ structured logs) as a payload-free projection over the event stream. But its
production wiring (`newObservability`) is invoked from **exactly one** entry
point — the `fuse loop-serve-net` networked Connect server (`cmd/fuse/loop_serve_net.go:196`).
Every other way of running `fuse` builds agents on `observe.NoopObserver{}`, which
emits nothing:

- `fuse shell` (interactive) and `fuse <task>` (one-shot) build agents via
  `buildAgentCore` (`cmd/fuse/run.go:803`), which **never calls `agent.WithObserver`**.
- `fuse loop-server` (stdio JSON-RPC) hardcodes `NoopObserver{}` (`cmd/fuse/loop_server.go:94`).
- The `runtime_binding.go` bindings hardcode `NoopObserver{}` (~lines 87, 304, 539).

A user running `fuse shell` therefore sees no telemetry regardless of config —
working as designed for #0051's scope, but a real gap for local dogfooding.

This change is **Piece 1** of a two-piece plan agreed with the human: wire the
**observer** (traces + operation metrics) into every local run path. **Piece 2**
(payload-free JSON-log projection over the event stream) is an explicit follow-up.

## Decision

### Scope (Piece 1 — this change)

Thread a single, session-shared `observe.Observer` into every locally-built
agent so `agent.WithObserver(observer)` is set, defaulting to `NoopObserver{}`
for callers that don't opt in. Concretely:

1. **`buildAgentCore` / `buildAgentWithRendererAndTrace` (`cmd/fuse/run.go`)** —
   add an `observer observe.Observer` parameter (last positional, or grouped with
   the other session deps). Call `a.WithObserver(observer)` on the built agent on
   both the gateway-adapter path and the `cli/` adapter path. A `nil`/omitted
   observer resolves to `observe.NoopObserver{}` so existing one-shot behavior is
   byte-unchanged.

2. **`fuse shell` (`cmd/fuse/shell.go` `runShell`)** — construct the observe layer
   once via `newObservability(ctx, cfg, stderr)` (the same constructor
   `loop-serve-net` uses). Thread `obs.observer` through the `build` closure
   (shell.go:231) into `buildAgentWithRendererAndTrace`, so the root agent AND
   every child agent it builds share the one observer. `defer obs.Close(ctx)`.
   If `metrics.bind` is set, start the scrape endpoint via
   `obs.startMetricsEndpoint(ctx, nil)` — nil verifier, since the shell has no
   bearer-token auth; `metrics.access: public` is acceptable for a local
   `127.0.0.1` bind.

3. **`fuse <task>` one-shot** — same treatment as shell if it shares the build
   path; wire the observer through the same `buildAgentCore` seam. (Confirm the
   one-shot entry point during reconcile — it may already flow through the same
   helper.)

4. **`runtime_binding.go` NoopObserver hardcodes (~87, 304, 539)** — replace with
   the observer constructed from config via `newObservability`, matching how
   `loop_serve_net.go` composes `obs.observer` into
   `buildLoopServerRuntimeDepsWithObserver` and `handler.WithObserver`.

### Behavioral decisions (settled with the human)

- **Bind failure → warn & continue.** A configured-but-unbindable metrics
  endpoint (e.g. port in use) logs to stderr and the shell keeps running with
  traces/logs still active. Observability never breaks the primary tool.
  (`startMetricsEndpoint` returns an error today; the shell caller warns instead
  of aborting.)
- **Invalid observability config → fail fast.** `newObservability` calls
  `cfg.Validate()`; a validation error refuses shell startup with the message
  surfaced, identical to `loop-serve-net`. Consistent, fix-once behavior — NOT a
  silent degrade to Noop.
- **One shared observer per session.** Built once in `runShell`, threaded into
  root + every child via the build closure. Child spans parent correctly under
  one provider/recorder, and Prometheus collectors register exactly once. Matches
  the serve-path composition (`metricsObserver{primary, metrics}`).

### Config gating (unchanged from #0051)

Telemetry stays **off unless config opts in**: `observability.metrics.enabled`,
`observability.traces.enabled`, `observability.logging.enabled`. Empty config →
`newObservability` returns a service whose `observer` is `NoopObserver{}`, so the
shell behaves exactly as today. The metrics HTTP endpoint additionally requires a
non-empty `metrics.bind`.

## Consequences

- **Enables**: `fuse shell` (and one-shot / local bindings) emit OTEL traces and
  Prometheus operation metrics when config enables them — local dogfooding of the
  observability stack #0051 built, per the "dogfood fuse's own infra" preference.
- **Costs**: `buildAgentCore`'s signature grows one parameter; every caller
  (shell, one-shot, child builds, tests) updates to pass an observer (Noop for
  the ones that don't opt in). Mechanical but touches several call sites.
- **Given up / deferred to Piece 2**: no payload-free JSON access-log projection
  in the shell yet. The shell uses the **legacy `EventStore`** interface
  (`fsstore.FSEventStore`: no `StreamKey`, no `AppendCommitted`), NOT the
  tenant-scoped `DurableStore`/`CommittedDurableStore` the serve path's
  `projectingDurableStore` / `observe.Runner` are built on — so those helpers
  can't be reused verbatim. Piece 2 must mirror `startProjectedLogConsumer`
  (shell.go:215), subscribing to the legacy store and calling
  `observe.ProjectEvent(key, e)` with a synthesized local `StreamKey`
  (local/default tenant + root id as loop).

## Out of scope

- Piece 2: JSON-log projection over the shell's event stream.
- Any bearer-token / operator-capability auth for the shell metrics endpoint —
  local, public-on-loopback only.
- Changes to the `loop-serve-net` path (already wired).
- Trace-carrier-on-the-wire for remote clients (by design in #0051; not a bug).

## Verification

1. `go build ./...` and `go test ./cmd/fuse/... ./internal/observe/...`.
2. With an observability-enabled config (`metrics.enabled: true`,
   `metrics.bind: 127.0.0.1:9464`, `metrics.access: public`, `traces.enabled`
   pointing at a local OTLP collector), run `fuse shell`, issue a turn with a
   tool call and a spawn, then `curl 127.0.0.1:9464/metrics` and assert the
   `fuse_loop_operations_total` / `fuse_tool_calls_total` / `fuse_spawn_operations_total`
   series appear with the expected labels; confirm spans land in the collector /
   Tempo with correct parent/child nesting (root turn → tool → spawned child).
3. With `metrics.bind` pointed at an already-bound port, confirm the shell warns
   and still runs (bind-failure decision).
4. With a deliberately invalid observability block, confirm `fuse shell` refuses
   to start with the validation error (fail-fast decision).
5. With no observability config, confirm `fuse shell` behaves exactly as before
   (NoopObserver, no endpoint, no output).
