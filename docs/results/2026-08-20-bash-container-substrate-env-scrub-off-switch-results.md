<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0063 — bash container substrate + env-scrub + off-switch — the sandbox container behind a pluggable OCI runtime seam](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0063-bash-container-substrate-env-scrub-off-switch.md)**
<!-- docket:backlink:end -->

# 0063 — bash container substrate + env-scrub + off-switch — results

> Change: **#0063** · Plan: `docs/superpowers/plans/2026-08-20-bash-container-substrate-env-scrub-off-switch-plan.md` · Decision of record: **ADR-0044**

## What shipped

The ambient-credential inheritance hole is closed by construction. `internal/tools/bash.go` previously
ran `/bin/sh` with `cmd.Env` never set, so the child inherited fuse's entire process environment. Both
substrates now apply the same explicit allowlist, and there is no code path that inherits.

13 plan tasks (T1–T13), then 6 review-fix commits. Whole-branch diff: 71 files, ~8,875 insertions.

## Merge-gate checks the human should run

Automated tests do not cover these:

1. **Local dogfooding still works.** `make build && ./fuse run 'run ls in bash'` on a machine with
   Docker. The container tier is now the zero-config default, so this is the first run where `bash`
   goes through a real container.
2. **The refusal is legible.** Stop Docker (`colima stop` / quit Docker Desktop) and run the same
   command. `bash` must REFUSE with a clear message, not run on the host. This is the intended
   ADR-0044 behaviour and the most visible change for anyone without a container runtime.
3. **The off-switch.** Create `.fuse/sandbox.local.yml` with `contained: false`, confirm `bash` runs on
   the host **with a scrubbed environment** (`env` must not show your API keys), then delete it and
   confirm containment returns.
4. **Cold-start feel.** The warm pool exists to keep a loop that calls `bash` repeatedly usable. Worth
   a subjective check on a multi-call task.

## Behaviour changes worth knowing

- **No container runtime + no off-switch file ⇒ `bash` refuses.** By design (ADR-0044 fail-closed),
  but it changes local dev for anyone without Docker.
- **`working_dir` outside the trusted root is now an error result to the model** rather than silently
  working. In-repo absolute paths, relative paths, and the repo root all resolve normally.
- **The shell binding was leaking its sandbox pool for the whole session** — its `LoopTeardown` was
  dead code, since the TUI drives turns and the `runtime.New(...)` result is discarded. Fixed.

## Review

`docket-review-deep` (rung selected from the highest build profile, `premium`; the >1500-line diff
modifier was already capped). **11 findings: 4 blocker, 2 important, 5 minor. All 11 fixed in-branch.**

The most serious was a **security inversion**: model-authored `working_dir` *defined* the container's
host bind-mount root, so a model emitting `{"working_dir":"/"}` got the host root mounted read-write at
`/workspace` in a root-user container — recovering by filesystem exactly the credential access the
env-scrub closed. The fix's RED output reproduced it verbatim (`working_dir "/" was accepted; argv:
[... "-v", "/:/workspace" ...]`). The trusted root is now always the mount source, and `working_dir` is
resolved as a contained subpath with escapes refused.

Two findings were confirmed NOT to be problems, and the reasoning is worth keeping: the container CLI
client's env passthrough cannot widen the container's env (the container env rides entirely in argv as
`--env K=V`), and the smoke test's `PWD`/`HOSTNAME` tolerance cannot hide a leak (the test asserts the
secret's *value* is absent from the whole output first).

## Design decisions recorded here rather than as ADRs

Both implement ADR-0044's existing "fail-safe, never fail-open" decision rather than extending it, so
neither was minted as a new decision of record. Flagging the judgment for the merge gate:

- **`NewService` returns a nil error when no container runtime exists.** The refusal
  (`ErrRefusedUncontained`) surfaces at `Acquire` instead. A construction error invites a caller-side
  `if err != nil { fall back }` whose only destination is uncontained execution.
- **The off-switch config is loaded ONCE at startup against a trusted resolved root, never per call.**
  A model that can write files could otherwise author its own `.fuse/sandbox.local.yml` mid-loop and
  turn containment off. `TestServiceMethodsNeverReloadConfig` parses `service.go` and fails if any
  `*Service` method reaches the filesystem.
- **`WithTrustedRoot` is applied LAST by `NewServiceFromRoot`**, so no caller-supplied option can
  redirect the mount source.

## Plan deviations

- **OTEL-native metrics were not built.** `internal/observe/otel` is a tracing-only observer with no
  meter instruments anywhere in the repo; standing up a MeterProvider/exporter is its own change. The
  Prometheus recorder carries all five `fuse_sandbox_*` families.
- **The loop→container dashboard panel was dropped, and with it that operator capability.** It had been
  built as a Tempo traceQL panel querying `fuse.sandbox.*` spans that are not producible — the sandbox
  package emits events and never opens spans, by design. Restoring it needs a logs datasource, a
  shipper, and a `container_id` log field (`Logger.Project` currently drops it). The plan still lists it
  at line 228; merged plans are frozen build records, so it was left untouched.
- **`internal/runtime/inproc.go` was not modified.** The existing `LoopTeardown` seam already fires on
  both the completion goroutine and the BuildAgent early return; tests pin that rather than adding a
  new lifecycle.
- **`event.KindSandboxHealth` has no production emitter.** `sandbox.PoolHooks` surfaces no health
  transition, so `fuse_sandbox_unhealthy_total` and its alert are defined but unfed. The projector
  handles the kind, so this is a no-op rather than a break.

## Follow-ups

- Egress/network policy is **#0064** (`--network` is deliberately left at the CLI default, marked
  `TODO(#0064)`); per-tenant filesystem isolation is **#0065** (`TODO(#0065)` retained on the mount
  line, now correctly scoped to per-tenant subdivision only).
- The loop→container capability, and a health signal for `KindSandboxHealth`, are each their own change.
  Auto-capture is disabled for this repo, so no stubs were minted.
- `sandbox.containerIdentified` is unimplemented, so `ContainerID` is always `""` (`run --rm` means no
  container outlives an `Exec`). Once a persistent-container substrate lands and ids become distinct,
  the recorder's remaining same-tenant/same-handler runtime collision resolves on its own.

## Verification

`make test` green at `4b8222d`. T13's end-to-end smoke test **ran against real Docker** (not skipped)
and passed, proving containment and env-scrub against a live runtime. No live model traffic was needed;
where any is ever required this project uses cheap gateway models, never Anthropic.

### Observability verified end-to-end against real containers (follow-up)

The build left an observability gap: the emitter side (`TestBashPoolEmitsSandboxLifecycleEvents`)
stopped at the event stream, and the projection side (`prometheus/sandbox_metrics_test.go`) started
from hand-built `observe.Record`s. Neither ran a real container and then confirmed its lifecycle moved
real `fuse_sandbox_*` series on a live `/metrics` scrape — so the emitter→projector→recorder plumbing
was only ever proven in halves.

`TestContainerLifecycleFeedsSandboxMetricsEndToEnd` (`internal/tools/sandbox_metrics_e2e_test.go`) closes
it. Gated on a real container CLI like T13, it drives the **production chain end-to-end with nothing
rebuilt**: a real container cold-acquire → exec → release (warm) → warm-reuse → `Close` reap through the
production `sandbox.Pool` hooks → `SandboxEventHooks` → a real `FSEventStore`, then `Replay` → production
`observe.ProjectEvent` → production `prometheus.Recorder` → the real `/metrics` handler. It asserts the
scrape shows `fuse_sandbox_acquire_total` for both a cold spawn and a warm reuse, a single
`fuse_sandbox_cold_start_seconds_count`, a `fuse_sandbox_reaped_total{cause="loop_end"}`, and — the leak
guard — `fuse_sandbox_active … 0` after teardown, all with the **real** handler/runtime labels read back
off the scrape. A negative-control flip of the reap cause was confirmed to fail the test, so it is not
vacuous. Ran against real Docker (29.4.0), passed, not skipped.

This confirms three of the four sandbox families are fed and correct end-to-end from a live container.
The fourth, `fuse_sandbox_unhealthy_total`, remains **unfed by construction**: `KindSandboxHealth` still
has no production emitter (see the known gap above). The E2E test pins this — it asserts the family gains
no series from a real run, so the day an emitter is added the guard fails and forces the E2E coverage to
be extended to drive and assert a real unhealthy transition rather than leaving the family unproven.
