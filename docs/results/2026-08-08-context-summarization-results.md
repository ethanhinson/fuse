<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0027 — Anchored context summarization at compression threshold](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0027-context-summarization.md)**
<!-- docket:backlink:end -->

# Anchored context summarization — results

Change: #0027 · Branch: feat/context-summarization · PR: <set at PR open> · Plan: docs/superpowers/plans/2026-08-08-context-summarization-plan.md · ADRs: none

## Verify (human)

Automated tests cover config parsing, the widened `SegmentSink` seam, the bounded
summarizer (prompt/anchoring/ladder/fallback), the loop integration (trigger sequence,
fail-safe golden match, suppression, anchoring, sink pointer), and a real-binary
gateway-seam pass. The surfaces worth a live look at the merge gate:

- [ ] **Live compaction against a real gateway.** With `context.summarization.enabled: true`
      (the default) and a small `context_window` model, drive a long tool-heavy session past
      85% and confirm: an `── REQ ── [summarizer]` block appears in the trace, the transcript
      shows `context: ~Nk/Mk tokens — summarized ~Kk of old tool results`, and the agent keeps
      working from the injected ODSNF summary without re-running the compacted tools.
- [ ] **Fail-safe is invisible.** Point `context.summarization.model` at an unreachable/invalid
      model id and repeat: the turn must fall through to today's Tier-1 stub pruning with no
      user-visible regression, and the summarizer must not hot-loop (suppression window).

## Findings

- **Injection shape = inject summary, then let Tier-1 stub the raw region (deviation, no ADR).**
  The plan said "replace the raw region with a summary message." Realized as: inject one
  assistant text summary at the protected-region boundary, then let the **unchanged**
  `pruneOldToolResults` stub the raw tool results. This keeps `pruneOldToolResults` byte-identical
  (so the fail-safe path is a provable golden match to Tier-1), and the summary — an assistant
  message with no `tool_calls` — is never re-stubbed and never orphans a tool-call/tool-result
  pair. Pinned by tests.
- **Anchoring drops the prior summary (deviation, no ADR).** To satisfy D3 ("only one summary
  lives in context at a time"), the loop removes the previously-injected summary (`dropPriorSummary`,
  matched by the `summaryHeader` prefix) before inserting the new anchored one — an initial version
  stacked summaries across compactions and was caught by the anchoring test.
- **Exported wiring entry point (deviation, no ADR).** `newSummarizer`/`*summarizer` are
  package-internal, so `cmd/fuse` cannot call them. Added `Agent.EnableSummarization(Completer,
  modelID, maxOutput, sink)` as the exported wiring method (kept `SetSummarizer` for in-package
  tests). `internal/agent` deliberately does not import `internal/config`, so the Agent carries the
  resolved summarizer + no-op sink rather than the config struct — same seam discipline as the
  blackboard/spawn cycles.
- **All gateway agent-construction sites carry the wiring (`patch-every-cloned-child-builder`).**
  Reviewed every `agent.New(` site in `cmd/fuse/run.go` by grep: four total. The two gateway paths
  (`buildAgentCore`, `buildChildAgent`) both call `installSummarizer`; the two `cli/`-adapter paths
  return early with a re-exec'd `fuse` subprocess that does its own context management, so
  summarization correctly does not apply there. No missed site.
- **`Threshold` config field is surfaced but not yet divergent from Tier-1.** Per the spec
  ("shares Tier-1's 85%"), the loop budget still uses the `pruneThresholdPct = 85` constant; the
  `context.summarization.threshold` field is parsed and stored for a future divergence but does not
  yet independently gate the summarizer. Spec-compliant for v1.

## Follow-ups (already tracked — not this change)

- **#0030** implements the real `SegmentSink` (raw archive, replay, GC) against the widened
  `SegmentRegion` seam this change ships; the "grep your past at `<path>`" recovery pointer lights
  up only once #0030 is wired.
- **#0028** swaps the recency candidate selector for relevance scoring.
- **#0029** adds the read-file dedup pre-compaction pass.

## Skill-layer note

`superpowers:writing-plans`, `superpowers:subagent-driven-development`, and
`superpowers:requesting-code-review` were all unavailable at runtime on this machine, so the
plan/build/review roles ran under docket's Skill-layer **missing-skill fallback** (degrade to
`auto` + warn): the plan was authored inline, the build was executed task-by-task under TDD, and a
whole-branch review was performed inline before this PR opened. Behavior and artifacts are
unchanged; only the mechanism differed.
