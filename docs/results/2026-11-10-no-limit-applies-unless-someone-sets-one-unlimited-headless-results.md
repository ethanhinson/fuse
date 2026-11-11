<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0155 — No limit applies unless someone sets one: unlimited headless turns and no assumed context window](../changes/archive/2026-11-11-0155-no-limit-applies-unless-someone-sets-one-unlimited-headless.md)**
<!-- docket:backlink:end -->

# No limit applies unless someone sets one - results

**Human action:** Recommended, not required. One optional live check against a real provider is listed below; nothing blocks merge.

## Outcome

fuse no longer invents two limits.

- A headless run (one-shot, non-TTY, `--approve-all`, mcp-server, research-probe) with `max_turns` unset now has no turn cap, the same as the interactive shell. An explicit `max_turns` still caps every entry point. The doom-loop detector still ends a run that repeats the same tool call, including under `--approve-all`.
- A model with no `context_window` now has an unknown window. fuse does not prune, summarize, clamp the output cap or abort against a guessed 128k size. When the provider rejects a request for context length, fuse learns a window from the size of the rejected request (prompt plus the output cap it sent), prunes and retries, and manages context against the learned window from then on. A later rejection lowers it. A run that cannot recover ends with `ErrContextTooLarge`, and the message says the window was learned from a provider rejection and names `context_window`.
- The interactive shell keeps a learned window for the rest of the session for that model, and resets it on a `/model` switch. Spawned child agents never share it.
- Each learn writes one stderr line and emits a new `context.window.learned` event.
- A configured `context_window` behaves as before.
- The models list shows `ctx unknown` for a model with no window, and the editor hint and docs no longer describe a 128k default.
- The decision is recorded as ADR-0083.

## Human actions and testing

- **Optional - live check against a real provider with no `context_window`.** Automated tests use a scripted gateway, so they do not prove that a real provider's rejection text is recognised.
  - Prerequisites: a build of this branch (`go build ./cmd/fuse`), a provider key, and a model alias whose real window is small or whose `context_window` you remove from your config.
  - Steps: run a one-shot task that reads several large files until the conversation exceeds the real window, for example `./fuse "read every .go file under internal/ and summarise each"`.
  - Expected: one line starting `context: <model> rejected a ~N-token request` and mentioning `learned window` appears, and the run continues instead of failing. If the provider's error text is not recognised, the run ends with the provider's own error. That would mean `isContextLengthErr` needs that provider's wording.
  - Cleanup: restore the `context_window` you removed.

## Verification performed

- The full suite (`make test`, including the `internal/e2e` black-box tests) passed through the docket build gate after the build, and again after the review fixes, at the final head.
- New tests cover:
  - `resolveMaxTurns` and the turn cap at the composition root.
  - A headless run going past 100 turns (unit and e2e).
  - The loop detector under `--approve-all` (e2e).
  - An unknown window sending more than 128k unpruned.
  - Learning and lowering a window, cap-inclusive learning, the unrecoverable message, the configured window still retrying once, summarization against a learned window, and the learned event.
  - The shell reusing a learned window across exchanges and resetting it on a model switch.
  - TUI text that fits 80 columns.
- Workers checked the key guards by mutation: each test went red with its guard removed. The guards are the accounting fix, the learn path, cap counting, the clamp and cut-off guards, shell seeding, and event emission.
- The review ran at the deep tier. It returned 7 findings: 0 blockers, 2 important, 5 minor. Six were fixed in-branch. One is deferred because it lives in a docket-generated file (see below).

## Known issues and follow-ups

- **A stuck headless run that keeps making different tool calls is no longer stopped at 100 turns.** This is intended by the spec and confirmed. Callers that want a ceiling set `max_turns`, `max_run_tokens` or `max_run_duration`. Suggested next action: none for this change. Change #147, which adds opt-in hard limits and clean shutdown for headless runs, covers the wall-clock side.
- **The first overflow with an unknown window costs one rejected request.** It costs two for a provider that counts only the prompt when the output cap is above 4096. This is confirmed by tests. Workaround: set `context_window` when the size is known.
- **If a gateway reports far fewer input tokens than the bytes/4 estimate**, the internal estimate can come out low after pruning. This is suspected only and has not been observed. Termination stays safe: no window is learned and the provider's error is returned. Suggested next action: watch traces for `context.window.learned` with odd sizes.
- **The spec copy on the feature branch has one em dash, in its docket-generated backlink line.** The repository rule forbids em dashes, but generated blocks must not be hand-edited. Impact: cosmetic. Suggested next action: change the docket backlink renderer so it uses a plain dash. That belongs to the docket tool, not this backlog. No existing change fits (checked 18).
