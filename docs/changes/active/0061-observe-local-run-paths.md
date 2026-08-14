---
id: 61
slug: observe-local-run-paths
title: Wire observability into local run paths (fuse shell + one-shot + runtime bindings)
status: in-progress
priority: medium
type: feat
created: 2026-08-13
updated: 2026-08-14
depends_on: []
related: [51]
discovered_from: [51]
adrs: [40]
spec: docs/superpowers/specs/2026-08-13-observe-local-run-paths-design.md
plan: docs/superpowers/plans/2026-08-14-observe-local-run-paths-plan.md
results:
trivial: false
auto_groomable:
branch: feat/observe-local-run-paths
claimed_at: 2026-08-14T01:40:47Z
pr:
blocked_by:
reconciled: true
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-08-13-observe-local-run-paths-design.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/2026-08-13-observe-local-run-paths-design.md) |
| Plan | [2026-08-14-observe-local-run-paths-plan.md](https://github.com/ethanhinson/fuse/blob/feat/observe-local-run-paths/docs/superpowers/plans/2026-08-14-observe-local-run-paths-plan.md) |
| ADRs | [ADR-0040](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0040-provider-neutral-composite-observer.md) |
<!-- docket:artifacts:end -->

## Why

Change #0051 built the loop observability stack (OTEL traces, Prometheus metrics,
structured logs) as a projection over the event stream, but only wired it into a
single entry point: the `fuse loop-serve-net` networked server. Every other way
of running `fuse` — including `fuse shell` and one-shot `fuse <task>` — builds
agents on `observe.NoopObserver{}` and emits nothing, regardless of config.

Running `fuse shell` with observability configured therefore shows no telemetry.
That is a real gap for local dogfooding of the stack #0051 built. This change
closes it by extending the observer wiring to the local run paths.

## What changes

Piece 1 of a two-piece plan: thread a single, session-shared `observe.Observer`
into every locally-built agent (traces + operation metrics), defaulting to
`NoopObserver{}` for callers that don't opt in.

- Add an observer parameter to `buildAgentCore` / `buildAgentWithRendererAndTrace`
  (`cmd/fuse/run.go`) and call `a.SetObserver(observer)` on the built agent —
  this closes the **root-agent** gap, which exists on *every* path today
  (including the otherwise-wired `loop-serve-net`).
- In `fuse shell` (`runShell`), construct the observe layer once via
  `newObservability(...)` and thread the shared observer through the `build`
  closure into root + every child agent; start the metrics endpoint when
  `metrics.bind` is set (nil verifier; `metrics.access: public` acceptable for a
  local `127.0.0.1` bind).
- Give the one-shot `fuse <task>` path the same treatment through the shared
  build seam.
- Replace the `observe.NoopObserver{}` hardcodes in `runtime_binding.go`
  (lines 87, 304, 539 — one-shot, research-probe, shell) with an observer
  supplied by the entry point. The child-agent and Spawner wiring at those
  bindings (`a.SetObserver` / `agent.WithObserver`) already exists; only the
  hardcoded value is the defect.

Settled behavior: metrics-endpoint bind failure warns and continues; invalid
observability config fails shell startup fast; one observer instance is shared
across the session. Telemetry stays gated behind the existing config opt-ins
(`observability.metrics.enabled` / `traces.enabled` / `logging.enabled`).

## Out of scope

- **Piece 2** (explicit follow-up): payload-free JSON access-log projection over
  the shell's event stream. The shell uses the legacy `EventStore` interface, not
  the tenant-scoped `DurableStore`/`CommittedDurableStore` the serve path's
  `projectingDurableStore` / `observe.Runner` build on, so those helpers can't be
  reused verbatim — Piece 2 must mirror `startProjectedLogConsumer`.
- Any bearer-token / operator auth for the shell metrics endpoint (local,
  public-on-loopback only).
- Changes to the already-wired `loop-serve-net` path.
- Trace-carrier-on-the-wire for remote clients (by design in #0051; not a bug).

## Open questions

- ~~Confirm during reconcile whether one-shot `fuse <task>` already flows through
  the same `buildAgentCore` helper, or needs its own wiring site.~~ **Resolved
  2026-08-14:** it does — `main.go:170` → `buildOneShotRuntimeDeps` →
  `Deps.BuildAgent` → `buildAgentCore` (`runtime_binding.go:245`). No separate
  wiring site; the observer is constructed in `run()` and passed into
  `buildOneShotRuntimeDeps`.

## Reconcile log

<!-- Appended by docket-implement-next's reconcile pass: dated entries of what changed. -->

### 2026-08-14

Reconciled against `origin/main` @ `e6e637f` (verified via `git show origin/main:…`,
not the local working tree). Design decisions all still hold; three mechanism
re-maps and one scope clarification.

1. **API re-map — `SetObserver`, not `a.WithObserver`.** The spec's "call
   `a.WithObserver(observer)` on the built agent" names an API that does not
   exist. The agent-side setter is `func (a *Agent) SetObserver(o observe.Observer)`
   (`internal/agent/agent.go:146`; nil → Noop, and it also fans out to the
   summarizer and the relevance classifier). `agent.WithObserver`
   (`internal/agent/spawn.go:265`) is a **Spawner `Option`**, already used
   correctly at the binding sites. Mechanism re-map only — the decision
   ("thread one shared observer into every locally-built agent") is unchanged.

2. **Smaller than scoped for children; the real gap is the ROOT agent.** All
   three bindings already thread their local `observer` into children and
   spawners: `a.SetObserver(observer)` at `runtime_binding.go:180 / 392 / 617`
   and `agent.WithObserver(observer)` at `:200 / 410 / 662`. The only defect
   there is that `observer` is the hardcoded `observe.NoopObserver{}` at
   `:87 / 304 / 539`. Separately — and **not previously known** — no path
   anywhere calls `SetObserver` on the ROOT agent, including the fully-wired
   `loop-serve-net` path (`loop_server.go:282` builds the root via
   `buildAgentCore` and never sets an observer). So the `buildAgentCore` /
   `buildAgentWithRendererAndTrace` parameter is the piece that actually closes
   the root gap, everywhere.

3. **Scope clarification — `loop_server.go` call sites are touched
   mechanically.** Adding the parameter to `buildAgentCore` necessarily updates
   its two `loop_server.go` call sites (`:239` child, `:282` root); both receive
   that binding's existing `observer` value. This incidentally makes the
   loop-server/serve-net ROOT agent observed. That is the natural completion of
   #0051's own intent, not a design change to the serve path, so "changes to the
   already-wired `loop-serve-net` path" stays out of scope in the sense meant
   (no composition or config change). `loop_server.go:94`'s
   `observe.NoopObserver{}` remains as-is — it is the deliberate no-observer
   default of the non-`WithObserver` variant.

4. **Wiring sites enumerated by grep at build time** (per the
   `patch-every-cloned-child-builder` learning — cmd/fuse clones this wiring in
   three builders): entry points are `main.go run()` (one-shot),
   `shell.go runShell()` (shell), and `research_probe.go` (research probe). Each
   constructs the observe layer once and passes `obs.observer` into its deps
   builder. `runShell` takes no `ctx` — use `context.Background()` (`context` is
   already imported) and the existing `stderr`. Note the shell has **two** root
   build sites that both need the observer: the TUI `build` closure
   (`shell.go:234`) and `buildShellRuntimeDeps`'s `BuildAgent`
   (`runtime_binding.go:696`).

5. Verified unchanged and still correct: `newObservability(ctx, cfg, stdout)
   (*observabilityService, error)` at `observability.go:99` (calls
   `cfg.Validate()` first → fail-fast holds); `obs.observer` field;
   `Close(ctx) error` at `:193`; `startMetricsEndpoint(ctx, verifier) error` at
   `:227` (nil verifier acceptable). Piece 2's premise also still holds: the
   shell still opens the legacy `fsstore.FSEventStore` (`shell.go:197`), not a
   `DurableStore`.

No obsolescence, no fundamental invalidation. Auto-capture is disabled for this
repo, so adjacent findings are reported in prose only.
