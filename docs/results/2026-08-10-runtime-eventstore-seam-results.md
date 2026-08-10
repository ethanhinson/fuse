<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0043 — Runtime EventStore seam — typed, pluggable, introspectable loop event stream](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0043-runtime-eventstore-seam.md)**
<!-- docket:backlink:end -->

# Runtime EventStore seam — typed, pluggable, introspectable loop event stream — results

Change: #43 · Branch: feat/runtime-eventstore-seam · PR: <url> · Plan: docs/superpowers/plans/2026-08-10-runtime-eventstore-seam-plan.md · ADRs: 24, 25

## Status: Stage A landed (green). Stage B (streaming deltas) DEFERRED to its own change.

The additive core is complete and the full suite is green under `-race` (23 packages, no
FAIL/panic/DATA RACE); `go build`, `go vet`, and `gofmt` are clean on the touched trees.
Stage B (streaming token deltas) is intentionally **not** in this PR — see *Deviations*.

## What landed (Stage A)

- `internal/event` — agent-free leaf package: the typed `Event` envelope + `Kind`
  discriminant + per-kind payload structs (`json.RawMessage` payloads, no agent/model
  dependency), the `EventStore` interface (`Append`/`Subscribe`/`Replay`), and a `NoopStore`
  default. Self-delimiting JSONL round-trip (`MarshalJSONL`/`ParseEvent`).
- `internal/event/fsstore` — `FSEventStore`, a mutex-guarded JSONL writer at
  `<baseDir>/<sessionID>/events.jsonl`, born PLAINTEXT + append-only. Store-allocated
  monotonic `Seq`; non-blocking per-subscriber fan-out (drop-newest + gap marker);
  `Replay(from)` cursor.
- `cmd/fuse` — process-global lock-guarded `activeEventStore` holder (mirrors
  `activeSegmentSink`, ADR-0019), installed at session startup next to the segment sink.
- `internal/agent` — `eventSink` field (never-nil no-op default), `SetEventSink` /
  `SetNodeIdentity` setters, and a single greppable `emit(kind, turn, payload)` best-effort
  helper. Loop emission at every state transition (turn, model-call, tool call/result,
  summarize, loop-detector trip, error) with FULL payloads.
- Spawn lifecycle (`spawn.start`/`spawn.done`) emitted at all THREE cloned child-builder
  sites (`shell.go`, `main.go`, `research_probe.go` — learning
  `patch-every-cloned-child-builder`).
- Session log re-expressed as a CONSUMER: `projectEventToLog` maps `spawn.done` → the exact
  current `session.jsonl` `LogEntry`; a subscriber writes a parallel `session.projected.jsonl`
  transiently alongside the retained direct write. Byte-equivalence is gated by test.

## Verify (human)

Automated suites are green, but the event stream lives in the real `cmd/fuse` wiring that the
teatest harness cannot reach (it fakes the Completer seam). Per learning
`verify-tool-loop-at-gateway-seam`, drive the shipped binary against a scripted
`LLM_GATEWAY_URL` double, built from THIS worktree:

- [ ] **Live stream at the real loop**: run an interactive session that makes a tool call and
  spawns a child, then inspect `~/.fuse/sessions/<root-id>/events.jsonl` — confirm the
  ordered `Seq` sequence with `turn.start`, `model.call.start`, `model.call.end`,
  `tool.call`, `tool.result`, `spawn.start`, `spawn.done`, `turn.end`, each carrying full
  payloads (full assistant text, full tool args/results).
- [ ] **Log-projection byte-equivalence** (the gate for deleting the direct write): after a
  session with ≥1 spawn, `diff <(sort session.jsonl) <(sort session.projected.jsonl)` should
  be EMPTY — the projected log reproduces the shipped log byte-for-byte (including the
  local-timezone TS; see Findings #1).
- [ ] **Summarization + loop-trip**: drive a long context to trigger Tier-2 summarization and
  confirm a `context.summarize` event lands beside the segment archive; force an identical
  tool-call loop to confirm `loop.detector.trip`.
- [ ] **Non-blocking under a stalled consumer**: confirm the loop never stalls even if a
  subscriber never reads (covered by `TestNonBlockingSlowSubscriber` under `-race`, but worth
  a live sanity check that the session runs to completion normally).

## Findings

- **Whole-branch review (auto — `superpowers:requesting-code-review` unavailable in this
  environment; degraded to inline review + a fresh-eyes review subagent) returned SAFE to
  open (Stage A), with two SHOULD-FIX items, both fixed in `782c7d2`:**
  1. **Projection TS timezone**: the shipped `session.jsonl` direct write uses `time.Now()`
     (LOCAL); the event store stamps `TS` in UTC. `projectEventToLog` now converts `e.TS` back
     to `.Local()` so the projected log is byte-identical to the shipped log IN PRODUCTION —
     the earlier equivalence test had a blind spot (it fed the same UTC ts to both sides). The
     test now compares the two real production TS sources. This matters because byte-equivalence
     is the gate for the trivial follow-up that deletes the direct write.
  2. **Emission symmetry on the structured-delegation (`return_result`) paths**: the terminal
     capture, exhaustion, and retry branches now emit `turn.end`, and the synthesized
     `return_result` call emits `tool.call`/`tool.result` — so an Expects-child's stream is
     balanced like every other turn. Added Expects-path emission coverage.
- Added a reader-vs-writer (`Replay` during `Append`) `-race` guard per learning
  `race-invisible-to-race-detector-without-concurrent-test`.
- **Two ADRs recorded** for the non-obvious decisions: **ADR-24** (EventStore independent of
  the segment store — events born plaintext, segments untouched; resolution (b) of the spec's
  must-resolve overlap question) and **ADR-25** (store-allocated Seq + non-blocking
  drop-newest-with-gap subscriber delivery, preserving ADR-0016).

## Deviations from the spec

- **Stage B (streaming token deltas) deferred to its own change.** The `Event` schema already
  carries `KindModelDelta` + `ModelDeltaPayload` (delta-ready), so this is a pure additive
  follow-up with no schema rework. It was deferred because it is genuinely non-additive on two
  load-bearing transport paths, exactly the risk the spec fenced:
  - The LiteLLM HTTP `Adapter` already streams internally (`Stream: true`) and parses per-delta
    content in `readStream`, but surfacing `model.delta` needs a `func(delta string)` callback
    threaded through `Complete → completeOnce → readStream`/`readBuffered` + the retry loop.
  - The `CLIAdapter` uses `--output-format json` + `cmd.Output()` (buffers the whole subprocess
    output); streaming needs a switch to `--output-format stream-json`, a streaming pipe reader,
    and a distinct parse path coupled to the per-invocation HITL relay.
  Recommended as its own change: `runtime-eventstore-streaming-deltas`.

## Follow-ups (trivial / documented, out of this PR by design)

- Delete the now-redundant direct `Logger.Write` call site (`cmd/fuse/shell.go`) once the
  projected log is confirmed byte-identical in production (the equivalence gate now holds).
- Optional gzip/rotation of `events.jsonl` (the JSONL format leaves room, ADR-0017-style).
- Stage B streaming deltas (its own change, above).
