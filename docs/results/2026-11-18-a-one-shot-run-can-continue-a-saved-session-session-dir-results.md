<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0156 — A one-shot run can continue a saved session (--session DIR)](../changes/active/0156-a-one-shot-run-can-continue-a-saved-session-session-dir.md)**
<!-- docket:backlink:end -->

# Results: a one-shot run can continue a saved session

**Human action:** None required. `main` is 5 commits ahead of `origin/main` (this change's 2 and 3 earlier sandbox commits); push when ready.

## Outcome

`fuse --session DIR` continues a conversation across one-shot runs. The run loads `DIR/conversation.json`, runs the new task on top of it and, on success, saves the whole conversation back atomically. A failed or stopped turn saves nothing. One run at a time holds a session (flock). `runtime.LoopConfig.History` carries the earlier turns into the root agent. Merged to `main` as 223f8fb9 and 67be103e (fast-forward).

## Verification performed

- `make test` passed on the feature branch and again on `main` after the merge.
- New tests:
  - the runtime runs on the history before the task;
  - two runs on one session send the first turn ahead of the second, and the file holds both;
  - a failed turn leaves the session unchanged;
  - a held lock and an unknown file version are refused before the model is called;
  - the conversation file round-trips tool calls, reasoning and image references.
- Live, through the factory's orchestrator (fuse-factory 7b4afb7): an interview over three turns on one session. The third turn sent only the new message and four lines of run state, and the model answered from its own conversation ("You said: want-to-read, reading, and finished"). The session file held 16 messages.

## Known issues and follow-ups

- `staticcheck ./...` reports 63 findings already present in about 30 files (mostly unused test helpers in `internal/tui`, `internal/mcp`, `cmd/fuse`). This change removed the ones in the files it touched. fuse's lint gate is `go vet`, which passes.
- Long text-only conversations are not compacted beyond what summarization does for tool output. A very long session is bounded by the model window.
