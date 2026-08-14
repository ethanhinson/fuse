<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0061 — Wire observability into local run paths (fuse shell + one-shot + runtime bindings)](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0061-observe-local-run-paths.md)**
<!-- docket:backlink:end -->

# Wire observability into local run paths — results

Change: #0061 · Branch: feat/observe-local-run-paths · PR: (see change `pr:`) · Plan: docs/superpowers/plans/2026-08-14-observe-local-run-paths-plan.md · ADRs: 40, 42

## Verify (human)

Automated tests cover the wiring; these need a real collector, a real terminal, and a real scrape.

- [ ] **Traces reach a collector with correct nesting.** With `observability.traces.enabled: true` pointed at a local OTLP collector, run `fuse shell`, issue one turn that makes a tool call and a spawn, then confirm the spans land with root turn → tool → spawned child nesting intact. The suite asserts a `fuse.model.attempt.complete` span reaches an in-process collector double, but not the nesting shape against a real backend (Tempo/Jaeger).
- [ ] **Metrics series appear with expected labels.** With `metrics.enabled: true`, `metrics.bind: 127.0.0.1:9464`, `metrics.access: public`, run a shell turn then `curl 127.0.0.1:9464/metrics` and confirm `fuse_loop_operations_total`, `fuse_tool_calls_total`, and `fuse_spawn_operations_total` are present with the labels you expect. Cardinality-policy label output is not asserted end-to-end anywhere.
- [ ] **The TUI is not corrupted by logging.** With `observability.logging.enabled: true` and `logging.output: stdout`, run `fuse shell` in a real terminal and confirm the alt-screen renders cleanly. This is the hazard `05978bf` fixed; the test asserts the sink writer, not the rendered frame.
- [ ] **`access: authenticated` warns rather than silently 401ing.** Set `metrics.access: authenticated` with a `metrics.bind` and confirm `fuse shell` prints the skip warning on stderr and still starts. (Asserted in tests; worth one human look since it is a new refusal a user may hit.)
- [ ] **No-observability-config behavior is unchanged.** Run `fuse shell` and a one-shot `fuse "<task>"` with no `observability` block and confirm nothing about the experience differs from `main`. This is the change's core regression promise.

## Findings

- **The `runtime.Deps.Observer` field is the load-bearing wiring on the `StartLoop` path, not `BuildAgent`.** `internal/runtime/inproc.go:306` (`a.SetObserver(r.deps.Observer)`) and `:596` (`agent.WithObserver(r.deps.Observer)`) **overwrite** whatever `BuildAgent` installed. The spec's design — thread the observer through `buildAgentCore` — is necessary but was **not sufficient**: without also publishing `Observer: observer` into each binding's `runtime.Deps`, every local root agent was silently reset to `NoopObserver{}` the moment the Runtime drove the loop. Caught only by an entry-point test; a seam test that calls `Deps.BuildAgent` directly cannot see it. Both test shapes are now kept deliberately (`observe_wiring_test.go`).

- **No path anywhere was observing the ROOT agent — including the already-wired `loop-serve-net`.** The three local bindings already threaded their observer into *children* and *spawners*; only the hardcoded `observe.NoopObserver{}` value was wrong there. The genuinely missing piece was root-agent wiring, absent on every path. Threading the parameter through `buildAgentCore` closed it for the serve path too, as a mechanical consequence of the signature change.

- **Local entry points cannot inherit the server's observability I/O posture.** Two concrete ways: the log sink collides with the writer bubbletea owns, and `metrics.access: authenticated` with a nil verifier binds a listener that 401s every scrape forever while reporting success. Both are now handled at the local caller. Recorded as **ADR-0042**.

## Follow-ups

- **`TestObservabilityAcceptanceHermetic` is flaky on `origin/main`, independent of this change.** Measured by the build worker at base `e6e637f`: **4/10** failures on base vs **2/10** on this branch, same symptom — `fuse.loop.run` absent from the exported spans because the loop span has not ended when `ForceFlush` runs (`waitForTurnEndByObserve` waits for *turn* end, not *loop* end). It also leaves a `TempDir RemoveAll cleanup` warning on failure. Pre-existing; deserves its own change. Not touched here.

- **Piece 2 remains the explicit follow-up:** payload-free JSON access-log projection over the shell's event stream. The shell still opens the legacy `fsstore.FSEventStore`, not the tenant-scoped `DurableStore`/`CommittedDurableStore` the serve path's `projectingDurableStore` / `observe.Runner` build on, so those helpers cannot be reused verbatim — Piece 2 must mirror `startProjectedLogConsumer`.

- **Shell metrics bearer/operator auth** stays a non-goal; a local run that wants a scrape endpoint must use `access: public` on a loopback bind (ADR-0042).

- **`buildAgentCore` / `buildAgentWithRendererAndTrace` now take 14–15 positional parameters.** An options-struct refactor was deliberately deferred out of this change to keep the reviewed diff readable. Worth its own mechanical change.
