<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0157 — Long conversations stay correct in the shell and in --session runs](../changes/archive/2026-11-18-0157-long-conversations-stay-correct-in-the-shell-and-in-session.md)**
<!-- docket:backlink:end -->

# Results: long conversations stay correct in the shell and in --session

**Human action:** None required. `main` is not pushed; push when ready.

## Outcome

The ten conversation-handling defects from the review are fixed, one commit each, merged to `main` by fast-forward at 8b3b9147:

1. A compaction summary never lands inside a tool batch (94bbb8b2). `agent.CheckPairing` also checks adjacency.
2. The next run keeps what the earlier summary held, in the shell and across `--session` runs (b160f785).
3. Compaction covers plain text: old reasoning is dropped, then the oldest exchanges are folded into one summary that opens the conversation. The shell has `/compact` (1ba58f5a).
4. Compaction clears only tool results it summarized first; an oversized region is split into chunks (92446fff).
5. The context estimate counts the system prompt and tools, and carries the provider's prompt usage across runs; `conversation.json` gains an optional `usage` field and stays version 1 (b3b905a6).
6. A `--session` run that fails part way saves its complete steps and a note that it was interrupted (9bf82345).
7. Relevance ages tool results by step and reads the last real user request (445a27c2).
8. A steer the turn never read returns to the input box; `/clear` drops queued steers (56f0a08c).
9. Only real context-length errors shrink the learned window, never below 2048 tokens (4a5e9878).
10. Esc stops the running turn or a `/compact`; Ctrl+C still quits (66b9b7a7).

Also: a run records only its new request as `user.input` (932d0bba), the shell input row fits the terminal (c24811a8), a very long word no longer makes the transcript lag (7476853e), and staticcheck findings in touched files are cleared (8b3b9147). The README has a new "Long conversations" section.

## Verification performed

- Each fix has a test that failed before the fix. `go vet ./...` and `go test -count=1 ./...` (e2e included) passed on the branch, at every commit, and again before the merge.
- Live, through the LiteLLM gateway with a 6000-token window: five `--session` runs, the first stating a fact and the next three sending about 6 KB of text each. Runs 2 to 5 each compacted the conversation; the saved session held one user-role summary followed by the last exchange, and the fifth run answered the fact from the first correctly.

## Known issues and follow-ups

- A summarizer that is configured but fails leaves the conversation as it was and fails the turn, so the next turn can try again. With summarization off, the oldest messages are removed with a note instead.
- Compaction still edits the caller's history slice in place (clearing results, dropping reasoning), as before.
- `make test` can report the e2e package from cache after the binary's source changed; use `make e2e` (`-count=1`).
- About 60 staticcheck findings remain in files this change did not touch.
