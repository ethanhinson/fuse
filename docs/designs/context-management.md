# Context Management Design — replacing the hard caps

Synthesis of three code-verified research reports (2026-08-04) on how mature
harnesses avoid the "append every tool output verbatim until the gateway
stalls" failure: **Cline** (cline/cline v3.89.2 + v4 SDK), **OpenCode**
(anomalyco/opencode, ex sst/opencode; plus the archived Go original), and
**grok-build** (xai-org/grok-build). Full reports live in the session research
transcripts; mechanisms below cite their origin.

## Where the three harnesses converge

1. **Bound tool output at the source, with continuation affordances.**
   Reads default to a window (Cline 1000 lines, OpenCode 2000 lines/50KB,
   grok-build 1000 lines) with offset/limit params and an explicit
   `(Showing X–Y of Z. Use offset=… to continue.)` footer.
2. **Truncation is lossless: spill to disk, point the model at it.**
   OpenCode writes full output to `<data>/tool-output/` and the marker says
   "use Grep/Read with offset — or delegate to a subagent". grok-build does
   the same for bash/MCP/web and even archives pre-compaction history as
   grep-able `segment_N.md` files. Context is treated as a cache over disk,
   not the source of truth.
3. **No client tokenizer.** Trigger decisions use provider-reported usage
   from the previous response; grok-build adds a bytes/4 estimate of only the
   delta appended since (hybrid counting — error never compounds).
4. **Threshold-triggered compaction, not turn failure.** Compact at ~85–90%
   of the context window; keep a verbatim tail (2 turns / 2–20k tokens);
   structured summary templates (Goal/State/Next/Files) updated incrementally
   ("anchored"); deterministic fallback ladder when the summarizer itself
   can't run; never orphan tool_call/tool_result pairs.
5. **Provider context-length errors are recoverable**: detect by pattern,
   shrink deterministically (never depending on another LLM call), retry
   once, replay the failed message.
6. **Subagents are a context strategy**: fresh child context, only the final
   text returns (capped), truncation markers hint at delegating spill-file
   analysis to a child.

Where they differ: Cline dedups repeated file reads (all but newest becomes a
stub) gated on ≥30% savings before any truncation; OpenCode prunes tool
outputs by recency (protect newest 40k tokens + last 2 turns, stub the rest as
`[Old tool result content cleared]`, only fire when reclaiming ≥20k);
grok-build keeps history verbatim between full-replace compactions.

## Fuse design

### Tier 1 — deterministic, no LLM calls (implemented)

Replaces the 16KB per-result chop and the 360KB hard turn-failure.

- **Spill-file truncation** (`internal/tools/spill.go`), applied centrally in
  `Registry.Execute`: results over 20KB keep head+tail (middle-out) and the
  full output is written to `~/.fuse/tool-output/`; the marker carries the
  path plus recovery guidance (grep / ranged read / spawn_agent). 7-day GC.
- **read_file windowing**: 1000-line default window, continuation footer,
  total-line reporting; ranged reads unchanged.
- **Hybrid token tracking** in the agent loop: exact usage from the last
  response + bytes/4 of the delta appended since; per-model
  `context_window` config (default 128k).
- **Recency pruning instead of turn failure**: at 85% of window, stub tool
  results older than the protected tail (newest ~40k estimated tokens of
  tool output; user/assistant messages never touched, so tool pairing stays
  valid) as `[old tool result cleared — re-run the tool if needed]`. Only
  if pruning still leaves the estimate over budget does the turn error
  (ErrContextTooLarge, now a last resort).
- **Context-length error recovery**: pattern-match the gateway 400, prune
  aggressively, retry the request once.

### Tier 2 — compaction (follow-up)

- Anchored LLM summarization at the 85% threshold when pruning is not
  enough: verbatim tail (last 2 user turns, 8k-token budget), fixed summary
  template (Objective / Important Details / Work State / Next Move /
  Relevant Files), previous summary passed back for incremental update,
  summarizer input ladder (drop oldest turns → strip tool outputs) so the
  summary call itself cannot overflow, and a suppression state so failures
  don't hot-loop (grok-build's design).
- Segment store: archive the pre-compaction transcript as markdown next to
  the session log with a "grep your past at <path>" pointer in the summary.
- Optional Cline-style duplicate-file-read dedup as a pre-compaction pass
  (30%-savings gate).

### Non-goals (absent in all three, deliberately)

Client-side tokenizers; semantic relevance scoring; per-message importance
models.
