# Results — 0012 Subagent UX: live-testing findings & hardening

Change 0012 shipped the subagent framework per spec, then went through three
rounds of live testing (traces 4–6), a four-reviewer architecture review, and
a three-harness context-management research pass. This doc records what was
found and what changed on the branch as a result. Companion docs:

- Architecture review (full findings + target design):
  `docs/reviews/2026-08-04-subagent-architecture-review.md`
- Context management design (research synthesis + tiers):
  `docs/designs/context-management.md`

## Production failure classes found in live testing

**1. Approval-slot deadlock (trace4 — total freeze).** A parallel
`spawn_agent` batch put N concurrent permission requests through a single
`m.approval` slot; each overwrote the last, orphaning responder channels.
Orphaned gate goroutines blocked forever on `<-respCh` (ctx never cancelled),
`executeTools`' `wg.Wait()` never returned, the turn froze permanently. A
second path: exiting the agents overlay discarded its private approval slot
with the same effect.

**2. Unbounded model call (trace5 — silent multi-minute stall).** A child
agent's synthesis request (8 full files in the prompt) hung on
`http.DefaultClient` with no timeout, no retry, no visibility. Children were
also untraced, so the hang was invisible in `--trace` output.

**3. Context balloon (trace6 — gateway starvation).** Full tool outputs were
appended verbatim to model history every turn: 3k → 150k prompt tokens in 12
turns (~330KB request JSON). LiteLLM stalled on the oversized request; header
timeouts + retries burned minutes on an unservable payload. `spawn_agent` was
offered in every request — the model simply preferred direct whole-file reads
(the only affordance the toolset gave it).

## Hardening landed on this branch

**Reliability / concurrency**
- Approval FIFO queue single-homed in `ShellModel`; overlay renders and
  answers the same queue; queue survives view switches; turn-end drains with
  deny; Esc actually denies (bubbletea names the key `"esc"`, not `"escape"`).
- `Program.Send` migration: `tui.StartBridges` pumps agent events, coalesced
  tree updates, and registry reloads into bubbletea; the entire
  `waitForMsg`/re-arm machinery — the root cause of the freeze/leak family —
  is deleted. Overlay message handling collapsed into one switch (fixing:
  assistant text lost during overlay, inline blocks stuck "Running" after an
  overlay visit, timer chains dying on overlay entry).
- Race-safe UI reads: `AgentNode.Snapshot()`/`AgentTree.SnapshotAll()` value
  copies; the TUI never touches live node fields (the `node.Events` slice race
  was memory-unsafe).
- Bounded model calls: per-attempt timeout (5m), 60s response-header timeout,
  3 attempts with backoff, cancel-aware; failures carry model, payload size,
  attempt count, and duration, and land in the trace as labeled
  `── ERROR ──` blocks. Root and children share one mutex-guarded trace file
  with per-agent labels.
- Remote lifecycle: cancelled remote jobs finish `StatusCancelled` (were
  reported as success); stream-lost matched by local event name (was spoofable
  error-string comparison); split dispatch (15s) vs stream (header-bounded,
  no overall timeout) HTTP clients — the old fallback killed any SSE stream
  over 30s; 4MiB scanner buffer (64KB default silently killed large events);
  retry budget resets on progress.
- Spawn path: tree-global width semaphore (8 concurrent children; queued ones
  stay visible as pending); unknown tool names fail the spawn with the names
  listed (was: silently near-empty registry); one-shot mode passes
  remote/intent fields through (was: silently ran locally); children that hit
  max-turns return their partial transcript with a stop-reason marker (was:
  entire transcript discarded).

**Context management (replaces the interim hard caps)**
- Spill-file truncation, applied centrally in `Registry.Execute`: results
  over 20KB keep head+tail inline; the full output is saved under
  `~/.fuse/tool-output/` and the marker points at it (grep / ranged read /
  delegate to a subagent). Lossless from the model's perspective. 7-day GC.
- `read_file`: 1000-line default window with explicit continuation footer;
  explicit ranges unchanged.
- New `grep` tool (safe-listed): bounded `path:line:` matches; persona prompt
  now teaches grep-then-ranged-read instead of whole-file dumps.
- Hybrid token accounting in the loop: provider-reported usage of the last
  response + bytes/4 of the delta appended since; per-model `context_window`
  config (default 128k). At 85% of window, old tool results outside a recency
  protection budget are stubbed (`[old tool result cleared …]`) — user and
  assistant messages never touched, so tool pairing stays valid.
  `ErrContextTooLarge` only as a last resort. Provider context-length
  rejections are pattern-detected, pruned hard, and retried exactly once.

**TUI fixes from live testing**
- Batched tool calls pair with their results (FIFO of pending calls; was one
  bullet followed by N unbroken result blocks).
- Agents detail pane follows the newest events; wheel scrolls it (was: wheel
  scrolled the hidden shell viewport; pane pinned to oldest events).
- Ctrl+L clears inline-block tracking (stale indices scribbled on new
  content); approval block width-capped; rune-safe truncation everywhere.

## Second hardening round (2026-08-05, from continued live testing)

**Stability**
- **Nested-spawn deadlock fixed**: the width cap counted "agents alive", so
  8 parents blocked in `spawn_agent` held every slot while their children
  queued — observed live as a fully frozen tree. `AgentTree.YieldSlot`/
  `UnyieldSlot` release a parent's slot while it waits on children
  (refcounted for parallel batches; root exempt). Regression test reproduces
  the freeze shape.
- **Panic containment**: an inverted `read_file` range (`start 110, end 60`)
  panicked on a child goroutine and killed the whole TUI. Three layers now:
  range validation in read_file, `recover` in `Registry.Execute` (any tool
  panic → error Result with stack), and a `recover` backstop around the
  whole child run in `spawnLocal`.
- **Display integrity**: read_file refuses binaries (NUL sniff — the model
  read the compiled `fuse` binary and Mach-O bytes corrupted the terminal);
  `sanitizeDisplay` strips ESC/C0/C1/`\r` and **expands tabs** (the
  fixed-width compositor counts `\t` as one cell while the terminal expands
  it — tab-indented Go source sheared both panes and desynced bubbletea's
  row diffing); event view and shell viewport hard-wrap (wordwrap + wrap
  chain) so no line can exceed pane width; model-controlled labels sanitized
  at every fixed-width render site.
- **Turn clock**: the root node was created running at session start and
  never finished — the agents view counted up forever. `BeginTurn`/`EndTurn`
  scope the root's clock to the turn; error turns end the root as `✕`.

**UX**
- User prompts echo into the transcript (previously vanished on submit).
- Non-verbose tool results show up to 8 real lines + "(+N more — /verbose,
  Tab → inspect)" instead of a 120-char chop.
- Agents overlay: tree pane scrolls with the selection (overflow indicators
  `↑/↓ N more…`); event list has cursor selection following the tail;
  **enter expands any event to its full untruncated content**, scrollable;
  esc walks back one level; mouse wheel targets the active pane; queued
  nodes explain themselves ("Queued — waiting for a spawn slot…").
- Status bar shows a live `⚒ N running · M queued` counter before the
  overlay is ever opened.
- A compact status tree of the turn's subagents is written into the chat
  history when the turn ends (glyphs, elapsed, tokens, └/├ edges).
- Approval modals render as overlays that vanish when answered, leaving
  timestamped `✓ allowed / ✗ denied` records in the transcript; `/approvals`
  recalls the session's full decision log; `/agents` and `/approvals` are in
  the completer.
- Model-call failures carry model, payload size, attempts, and duration in
  both the UI error and labeled `── ERROR ──` trace blocks.

## Deviations from the original spec (known, deliberate)

- **Children run `permissions.AlwaysApprove`**, not the spec'd
  snapshot-clone gate. `CloneForChild` exists and is correct but needs the
  approval queue in place first (now landed); re-enabling it is Phase 3 of
  the architecture-review roadmap. Until then, spawn is an approval bypass —
  flagged as a security gap in the review doc.
- `Registry.Subset` unknowns now **fail the spawn** rather than being
  "dropped with a node event" — a tool-error feedback loop beats silent
  degradation.

## Follow-on work (from the review; not in this change)

- Phase 2: structured transcript store (kills the line-index inline-block
  machinery and O(transcript)-per-frame rewraps).
- Phase 3: `toolcore` leaf package + `ChildSpawner` interface +
  `harness.Factory` (replaces the duplicated 80-line spawn closures, fixes
  grandchild privilege escalation by subsetting from the parent's registry,
  re-enables `CloneForChild`, structured `SpawnResult` with usage/stop
  reason).
- Phase 4: single `AgentEvent` vocabulary; `Renderer` → `Emit(event)`.
- Context Tier 2: anchored LLM compaction with segment store (designed in
  the context-management doc).
- Remote/secrets fixes from the review not yet applied: git tokens plaintext
  in dispatch JSON even in encrypted mode; no https enforcement; dispatch
  error bodies reflected unsanitized into model context; `Collect` ordering
  race.
