# 0067 — Auto-mode permission observability + deny-resilience — plan

Spec: `docs/superpowers/specs/2026-08-17-auto-mode-permission-observability-deny-resilience-design.md` (docket branch). Baselines and design rationale live there; this is the task order.

## Tasks

1. **Event surface** — `internal/event/event.go`: add `KindPermissionDecision "permission.decision"` + `PermissionDecisionPayload{ToolCallID, Tool, Verdict, Layer, Reason, Mode, Command}`; add optional `Reason` to `LoopTripPayload`. Pin in `event_test.go` kind table + payload round-trip.
2. **Typed denials** — `internal/tools/registry.go`: `Result` gains `Denied bool`, `DenyLayer string`.
3. **Decision sink + layers** — new `internal/permissions/decision.go`: `Decision` struct, `WithDecisionSink`/`decisionSinkFrom` (ctx-carry, mirrors `WithUserMessages`), `Layer*` constants (incl. `parse` for unparseable-bash asks — deviation from spec vocab, adds the #0070 target measurement; noted in results).
4. **Deny hints** — new `internal/permissions/denyhint.go` + tests.
5. **Gate refactor** — `gate.go`: `resolveAuto`/`classifyOrAsk`/`classifyWebFetch` return `(Verdict, layer, reason)`; `ToolPolicy` gains `DenyLayer`; `resolve` emits a `Decision` at every terminal outcome + pre-ask + post-human; `Execute` sets `Denied`/`DenyLayer` + appends hints.
6. **Valve recovery** — `gate.go`: `ValveApprovalToolName` sentinel; interactive trip → one `claimPrompt`-guarded approval, approve ⇒ `valve.reset()` (clears `promptedOnce`) and continue to classifier; reject ⇒ per-call asks, no re-prompt. Headless trip deny carries `DenyLayer:"valve"`. TUI: treat the new sentinel like the loop sentinel (`internal/tui/shell_model.go` isLoopApproval area).
7. **Loop resilience** — `internal/agent/loop.go`: `ErrAutoModePaused` sentinel; `executeTools` also returns `[]toolResult` (2 callers); `executeToolBounded` gains `turn`, installs the per-call decision sink stamping `ToolCallID`; denied-repeat tracker (`deniedCount`/`nudgedFP` by fingerprint) — skip `detector.seen` for all-denied-repeat turns, nudge (synthetic user msg + `KindUserInput` emit + `detector.reset()`) after 2 identical denials, post-nudge repeat → LoopApproval (interactive) / `ErrLoopDetected` (headless); valve-layer denial headless → emit error + return `ErrAutoModePaused`.
8. **Spawner collapse** — `cmd/fuse/run.go` `childResult`: add `ErrAutoModePaused` to the partial-success collapse.
9. **Tests** — per spec Tests section (gate decision-sink table, valve one-ask/reset/concurrency, loop nudge/post-nudge/valve-pause, event pins). Full `make test`.

## Verification

`make test`; then a live headless run against a cheap gateway model forcing repeated denials and a valve trip, confirming `permission.decision` events appear in `events.jsonl` and the run ends with one structured stop.
