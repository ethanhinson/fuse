<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0067 — Auto-mode permission observability + deny-resilience — measure every gate decision, stop dying on denials](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/archive/2026-08-17-0067-auto-mode-permission-observability-deny-resilience.md)**
<!-- docket:backlink:end -->

# Auto-mode permission observability + deny-resilience — design

**Change:** #0067 · Stage A of the auto-mode overhaul (Claude Code / Cursor parity arc: #0067 → #0068 → #0069 → #0070)

## Problem

Auto-mode runs die instead of adapting, and the gate's behavior is unmeasurable:

- A policy denial returns a plain `tools.Result{IsError: true}` string; models retry the identical call verbatim → the loop detector (`loopLimit=3`) kills the run with `ErrLoopDetected`. 11 such trips in recent local sessions were denial amplification, not real loops.
- The escalation valve (3 consecutive / 20 total classifier blocks) is **permanent once tripped**: `classifyOrAsk` short-circuits at `valve.tripped()` and never records again, so interactive sessions degrade to per-call asks forever and headless runs cascade denials until loop-death. 8 trips observed.
- No permission event kind exists. Denies are only visible as tool-result strings; asks (human prompts) and headless `AlwaysApprove` resolutions (loop_serve_net.go, ADR-0028) are completely invisible, so prompt frequency — the core UX metric for auto mode — cannot be measured.

Baseline numbers to beat (from `~/.fuse/sessions/*/events.jsonl`, 2026-08-17): 62 rules-layer denies, 32 web_fetch classifier denies, 19 bash classifier denies, 8 valve trips, 11 loop-detector trips.

## Design

### D1. `permission.decision` event for every gate resolution

- **Ctx-carried sink** mirroring the existing `WithUserMessages` seam (`internal/permissions/context.go`) so `internal/permissions` stays a leaf package: `type Decision struct { Tool, Verdict, Layer, Reason, Mode, Command string }`, `WithDecisionSink(ctx, func(Decision))`, nil-safe `decisionSinkFrom`.
- Refactor `resolve`/`resolveAuto`/`classifyOrAsk`/`classifyWebFetch` (`internal/permissions/gate.go:329-587`) to carry `(verdict, layer, reason)` triples to a **single emission choke point**. Layer vocabulary (const strings): `mediation | disabled | mode_off | cache | rules | safelist | heuristic | classifier | fetch_floor | edit_scope | valve | human | smart_config`.
- Emit `verdict=ask` **immediately before** `g.approve` is invoked (gate.go:393-399) and a `layer=human` allow/deny after it. This makes asks countable regardless of the approval binding — a headless `AlwaysApprove` shows as back-to-back ask→allow events with sub-ms spacing, making the ADR-0028 posture auditable.
- New `KindPermissionDecision Kind = "permission.decision"` + `PermissionDecisionPayload{ToolCallID, Tool, Verdict, Layer, Reason, Mode, Command}` in `internal/event/event.go`. Additive-only: the resume fold (`internal/runtime/reconstruct.go:105` default case) and the OTEL projector (`internal/observe/projector.go:85`) skip unknown kinds safely.
- The agent loop installs the sink **per tool call** in `executeToolBounded` (`internal/agent/loop.go:836-879`, gains a `turn` param — two call sites) so `ToolCallID` is stamped.
- Hygiene: `Command` populated for bash only, truncated ~200 chars; other tools get `makePreview`-style truncation; never raw args (tool.call already carries them).

### D2. Typed denials + actionable hints

- `tools.Result` (`internal/tools/registry.go:15-18`) gains `Denied bool`, `DenyLayer string`. Only the gate sets them; its denial results return directly from `Execute` (gate.go:316-324), never wrapped by registry/spill paths. Zero values keep every existing constructor valid.
- New `internal/permissions/denyhint.go`: `denyHint(layer, tool, command) string` appended to denial output — egress → "use web_fetch/web_search instead"; out-of-workspace write → "write inside the workspace root or scratch directory"; generic → "retrying the identical call will fail — change the command or ask the user".

### D3. Deny→retry no longer kills the run

In `Run` (`internal/agent/loop.go`), a fingerprint-keyed tracker alongside the existing detector (`deniedCount map[string]int`, `nudgedFP map[string]bool`, reusing `fingerprint()`):

- Turns whose tool calls are **all** policy-denied repeats skip `detector.seen` (policy denials no longer feed generic `ErrLoopDetected`).
- After **2 identical denials**: inject a synthetic user message — `[policy] The call to <tool> was denied twice by the <layer> policy layer. Repeating the identical call will keep failing. Change approach: <hint>` — call `detector.reset()`, and **emit `KindUserInput` for the injection**. The emission is mandatory: change-0054 resume folds rebuild transcripts from events; an unemitted injected message diverges on resume.
- Repeat **after** the nudge: interactive (`a.LoopApproval != nil`) → approval prompt with a policy-aware preview; headless → `fmt.Errorf("%w: %s repeated after policy-denial nudge", ErrLoopDetected, names)` — preserves `errors.Is` contracts (`childResult`, cmd/fuse/run.go:521-531).
- One nudge per fingerprint; distinct denied commands get independent budgets; non-denied loop detection is untouched.
- `executeTools` additionally returns its `[]toolResult` (already built internally; 2 callers).

### D4. Valve trips become recoverable

- **Interactive**: `valveTripped()` (gate.go:579) issues ONE synthetic approval — new sentinel `ValveApprovalToolName` beside `LoopApprovalToolName` (gate.go:15-19), preview "auto mode has denied N commands (M in a row) — continue in auto mode?". Approve → `valve.reset()` (zeroes both counters, curing total-limit stickiness) and fall through to the classifier for the pending call. Reject → per-call asks; a `promptedOnce` flag under `valve.mu` prevents re-prompting (valve is shared by reference across `CloneForChild` — parallel children must not double-prompt). TUI: extend the sentinel check at `internal/tui/shell_model.go:1440` so the valve prompt renders session-option-less.
- **Headless**: valve denial carries `DenyLayer:"valve"`; after `executeTools` returns (tool-result messages already appended, provider pairing stays valid), the loop returns new sentinel `ErrAutoModePaused` with the valve summary — one structured stop instead of a denial cascade. Added to the partial-success collapse in `childResult` so a child's completed work survives.
- `valveTripped` gains ctx + pending name/args (both call sites — `classifyOrAsk`, `classifyWebFetch` — have ctx).

## Tests

- `gate_test.go`: recorded `DecisionSink` assertions per scenario (rules deny / safelist allow / cache allow / mode-off allow / mediation deny / ask→human); `Denied`/`DenyLayer` on `Execute` results; denyhint units.
- `valve_test.go`: trip produces exactly one valve ask; approval resets counters and the classifier is consulted again (stub call-count advances); headless trip → `Denied` + `DenyLayer=="valve"`; no-double-prompt concurrency across cloned children.
- `loop_test.go` (following `TestRunLoopDetectionAborts/ForcesApproval`): nudge injected after 2 identical denials (transcript + `user.input` event asserted via the event-capture store); post-nudge repeat → `errors.Is(err, ErrLoopDetected)`; non-denied loops unchanged; valve-layer denial headless → `ErrAutoModePaused` with tool-result messages intact.
- `event_test.go`: pin `"permission.decision"` in the kind table + payload round-trip. `event_emit_test.go`: `TestEmitPermissionDecision`.

## Risks / notes

- The resolve refactor touches every verdict path — existing gate/auto/valve/clone suites are the regression net; run wholesale.
- Mutex discipline: modeMu vs valve.mu ordering is documented at gate.go:243-245 — don't invert.
- Wire format additive-only; never rename existing kinds.
- `fsstore.Append` goroutine-safety: spawn goroutines already emit concurrently — verify once.
