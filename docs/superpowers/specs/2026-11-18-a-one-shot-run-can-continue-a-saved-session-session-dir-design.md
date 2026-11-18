<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0156 — A one-shot run can continue a saved session (--session DIR)](../../changes/active/0156-a-one-shot-run-can-continue-a-saved-session-session-dir.md)**
<!-- docket:backlink:end -->

# A one-shot run can continue a saved session

## Problem

A program that holds a conversation with fuse over many turns (the factory's orchestrator) has no way to continue one: a one-shot run writes no history and exits, `fuse shell` keeps history in memory behind a terminal UI, and the server tier that once offered resumable sessions was removed (317ab835). Callers rebuild the history into each task file themselves, and that hand-built context drifts from what the model actually saw.

## Design

### Runtime

`runtime.LoopConfig` gains `History []model.Message`: earlier turns of the conversation. `StartLoop` runs the root agent on `History` followed by the new user message (the task and its attachments). `Agent.Run` already takes a message history and keeps the system prompt out of it (`withSystem` adds it per request), so nothing else in the loop changes. The messages `Wait` returns are the whole conversation, compaction summaries included.

### `fuse --session DIR`

- `DIR` is created if missing. The conversation lives in `DIR/conversation.json`:
  `{"version": 1, "messages": [...]}`, each message with explicit JSON fields (`role`, `content`, `tool_calls` [`id`, `name`, `arguments`], `tool_call_id`, `name`, `reasoning`, `reasoning_field`, `attachments` [`media_type`, `name`, `ref`, `size`, `sha256`, `width`, `height`]). Attachment bytes are not saved: a resumed image is referenced, not resent.
- A missing file starts a new conversation; an unknown version or an unreadable file is an error (exit 2), never a silent fresh start.
- One turn at a time: the run holds an exclusive `flock` on `DIR/lock` for its whole life; a second run on the same session exits 2 with "session DIR is in use". The OS releases the lock if the process dies.
- Saved only on success: when the run ends with exit 0 the returned conversation is written to a temp file in `DIR` and renamed over `conversation.json`. A failed, stopped or timed-out run leaves the file as it was, so the session never holds half a turn; the caller retries the turn.
- Without `--session` nothing changes.

### What it does not do

No long-lived process, no shell resume, no replay of events.jsonl. Summarization keeps compacting tool output as today; very long text-only conversations are bounded by the model window as any run is.

## Tests

- runtime: `History` reaches the root before the task.
- cmd: two runs on one session against a fake gateway: the second request carries the first turn (user, assistant) before the new message; the file holds both turns.
- cmd: a failed turn leaves the session file unchanged; a held lock refuses a second run; an unknown version is refused; without `--session` no file is written.
