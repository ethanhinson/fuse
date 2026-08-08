<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0027 — Anchored context summarization at compression threshold](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0027-context-summarization.md)**
<!-- docket:backlink:end -->

# Implementation Plan — Anchored context summarization at compression threshold (change 0027)

**Change:** #0027 (context-summarization) · **Spec:** `docs/superpowers/specs/2026-08-08-context-summarization-design.md` (on `docket`) · **Date:** 2026-08-08

> **Provenance note.** `superpowers:writing-plans` was unavailable at runtime, so this plan was
> authored inline by `docket-implement-next` under the Skill-layer missing-skill fallback (degrade
> to `auto` + warn). The build step likewise falls back to inline TDD if
> `superpowers:subagent-driven-development` is unavailable — each task below is written so either an
> SDD worker or an inline builder can execute it test-first.

## Goal

Implement **Tier 2 — anchored LLM summarization** in the fuse agent loop. At the existing 85%
over-budget point, run a bounded LLM summarization pass over the old (recency-unprotected) tool-result
region **before** the existing stub pruning, replace that raw region with a single structured
Objective/Details/State/Next/Files (ODSNF) summary message at the protected-region boundary, and carry
the previous summary forward (anchored). Any summarizer failure falls through to today's Tier-1 stub
pruning and arms a bounded suppression window. Ship a widened no-op `SegmentSink` seam (#0030 implements
the real sink) and a `context.summarization` config block.

**Invariants that must hold at every task boundary:** `go build ./...` and `go test ./...` green.

## Design anchors (from spec + reconcile)

- Trigger site: `internal/agent/loop.go`, the `if estimate > budget {` branch in `Agent.Run`
  (currently ~line 141), immediately **before** `pruneOldToolResults(messages, protectBudget(window, false))`.
- Constants already present: `pruneThresholdPct = 85`, `pruneProtectTokens = 40_000`, `bytesPerToken = 4`,
  `protectBudget(window, recovery)`, `messagesSize`, `prunedStub`, `pruneOldToolResults` (only touches
  `Role == "tool"` messages — the same pairing discipline the summary injection must honor).
- Bounded transport: `internal/model/adapter.go` — `Adapter` with `RequestTimeout`,
  `ResponseHeaderTimeout` (on the shared client), `MaxAttempts`, `RetryBackoff`, `WithTraceLabel(w, label)`,
  copy-on-configure idiom. The summarizer reuses this with a distinct `summarizer` trace label.
- `model.Message{Role, Content, ToolCalls, ToolCallID, Name}`; `CompletionReq{Model, Messages, Tools,
  MaxTokens, ToolChoice}` (`ToolChoice: "none"` forces a text-only reply — the summarizer wants no tools);
  `CompletionResp{Content, ...}`.
- Config: on-disk `rawConfig` (loader.go `mergeFile`) → normalized `Config`. Add a `context:` top-level
  key. Precedent for a named secondary model alias: `AutoConfig.ClassifierModel`.
- Agent wiring: `cmd/fuse/run.go` `buildAgent`/`buildChildAgent` set `a.ContextWindow = mc.ContextWindow`
  after `agent.New(...)`. The summarizer Completer + summarization config are installed on the Agent the
  same way (new exported fields/setter, nil ⇒ Tier-2 off — byte-identical to today).

## Learnings honored (blocking)

- **`bound-every-model-call`** — the summarizer is a model call; it MUST go through the bounded adapter
  with a per-attempt timeout, response-header timeout, bounded retries, and a distinct `summarizer` trace
  label. No `http.DefaultClient`, no untraced call. Asserted in Task 3's tests.
- **`verify-tool-loop-at-gateway-seam`** — the loop-wiring change is verified with the real binary against
  a scripted `LLM_GATEWAY_URL` double (Task 6), not only via the faked-Completer unit seam.

---

## Task 1 — `context.summarization` config surface

**Files:** `internal/config/schema.go`, `internal/config/loader.go`, `internal/config/loader_test.go`

**Test first:** a loader test that parses a YAML doc with

```yaml
context:
  summarization:
    enabled: true
    model: "cheap/model"
    threshold: 0.85
    max_output: 2000
```

asserts the normalized `Config.Context.Summarization` fields; a second doc with the block absent asserts
the zero-value default (`enabled` default per spec — Tier 2 **on** by default, so model the default as
`enabled: true` when the `context:` block is absent, but the summarizer stays inert until a Completer is
wired in Task 5, keeping the absent-block case behavior-identical to today). Include an `enabled: false`
doc asserting it disables. A doc with only `model:` set asserts the other fields keep defaults.

**Implement:**
- Add `SummarizationConfig struct { Enabled bool; Model string; Threshold float64; MaxOutput int }` and a
  `ContextConfig struct { Summarization SummarizationConfig }` to `schema.go`; add `Context ContextConfig`
  to `Config`. Add `rawContextConfig` / `rawSummarizationConfig` mirrors (plain scalars/ints — no free-text
  colon-space fields, so `yaml.Unmarshal` is safe per the `yaml-plain-scalar-colon-space` learning).
- Wire `raw.Context` into `mergeFile` in `loader.go` with the same present-value-overrides discipline used
  for other blocks. Use a `*bool` for `enabled` in the raw mirror so an omitted key keeps the true default
  while `enabled: false` takes effect (mirrors `RespectRobots`). Threshold default 0.85, MaxOutput default
  2000 applied at load (or a `defaultsContext()` helper) so an absent block yields the documented defaults.

**Green:** `go test ./internal/config/...`, `go build ./...`.

---

## Task 2 — Widened `SegmentSink` seam + no-op default

**Files:** `internal/agent/segment.go` (new), `internal/agent/segment_test.go` (new)

**Test first:** assert the no-op sink returns `("", nil)` for any `SegmentRegion`; assert a fake sink
returning a path returns that path. (The pointer's effect on the summary text is tested in Task 4.)

**Implement** exactly the spec's widened signature (2026-08-08 amendment, corroborated by #0030 D1):

```go
type SegmentRegion struct {
    TurnStart, TurnEnd int
    Messages           []model.Message
    Summary            string
    ToolNames          []string
    TokensBefore, TokensAfter int
}
type SegmentSink interface {
    Archive(r SegmentRegion) (pointer string, err error)
}
type noopSegmentSink struct{}
func (noopSegmentSink) Archive(SegmentRegion) (string, error) { return "", nil }
```

`Archive` errors are best-effort (Task 5 logs, never fatal). Ship the no-op as the default sink.

**Green:** `go test ./internal/agent/...`, `go build ./...`.

---

## Task 3 — The bounded summarizer (`summarize.go`) with ODSNF prompt, anchoring, input ladder

**Files:** `internal/agent/summarize.go` (new), `internal/agent/summarize_test.go` (new)

**Test first (unit, faked Completer):**
- **ODSNF prompt + text-only:** the summarizer builds a `CompletionReq` with `ToolChoice: "none"`,
  `MaxTokens` = configured `max_output`, `Model` = resolved summarizer model, and a prompt containing the
  Objective/Details/State/Next/Files instruction plus the candidate region. Assert on the request captured
  by a fake Completer.
- **Anchoring (D3):** when a non-empty `previousSummary` is passed, it appears in the request with an
  "update this in place" instruction; the returned summary is the model's text.
- **Bounded call (`bound-every-model-call`):** the summarizer is constructed from the bounded adapter with
  a distinct `summarizer` trace label. Drive a scripted trace `io.Writer` and assert a `[summarizer]`-labeled
  `REQ`/`RESP` block appears. (Use the real `model.Adapter` against an `httptest.Server` returning a canned
  completion so the trace path is exercised — same shape as `adapter_test.go`.)
- **Input ladder (D2):** given a candidate region that exceeds the summarizer's own input budget, the ladder
  shrinks input deterministically — **drop oldest turns → strip tool outputs** — and still returns a summary;
  assert which rung was taken (expose the rung via the return or a test hook). Ladder-exhaustion returns the
  fallback sentinel (no summary).
- **Fallback (fail-safe):** timeout / transport error / empty output each return `("", false)` (or an
  equivalent "no summary" signal) rather than propagating — the caller decides fallback.

**Implement:** a `summarizer` type holding a `Completer` (bounded adapter, `summarizer` label), the resolved
model id, and `maxOutput`. Method `summarize(ctx, region []model.Message, previousSummary string) (string, bool)`.
The ladder is a deterministic pre-flight over `region` gated by an internal input-token estimate (reuse
`messagesSize`/`bytesPerToken`). Keep the per-attempt timeout and retries in the adapter (do not re-implement).

**Green:** `go test ./internal/agent/...`, `go build ./...`.

---

## Task 4 — Summary message assembly + recovery-pointer rule

**Files:** `internal/agent/summarize.go` (extend), `internal/agent/summarize_test.go` (extend)

**Test first:**
- Building the injected message from an ODSNF summary + an **empty** sink pointer omits the "grep your past
  at `<path>`" line; a **non-empty** pointer includes exactly that line.
- The injected message is a single synthetic message that a subsequent `pruneOldToolResults` will not
  re-stub incorrectly and that keeps tool-call/tool-result pairing valid (choose `Role == "tool"` with a
  synthetic `ToolCallID`/`Name`, OR an assistant text message placed so no tool_call is orphaned — pick the
  shape that leaves pairing valid and pin it with a test that runs the provider-pairing invariant used
  elsewhere).

**Implement:** a helper that formats the final summary message text (ODSNF body + optional recovery pointer)
and returns the `model.Message` to inject at the protected-region boundary.

**Green:** `go test ./internal/agent/...`, `go build ./...`.

---

## Task 5 — Loop integration: summarize → archive → prune → inject, with suppression

**Files:** `internal/agent/agent.go`, `internal/agent/loop.go`, `internal/agent/loop_test.go`,
`cmd/fuse/run.go`

**Test first (loop_test.go, faked Completer + faked/real summarizer):**
- **Trigger sequence:** an over-budget turn calls the summarizer **before** pruning; the raw region is
  replaced by the summary at the protected-region boundary; the post-pass estimate drops below budget;
  assert no orphaned tool-call/tool-result pairs.
- **Fail-safe golden:** with the summarizer forced to fail (timeout/error/empty), the loop falls through to
  `pruneOldToolResults` and the post-state is **identical** to today's Tier-1 path (golden comparison against
  a run with Tier-2 disabled).
- **Suppression:** after a summarizer failure, the next N over-budget turns do **not** call the summarizer
  (assert zero summarizer requests during the window), then summarization resumes after the window clears.
- **Anchoring end-to-end:** two successive compactions pass `summary_v1` into the second call and produce
  `summary_v2`; assert only one summary message lives in context at a time.
- **Disabled:** `Context.Summarization.Enabled == false` (or no summarizer Completer wired) ⇒ pure Tier-1
  behavior, byte-identical to today.
- **SegmentSink:** a fake sink records the `SegmentRegion` (turn range, tool names, tokens before/after) and
  returns a path; assert the summary then carries the recovery pointer; the default no-op omits it. Sink
  `Archive` error is logged, not fatal.

**Implement:**
- Add to `Agent` (agent.go): a summarizer holder (nil ⇒ Tier-2 off), a `SegmentSink` (default no-op), the
  resolved `SummarizationConfig`, and internal fields for `previousSummary` + suppression counter (loop-scoped
  state; keep them local to `Run` if they need not survive across calls — prefer function-local state in `Run`
  so concurrent agents don't share). Add a `SetSummarizer(...)`/exported fields setter mirroring `SetStripSpawn`.
- In `loop.go`, inside `if estimate > budget {`, before `pruneOldToolResults`: if summarization enabled AND a
  summarizer is wired AND not suppressed: compute the candidate region (tool results older than
  `protectBudget(window, false)`), call `summarize`, and on success — call `sink.Archive(region)`, replace the
  raw span with the injected summary message at the boundary, set `previousSummary`, recompute `estimate`; on
  failure — arm suppression for N turns. Then fall through to the **unchanged** `pruneOldToolResults` +
  `ErrContextTooLarge` path. Suppression window N is an internal constant (mirror `pruneThresholdPct` style),
  not config, unless a test shows tuning is needed.
- In `cmd/fuse/run.go` `buildAgent`/`buildChildAgent`: after `agent.New(...)`, when
  `cfg.Context.Summarization.Enabled`, construct the summarizer from a bounded adapter decorated with
  `WithTraceLabel(traceW, "summarizer")`, resolve the summarizer model (`cfg.Context.Summarization.Model`
  empty ⇒ `mc.ID`, D4), and install it on the Agent with the no-op sink. Keep the non-summarizing path
  unchanged when disabled.

**Green:** `go test ./...`, `go build ./...`.

---

## Task 6 — Real-binary gateway-seam verification (`verify-tool-loop-at-gateway-seam`)

**Files:** `cmd/fuse/context_summarization_gateway_test.go` (new; mirror the existing
`cmd/fuse/blackboard_gateway_e2e_test.go` rig)

**Test first (this IS the test):** stand up a scripted `LLM_GATEWAY_URL` double that (a) logs each incoming
request, (b) returns a canned ODSNF summary for the **summarizer** request (identified by its distinct
label/model or `ToolChoice: "none"` + summarizer prompt), and (c) returns a normal tool-call/stop sequence for
the main loop. Drive the real binary with a `.fuse.local.yml` override forcing a tiny context window /
threshold so an over-budget turn is reachable within a short scripted run, and assert the post-compaction main
request carries the **injected summary** in place of the raw region — proving the loop wiring, not just a
`loop.go` unit. If the existing e2e rig cannot force an over-budget turn deterministically, add the smallest
config hook needed (e.g. honor a very small `context_window`) rather than weakening the assertion.

**Green:** `go test ./cmd/fuse/...` (and the full `go test ./...`), `go build ./...`.

---

## Task 7 — Docs + final gate

**Files:** `docs/designs/context-management.md` (mark Tier 2 implemented, link the config keys), plan
back-link (stamped separately by the skill).

**Test first:** none (docs). Final gate: `go build ./...` and `go test ./...` both green from the feature
worktree. `gofmt`/`go vet ./...` clean.

---

## Out of scope (do not build)

- Real segment persistence (#0030 — this ships only the no-op sink).
- Relevance-based candidate selection (#0028 — recency selector only).
- Read-file dedup pre-pass (#0029).
- Continuous (every-N-turns) summarization; summarizing user/assistant messages; cross-session persistence.

## Task ordering rationale

Config (1) and the sink seam (2) are leaf dependencies with no loop coupling — build them first so Tasks 3–5
compile against real types. The summarizer (3–4) is unit-testable in isolation at the Completer seam. Loop
integration (5) wires them together and is where the behavioral invariants live. Gateway-seam verification (6)
is last because it exercises the fully composed binary. Each task is independently green.
