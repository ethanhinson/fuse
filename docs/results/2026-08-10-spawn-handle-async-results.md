<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0044 — Spawn handle-async — location-transparent spawning behind a handle-returning contract](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0044-spawn-handle-async.md)**
<!-- docket:backlink:end -->

# Spawn handle-async — results
Change: #44 · Branch: feat/spawn-handle-async · PR: (opened at close-out) · Plan: docs/superpowers/plans/0044-spawn-handle-async-plan.md · ADRs: ADR-0026

## Verify (human)

No interactive/manual checks required beyond CI. `go test -race ./...` is green across all
packages, and the model-facing contract is asserted directly (not just via a live model). If a
belt-and-suspenders live check is wanted, drive a real `spawn_agent` run against a scripted
`LLM_GATEWAY_URL` double and confirm the tool result string (prose + budget line + quota line) is
identical before/after this change — but the unit assertions
(`TestSpawnAgentTool_AwaitsHandleForModelContract`,
`TestSpawnFuncFromWiredToToolPreservesModelContract`) already gate that byte-for-byte.

## What shipped

- `tools.SpawnFunc` is now handle-returning: `(ctx, SpawnRequest) (tools.SpawnHandle, error)`. The
  model-facing `spawn_agent` tool awaits `handle.WaitResult()` internally and emits the same
  result string — model-visible output unchanged, Go-visible contract new (D1).
- Import cycle resolved with an agent-free `tools.SpawnHandle` interface + `tools.SpawnResult`
  value type in `internal/tools`; a thin `cmdSpawnHandle` adapter in `cmd/fuse` satisfies it over
  `agent.AgentHandle`. `internal/tools` still does not import `internal/agent`. (ADR-0026.)
- `spawn.start` / `spawn.done` emission relocated from the three cmd-site child builders (0043)
  into the `agent.Spawner` — the single choke point — via a new `WithEventStore` option
  (default `NoopStore`, inert). Emitted at admission and completion respectively (D2).
- Slot-yield timing (ADR-0016) preserved: `YieldSlot`/`Wait`/`UnyieldSlot` moved into
  `cmdSpawnHandle.WaitResult()`, so the parent yields its slot only while actually blocked.
- Pipeline (`internal/pipeline`) is untouched — it uses `agent.AgentHandle` (`h.Wait()`/
  `h.Result()`) directly and never the `tools.SpawnFunc` seam.
- No compat `(string, error)` spawn adapter survives (grep-gated).

## Findings

Whole-branch review (before PR) confirmed the core refactor correct — cycle stays broken,
choke-point emission with no double/lost emission, slot-yield timing faithful, pipeline isolation,
model-facing contract well-covered. One material **should-fix** was found and addressed in a
follow-up commit:

- **Projected session log diverged from the direct write on the max-turns / loop-detected path
  (should-fix).** Relocating `spawn.done` to the Spawner re-sourced its `Err` field from
  `childResult`'s collapsed error — which is `nil` on the max-turns/loop stop path (childResult
  returns a `"[stopped: …]"` partial-success string). So the projected log selected `kind:"done"`
  while the still-shipping direct `sessLog.Write` (raw `rerr`) selected `kind:"error"`, breaking the
  byte-equivalence that is 0043's whole purpose (and the gate for the trivial follow-up that
  deletes the direct write). Fixed with a spawner-allocated `RunErrSink` (mirroring the existing
  `expectsSink` idiom): each child builder reports the RAW `a.Run` error, and the Spawner's
  `spawn.done` uses it for `Err`, while `SpawnDone.Err` (the handle/model control path) stays the
  collapsed value — so the model-facing partial-success contract is unchanged and the projection
  matches the direct write again. Regression test added
  (`TestSpawnerSpawnDoneUsesRawErrOnStopPath`), plus the missing-coverage gap the reviewer noted
  (the earlier error tests never exercised childResult's nil-swallowing).
- **`Structured` marshal source (nit, no action).** The Spawner's `spawn.done` `Structured` now
  comes from the sink value (vs 0043's lenient-validated value); it is marshaled best-effort and
  the log projection ignores `Structured` entirely, so only a hypothetical `Subscribe()`r would
  observe the difference. No consumer exists; left as-is.

## Follow-ups (auto-capture disabled → reported, not minted)

- **Delete the direct `sessLog.Write` once the projected log is proven equivalent.** This is
  0043's own trivial follow-up. This change restores the byte-equivalence (finding above) that
  gate depends on, but does not own the deletion — leave it as its own change.
- **Change 3 — Runtime interface extraction + binding #2.** Out of scope here; this change made the
  spawn seam handle-shaped so the Runtime is a new implementation behind an unchanged interface.

## Plan deviations

- Import-cycle resolution: the plan chose the interface-in-tools option (option 2); confirmed in
  build and recorded as ADR-0026. No deviation.
- Emission relocation: the plan flagged the `Result`-source subtlety but its byte-identity guard
  reasoned only about `Result`, not `Err`. The review caught that `Err` (which the projection reads
  for `kind`) also diverges on the stop path; fixed as above. This is the one place the plan's
  guard was incomplete.
- Build/review skills: `superpowers:subagent-driven-development` and
  `superpowers:requesting-code-review` are unavailable in this environment → both degraded to the
  `auto` fallback (implementer-driven TDD build; an independent whole-branch review agent before
  the PR). Noted in the PR body.
