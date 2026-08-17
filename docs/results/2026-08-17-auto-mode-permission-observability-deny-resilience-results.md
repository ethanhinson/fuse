# 0067 — Auto-mode permission observability + deny-resilience — results

Change: `docs/changes/active/0067-auto-mode-permission-observability-deny-resilience.md` (docket branch) · Spec: `docs/superpowers/specs/2026-08-17-auto-mode-permission-observability-deny-resilience-design.md` · Plan: `docs/superpowers/plans/2026-08-17-auto-mode-permission-observability-deny-resilience-plan.md`

## What landed

- **`permission.decision` event** (`internal/event/event.go`): every gate resolution — allow/ask/deny, deciding layer, mode, bounded command preview, tool-call ID. Asks are emitted BEFORE the approval func and the human outcome after, so headless `AlwaysApprove` bindings show as back-to-back ask→allow (the ADR-0028 posture is now auditable). Additive wire change; kind pinned in `event_test.go`.
- **Decision sink** (`internal/permissions/decision.go`): ctx-carried `DecisionSink` mirroring `WithUserMessages`; installed per call in `executeToolBounded` (which gained `turn`). Layer vocabulary as `Layer*` constants.
- **Typed denials** (`internal/tools/registry.go`): `Result.Denied` / `Result.DenyLayer`, set only by the gate. Denial outputs carry per-layer actionable hints (`internal/permissions/denyhint.go`).
- **Deny→retry resilience** (`internal/agent/loop.go`): policy-denied repeats bypass the generic doom-loop abort; after 2 identical denials a `[policy]` nudge is injected as a user turn AND emitted as `user.input` (resume-fold fidelity, change 0054); only post-nudge repetition ends the run (interactive: `LoopApproval` prompt with policy-aware preview; headless: `ErrLoopDetected` naming the nudge). `LoopTripPayload` gained optional `reason`.
- **Recoverable valve** (`internal/permissions/gate.go`): interactive trip issues ONE `ValveApprovalToolName` recovery prompt (rendered by the TUI without the session option); approval resets both counters and the pending call proceeds to the classifier; rejection falls back to per-call asks with no re-prompt (`promptedOnce` under `valve.mu`, safe across `CloneForChild`). Headless trip returns `ErrAutoModePaused` — one structured stop, tool-result pairing intact — and the spawner collapses it to a partial result like `ErrMaxTurns`.

## Verification

- `make test`: full suite green (38 packages), including new coverage: decision-sink layer attribution per scenario, valve one-ask/reset/rejection/no-double-prompt, nudge injection + post-nudge abort + tracker-clear, headless `ErrAutoModePaused`, end-to-end gate→loop→store emission (`TestPermissionDecisionEmitted`).
- **Live run** (cheap gateway model `deepseek-flash`, one-shot auto mode): asked the model to run `curl -s https://example.com` and retry on refusal. Observed: two rules-layer denials each carrying the hint ("…use the web_fetch or web_search tool instead"), model **stopped retrying on its own** and summarized cleanly — no loop-detector death, run exited 0. This is the exact failure shape that previously produced 11 `loop.detector.trip` run-kills in session logs.
- Baseline for later stages (from pre-change session logs, 2026-08-17): 62 rules denies / 32 web_fetch classifier denies / 19 bash classifier denies / 8 valve trips / 11 loop trips. Stage B–D (#0068–#0070) reduce the deny volume; the events landed here are how that reduction gets measured.

## Deviations from spec

- Added a `parse` layer (unparseable-bash asks) beyond the spec's vocabulary list — it measures exactly the shapes change #0070 will fix; strictly additive.
- The nudge text references the tool and layer but not the per-layer hint verbatim (the hint already arrived in the denial results); wording kept shorter.
- `resolve` reads the live mode once at entry (before mediation) so decision events can carry it; mediation remains mode-independent and terminal — behavior unchanged, ordering of the mode read is the only difference.

## Notes for the stack

- One-shot runs don't persist a session event store, so `permission.decision` events appear in shell and loop-server sessions (where `events.jsonl` exists). The 0051 OTEL projector safely ignores the new kind until a mapping is added (optional follow-up).
