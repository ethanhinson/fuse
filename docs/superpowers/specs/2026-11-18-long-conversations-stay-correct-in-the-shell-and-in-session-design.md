<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0157 — Long conversations stay correct in the shell and in --session runs](../../changes/archive/2026-11-18-0157-long-conversations-stay-correct-in-the-shell-and-in-session.md)**
<!-- docket:backlink:end -->

# Long conversations stay correct

## Goal
Conversation handling in `fuse shell` and in one-shot `--session` runs works for long conversations the way a serious coding chat harness does: providers never see a malformed history, compaction never loses content, a stuck or failed turn never loses the conversation.

## Fixes (one commit each, test first)
1. A compaction summary is inserted after the whole tool batch, never between results of one parallel batch. The pairing test helper also checks adjacency.
2. The living summary already in history seeds the next run's summarizer, so a second compaction folds the first one in.
3. A full-conversation compaction tier summarizes the oldest whole exchanges (text included) and drops old replayed reasoning, never cutting an assistant(tool_calls)+results group. The shell gets `/compact`.
4. One protected set: what is stubbed is exactly what was summarized; the summarizer chunks instead of dropping the oldest content.
5. The token estimate includes the system prompt and tool schemas, and the last known usage carries across runs (shell, and --session via conversation.json, still reading v1).
6. A failed --session run saves its pairing-valid partial history plus a marker saying the turn was interrupted and why, and still exits nonzero.
7. Relevance recency is indexed by assistant step; the query is the last real user message, never a synthetic nudge.
8. Queued steering messages are drained when a run ends in error, and /clear clears the queue.
9. Context-length errors are matched by provider codes and tight phrases; a learned window has a floor.
10. Each shell turn has a cancellable context bound to Esc; cancelling keeps the valid partial history and the shell stays usable.
Small: a run emits user.input only for the new user message, not every historical one.

## Acceptance
- Each fix has a test that failed before the fix.
- `make test` passes.
- README documents /compact, Esc to stop a turn, --session saving a failed turn, and compaction of text.
