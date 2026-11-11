<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0155 — No limit applies unless someone sets one: unlimited headless turns and no assumed context window](../../changes/archive/2026-11-11-0155-no-limit-applies-unless-someone-sets-one-unlimited-headless.md)**
<!-- docket:backlink:end -->

# No limit applies unless someone sets one - Implementation Plan

> **For agentic workers:** this plan is executed by `docket-build`, one task per worker, TDD (failing test first), one commit per task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Remove the two limits nobody configured: the 100-turn headless backstop and the assumed 128k context window. An unset `max_turns` is unlimited everywhere. An unset `context_window` is unknown: no proactive pruning or abort until the provider rejects a request for length, after which the loop learns a window from the rejected request and manages context against it for the rest of the run.

**Spec:** `docs/superpowers/specs/2026-11-11-no-limit-applies-unless-someone-sets-one-unlimited-headless-design.md` (change 0155).

**Architecture:** Turn cap: `resolveMaxTurns` in `cmd/fuse/run.go` stops looking at interactivity; `headlessTurnBackstop` and the dead `SetMaxTurnsExplicit` / `MaxTurnsExplicit` pair go away. `oneShotBudgetInteractive` and the `interactive` parameters stay, because they still decide the doom-loop approval hook (`loopApprovalFor`). Context window: `DefaultContextWindow` goes away; `Agent.Run` in `internal/agent/loop.go` keeps a loop-local `window` that is `0` (unknown) or a configured or learned size. All proactive context management is gated on `window > 0`. A context-length rejection with no configured window sets `window` from the hybrid estimate of the rejected request and goes round the turn again, so the existing prune, summarize and abort code runs against the learned window. A small accounting fix keeps the estimate correct across rejected retries.

**Tech stack:** Go. Tests: `go test` (unit), `internal/e2e` (black-box, execs the real binary against a scripted gateway). Full gate: `make test` (runs `go test ./...`, which includes `internal/e2e`). Also run `make lint` (`go vet ./...`) and `gofmt -l .` before the final commit.

**Writing rules for every task:** never write the em dash character in code, comments, strings, tests, docs or commit messages; use a plain `-`. Leave existing em dashes in lines you do not otherwise touch. Do not hand-edit any CHANGELOG or generated file. Commit messages carry no agent co-author line.

---

## Background the workers need

Read these before starting any task:

- `cmd/fuse/run.go` lines 113-148: `headlessTurnBackstop`, `resolveMaxTurns(cfgMaxTurns *int, interactive bool)`, `oneShotBudgetInteractive`.
- `cmd/fuse/run.go` `buildChildAgent` (around line 480) and `buildAgentCore` (around line 687). Both call `resolveMaxTurns(cfg.MaxTurns, interactive)` and `a.SetMaxTurnsExplicit(cfg.MaxTurns != nil)` (three `SetMaxTurnsExplicit` sites: lines ~516, ~735, ~750). `interactive` is also passed to `loopApprovalFor`, which must keep working.
- `internal/agent/agent.go`: field `maxTurnsExplicit` (~line 131), `SetMaxTurnsExplicit` / `MaxTurnsExplicit` (~lines 491-497), `SetMaxTurns` comment (~line 251), `ContextWindow` comment (~line 70).
- `internal/agent/loop.go`: constants block (~lines 43-67, includes `DefaultContextWindow`), `protectBudget`, `isContextLengthErr` (~line 362), and `Run` (~lines 382-720). Key spots in `Run`: window/budget/abortAt setup (~412-417), hybrid accounting vars `lastUsage`, `accounted`, `retriedContext` (~425-427), the top-of-turn estimate and over-budget block (~477-569), `clampOutputCap` call (~589), the error branch with the single configured retry (~622-637), `planCutOff` call (~643), and the usage update `lastUsage = resp.InputTokens; accounted = len(messages)` (~705-708).
- `internal/agent/truncation.go`: `clampOutputCap`, `nextOutputCap`, `planCutOff` all take `window`.
- Verified by grep at plan time: `MaxTurnsExplicit()` has **no production reader**; its only other user is `internal/runtime/exchange_cap_test.go:38` (`a.SetMaxTurnsExplicit(true)`). `DefaultContextWindow` is used only in `loop.go` and `internal/agent/context_window_test.go`.
- Tests that relied on the backstop to terminate: `internal/e2e/permissions_test.go` `TestDeniedCallLoop_Varying_BoundedByTurnCap` (asserts `srv.count() <= 100` with varying denied calls and no `max_turns`). Without the backstop it would never end. `cmd/fuse` loop-host tests that say "the turn cap ends the run" set `max_turns: 2`/`1` explicitly in `harnessOperator` and are unaffected.
- e2e harness facts: one-shot renders `Errorf` lines as `! <text>` on **stdout** (`internal/tui/renderer.go` `Errorf` writes to `r.w`, which is stdout for one-shot); the final run error is printed on **stderr**. Assert on `r.stdout + r.stderr` when you are not sure which stream carries a line. `h.mextra` appends to the model entry (for `context_window`), `h.extra` to the top-level config (for `max_turns`).
- Relevant learnings (from the learnings ledger):
  - `security-knob-inert-at-composition-root`: assert the turn cap at the composition root (`buildAgentCore` / `buildChildAgent`), not only in `resolveMaxTurns`.
  - `patch-every-cloned-child-builder`: there are several agent builders in `cmd/fuse` (`main.go` one-shot via `runtime_binding.go`, `shell.go`, research-probe, mcp-server). All of them reach `buildAgentCore` or `buildChildAgent`; grep `resolveMaxTurns(` and `SetMaxTurnsExplicit(` at fix time and make sure every site is updated, rather than trusting this list.
  - `closed-label-allowlist-silently-swallows-enum-growth`: do not add a new `event.Kind` for "window learned". Use a renderer log line (the same channel the existing `context: ... cleared ...` prune lines use). The spec allows "one event or log line".
  - `verify-from-feature-worktree-binary`: the e2e suite builds the binary from the current checkout; run it from the feature worktree.

---

## Task 1: Headless runs have no turn cap unless `max_turns` is set

**Suggested tier:** standard.

**Files:**
- Modify: `cmd/fuse/run.go`
- Modify: `cmd/fuse/main.go` (comment at ~lines 202-206 only)
- Modify: `internal/agent/agent.go`
- Modify: `internal/config/schema.go` (comments only: `MaxTurns` field doc ~lines 280-285, `Default()` comment ~lines 526-528)
- Modify: `internal/config/loader_test.go` (comment at ~lines 20-22 only)
- Modify: `internal/runtime/exchange_cap_test.go` (drop the `SetMaxTurnsExplicit(true)` line)
- Test: `cmd/fuse/run_helpers_test.go`, `cmd/fuse/shell_test.go`, `internal/agent/loop_test.go`

- [ ] **Step 1: Write the failing tests.**

  In `cmd/fuse/run_helpers_test.go`:
  - Rewrite `TestResolveMaxTurns` for the new one-argument signature `resolveMaxTurns(cfgMaxTurns *int) int`: `nil -> 0`, `ptr(0) -> 0`, `ptr(9) -> 9`.
  - In `TestOneShotBudgetPostureUnderApproveAll`, keep the posture and loop-hook assertions exactly as they are. Replace the backstop block (lines ~75-84) so that the test asserts `resolveMaxTurns(nil) == 0` and `resolveMaxTurns(ptr(9)) == 9` regardless of posture. Update the doc comment above the test: `--approve-all` still makes the budget posture headless so a doom-loop trip aborts; there is no longer a turn backstop. Remove every reference to `headlessTurnBackstop`.

  In `cmd/fuse/shell_test.go` add `TestBuildAgentTurnCapAtCompositionRoot` (composition-root wiring test). Use `config.Default()` (its `MaxTurns` is nil) and `model.DefaultRegistry()` as `TestBuildAgentLoopApprovalWiring` does. Assert:
  - `buildAgentCore(..., interactive=false, ...)` with `cfg.MaxTurns == nil` -> `a.MaxTurns() == 0`.
  - The same with `interactive=true` -> `0`.
  - With `cfg.MaxTurns = &seven` (7), both postures -> `7`.
  - `buildChildAgent(cfg, reg, reg.Default, r, "sys", toolReg.Clone(), permissions.AlwaysApprove, nil, "child", nil, false, nil, nil)` with nil `MaxTurns` -> `0`, and with 7 -> `7`.
  This needs a new getter, `func (a *Agent) MaxTurns() int`, which does not exist yet (that is the failing part).

  In `internal/agent/loop_test.go` add:
  - `TestRunUnlimitedRunsPastOneHundredTurns`: `New(comp, exec, nopRenderer{}, "m", "", 0, 100)` with 150 distinct tool-call responses (distinct `Arguments` so the doom-loop detector never trips), then `{Content: "done"}`. Assert `err == nil` and `len(exec.calls) == 150`.
  - `TestRunUnlimitedStillStopsOnDoomLoop`: `maxTurns` 0, `LoopApproval` nil (the headless and `--approve-all` posture), five identical tool-call responses. Assert `errors.Is(err, ErrLoopDetected)`.

- [ ] **Step 2: Run the tests and see them fail.**
  `go test ./cmd/fuse/ -run 'TestResolveMaxTurns|TestOneShotBudgetPostureUnderApproveAll|TestBuildAgentTurnCapAtCompositionRoot' ./internal/agent/ -run 'TestRunUnlimited'`
  Expected: compile failure (`resolveMaxTurns` arity, `MaxTurns` undefined). The two agent tests may already pass; that is fine, because they pin behaviour that this change must keep.

- [ ] **Step 3: Implement.**
  - `cmd/fuse/run.go`: delete `headlessTurnBackstop` and its comment. Change `resolveMaxTurns` to `func resolveMaxTurns(cfgMaxTurns *int) int` that returns `0` for nil and `*cfgMaxTurns` otherwise. Its doc comment says: unset means unlimited in every entry point; an explicit `0` is unlimited; `N > 0` caps every entry point; the doom-loop detector (and the opt-in `max_turns`, `max_run_tokens`, `max_run_duration`) is what stops a stuck headless run. Update both call sites to `resolveMaxTurns(cfg.MaxTurns)`. Keep the `interactive` parameters of `buildChildAgent` and `buildAgentCore`, because `loopApprovalFor(approve, interactive)` still uses them.
  - Rewrite the `oneShotBudgetInteractive` doc comment: it decides the doom-loop approval posture only. With `--approve-all` the loop hook stays nil, so a trip aborts instead of auto-continuing. Drop the "backstops at 100" wording. Same for the comment block in `cmd/fuse/main.go` around `oneShotBudget :=`.
  - Delete all three `a.SetMaxTurnsExplicit(cfg.MaxTurns != nil)` lines.
  - `internal/agent/agent.go`: delete the `maxTurnsExplicit` field, `SetMaxTurnsExplicit` and `MaxTurnsExplicit`. Add:
    ```go
    // MaxTurns reports the per-run turn cap; 0 means unlimited.
    func (a *Agent) MaxTurns() int { return a.maxTurns }
    ```
    In the `SetMaxTurns` comment, drop the "independent of any per-turn backstop a one-shot binding wants" clause.
  - `internal/runtime/exchange_cap_test.go`: delete `a.SetMaxTurnsExplicit(true)`. The test still passes `maxTurns` 2 to `agent.New` and still expects `ErrMaxTurns`.
  - `internal/config/schema.go` comments: `MaxTurns` nil means unset, which is unlimited in every entry point, the same as an explicit 0. The pointer stays because layered configs need to tell an omitted key from an explicit override. Fix the `Default()` comment the same way. `internal/config/loader_test.go` comment: same wording, with no "100 headless".
  - Grep the worktree for `headlessTurnBackstop`, `SetMaxTurnsExplicit`, `MaxTurnsExplicit` and `backstops at 100`. Outside `docs/superpowers/` and `.docket/`, nothing may remain.

- [ ] **Step 4: Run the tests and see them pass.**
  `go test ./cmd/fuse/ ./internal/agent/ ./internal/runtime/ ./internal/config/`
  Expected: PASS.

- [ ] **Step 5: Commit.**
  `git commit -m "fix(run): no turn cap on headless runs unless max_turns is set"`

---

## Task 2: Headless turn behaviour, end to end

**Suggested tier:** standard.

**Files:**
- Modify: `internal/e2e/permissions_test.go`
- Modify: `internal/e2e/headless_test.go`

- [ ] **Step 1: Write and update the e2e tests.**
  - Add `TestHeadless_NoMaxTurns_RunsPastOneHundredTurns` to `internal/e2e/headless_test.go`. The router answers request `r.N < 105` with `tool("list_directory", fmt.Sprintf(`{"path":".","n":%d}`, r.N))` and anything after that with `text("all done")`. Run with no `max_turns`. Assert exit 0, `srv.count() == 106`, and that stdout contains `all done`. This is the end-to-end proof that the old 100-turn backstop is gone. (`list_directory` is read-only, so it needs no `--approve-all`. If the harness denies it, use `h.run("--approve-all")`.)
  - Add `TestHeadless_ApproveAll_IdenticalCallsStopOnLoopDetector` next to it: the router always returns the identical `tool("list_directory", map[string]string{"path": "."})`. Run with `h.run("--approve-all")` and no `max_turns`. Assert exit 1, `srv.count() <= 5`, and that stderr contains `loop detected`. This pins the spec requirement that `--approve-all` still ends a doom loop instead of auto-continuing it.
  - In `internal/e2e/permissions_test.go`, change `TestDeniedCallLoop_Varying_BoundedByTurnCap`. It relied on the removed backstop and would now never end. Rename it to `TestDeniedCallLoop_Varying_BoundedByExplicitTurnCap`, set `h.extra = "max_turns: 20\n"`, and assert exit 1, `srv.count() == 20`, and that stderr contains `max turns`. Rewrite the comment: different arguments dodge the identical-call detector, so a caller who wants a ceiling sets `max_turns`.

- [ ] **Step 2: Run them.**
  `go test ./internal/e2e/ -count=1 -run 'TestHeadless_|TestDeniedCallLoop_|TestMaxTurns_Nonzero|TestSuccessfulCallLoop_Identical'`
  Expected: PASS. Task 1 already removed the backstop, so the new tests should pass on first run. If `RunsPastOneHundredTurns` fails at exactly 100 requests, Task 1 missed a builder site: fix it there.

- [ ] **Step 3: Commit.**
  `git commit -m "test(e2e): headless runs are uncapped by default; explicit max_turns and the loop detector still stop them"`

---

## Task 3: An unconfigured context window is unknown, not 128k

**Suggested tier:** standard.

**Files:**
- Modify: `internal/agent/loop.go`
- Modify: `internal/agent/truncation.go`
- Delete: `internal/agent/context_window_test.go`
- Modify: `internal/agent/agent.go` (`ContextWindow` comment)
- Test: `internal/agent/loop_test.go`, `internal/agent/truncation_test.go`

- [ ] **Step 1: Write the failing tests.**
  - `internal/agent/loop_test.go` `TestRunUnknownWindowSendsLargeContextUnpruned`: build an agent with `ContextWindow` left at 0 and `maxTokens` 8000. History: a user message plus three `Role: "tool"` results of 200,000 bytes each (about 150k estimated tokens in total, above the old 128k guess). Use a `capturingCompleter` that records the request. Assert: no error (in particular no `ErrContextTooLarge`); the completer was called once; no message in the returned history equals `prunedStub`; the captured `req.MaxTokens == 8000` (no window clamp).
  - `internal/agent/truncation_test.go` `TestClampOutputCapUnknownWindow`: `clampOutputCap(8000, 0, 500_000) == 8000` and `nextOutputCap(8000, 32000, 0, 500_000) == 16000`.
  - Delete `internal/agent/context_window_test.go`. It only pins `DefaultContextWindow == 128_000`.

- [ ] **Step 2: Run them and see them fail.**
  `go test ./internal/agent/ -run 'TestRunUnknownWindow|TestClampOutputCapUnknownWindow'`
  Expected: FAIL (today the old window prunes or aborts, and the clamp uses `window - estimate`).

- [ ] **Step 3: Implement.**
  - `loop.go`: delete `DefaultContextWindow` and its comment from the const block.
  - In `Run`, replace the window setup with `window := a.ContextWindow` (0 = unknown). Drop the `budget` and `abortAt` locals computed once before the loop. Compute them where they are used, from the current `window`, so a learned window (Task 4) takes effect: `budget := window * pruneThresholdPct / 100` and `abortAt := window * abortThresholdPct / 100` inside the turn body, after `estimate` is computed.
  - Gate the whole top-of-turn over-budget block (summarize, prune, harder prune, `ErrContextTooLarge`) on `window > 0 && estimate > budget`. With an unknown window, requests are sent as they are.
  - `truncation.go` `clampOutputCap`: return `limit` unchanged when `window <= 0` (`if limit <= 0 || window <= 0 { return limit }`). Update its doc comment: an unknown window (0) never clamps. `nextOutputCap` and `planCutOff` inherit this through `clampOutputCap`.
  - Leave the error branch alone in this task. With `window == 0` the existing configured-retry branch runs `protectBudget(0, true) == 0`, which is a valid (aggressive) prune; Task 4 replaces this path.
  - `agent.go` `ContextWindow` comment: the model's context size in tokens; 0 means unknown, so the loop does no proactive pruning, and learns a window for the run if the provider rejects a request for length (see `Run`).

- [ ] **Step 4: Run the agent package and the context e2e tests.**
  `go test ./internal/agent/ && go test ./internal/e2e/ -count=1 -run 'TestContext|TestCompaction'`
  Expected: PASS. Every existing pruning, summarization, abort and truncation test sets `ContextWindow` explicitly and must pass unchanged. If one does not set it and fails, it relied on the 128k guess: give it an explicit `ContextWindow` of `128_000` rather than changing its assertions, and say so in the commit body.

- [ ] **Step 5: Commit.**
  `git commit -m "fix(agent): an unset context_window is unknown, not 128k"`

---

## Task 4: Learn a context window from a provider length rejection

**Suggested tier:** premium. This is a loop-control change with a termination argument and an accounting fix. A mistake here either loops forever on rejections or ends healthy runs.

**Files:**
- Modify: `internal/agent/loop.go`
- Test: `internal/agent/loop_test.go` (or a new `internal/agent/learned_window_test.go`)
- Modify (only if needed, see Step 5): `internal/e2e/context_test.go`, `internal/e2e/model_test.go`

**Design (implement exactly this):**

1. **Pure helper** in `loop.go`:
   ```go
   // learnContextWindow returns the window to manage context against after the
   // provider rejected a request of about rejected tokens for length, when the
   // window was not configured. current is 0 (unknown) or a window learned earlier
   // in this run. A learned window only ever goes down. ok is false when nothing
   // can be learned (no size to learn from, or no lower window).
   func learnContextWindow(current, rejected int) (window int, ok bool) {
   	if rejected <= 0 || (current > 0 && rejected >= current) {
   		return current, false
   	}
   	return rejected, true
   }
   ```

2. **Accounting fix (needed for a second rejection to work).** Today the next turn recomputes `estimate := lastUsage + messagesSize(messages[accounted:])/bytesPerToken`. Stubs written by a prune inside `messages[:accounted]` are still counted in `lastUsage` until the next successful response reports fresh usage. After a rejection there is no successful response, so the recomputed estimate comes back near its pre-prune size. Against a lowered learned window that would end a recoverable run with a false `ErrContextTooLarge`. Fix:
   - Add a loop-local `prunedAccounted := 0`, documented as the tokens cleared inside `messages[:accounted]` since `lastUsage` was last set.
   - Add a closure in `Run` that every prune call site uses:
     ```go
     prune := func(protect int) int {
     	before := messagesSize(messages[:accounted])
     	freed := pruneOldToolResults(messages, protect, a.relevanceScorer, a.recencyFloorPct())
     	prunedAccounted += (before - messagesSize(messages[:accounted])) / bytesPerToken
     	return freed
     }
     ```
     Replace the three existing `pruneOldToolResults(messages, protectBudget(window, ...), a.relevanceScorer, a.recencyFloorPct())` calls in `Run` with `prune(protectBudget(window, ...))`.
   - Both estimate formulas in `Run` (top of turn, and the recompute after summarization) become `lastUsage - prunedAccounted + messagesSize(messages[accounted:])/bytesPerToken`.
   - Where `lastUsage = resp.InputTokens` is set, also set `prunedAccounted = 0`.
   - On every path that does not end in a rejection, `prunedAccounted` is 0 whenever an estimate is computed, because each successful response resets it. Configured-window behaviour is therefore unchanged except for a more accurate estimate on the one turn after a configured-window rejection retry.

3. **Error branch.** Replace the current `if isContextLengthErr(err) && !retriedContext { ... }` block with:
   ```go
   if isContextLengthErr(err) {
   	if a.ContextWindow <= 0 {
   		// No configured window: learn one from what the provider just refused,
   		// then go round the turn again so the normal prune/summarize/abort path
   		// runs against it.
   		if learned, ok := learnContextWindow(window, estimate); ok {
   			window = learned
   			learnedFrom = err
   			a.renderer.Errorf("context: %s rejected a ~%d-token request for length; managing context against a learned window of ~%d tokens for the rest of this run (set context_window for this model to skip the rejected request)", a.modelID, estimate, window)
   			turn--
   			continue
   		}
   	} else if !retriedContext {
   		// ... the existing configured-window single retry, unchanged, except that it calls prune(...) ...
   	}
   }
   ```
   `learnedFrom error` is a new loop-local (nil until a window is learned). `estimate` here is the hybrid estimate of the request just sent (net of anything pruned this turn). That is "the size of the rejected request" from the spec, in the same units the thresholds use.

4. **Abort message.** In the top-of-turn `estimate > abortAt` branch, when `learnedFrom != nil`, build:
   ```go
   fmt.Errorf("%w: ~%d tokens against a window of ~%d tokens learned from a provider rejection (%v), even after pruning - set context_window for this model to its real size, delegate to spawn_agent children, or start a fresh session", ErrContextTooLarge, estimate, window, learnedFrom)
   ```
   Use exact token counts, not `k`: a learned window can be tiny (a first-turn rejection). Keep the existing message unchanged for a configured window. Including the provider error keeps the real limit visible to the operator. It also keeps `TestContextOverflow_NoToolHistory_FailsOnce`'s `"maximum context length"` stderr assertion true.

5. **Why it terminates.** After a learn, the next pass through the top of the turn has `estimate == window > budget`, so it prunes. Either the estimate drops to `<= abortAt` (95% of the learned window) and the request is sent, or it aborts. A later rejection therefore learns a window at most 95% of the previous one (`learnContextWindow` only lowers), so the window shrinks geometrically until the request fits or pruning cannot help and the run aborts. `learnContextWindow` returning `ok == false` falls through to the existing `model error` return with the provider's error.

6. Subagents need nothing: each child is its own `Agent.Run` with its own loop locals, and inherits only the configured `ContextWindow` from its builder. Do not share the learned window.

- [ ] **Step 1: Write the failing tests.**
  - `TestLearnContextWindow` (table): `(0, 0) -> (0, false)`; `(0, 1000) -> (1000, true)`; `(1000, 800) -> (800, true)`; `(1000, 1000) -> (1000, false)`; `(1000, 1200) -> (1000, false)`.
  - `TestRunUnknownWindowLearnsFromRejectionAndRecovers`: `ContextWindow` 0. Use `lengthRejectingCompleter{limitBytes: 70_000}` (already in `loop_test.go`) and the history from `TestRunRecoversFromProviderLengthRejection` (user plus two 60,000-byte tool results). Capture renderer lines with a small recording renderer (`Errorf` appends `fmt.Sprintf(format, args...)`). Assert: no error; `comp.calls == 2`; `hist[1].Content == prunedStub`; one recorded line contains `learned window`.
  - `TestRunLearnedWindowLowersOnSecondRejection`. This test must catch the stale-accounting bug, so the accounted prefix must be non-empty when the rejections happen:
    - The completer computes `in := messagesSize(req.Messages) / bytesPerToken`. While `in` is at most its current limit, it returns a tool call with distinct arguments and `InputTokens: in` (so `lastUsage` and `accounted` advance). Once it is over the limit, it returns `errors.New("gateway status 400: maximum context length exceeded")`. After its first rejection it lowers its limit (for example to 60% of the first limit), so the request sent after the first learn-and-prune is rejected again. After the second rejection it accepts anything at or under the lower limit and answers `{Content: "done"}`.
    - The exec returns a large result (for example 40,000 bytes) for each call, so a few turns cross the first limit.
    - Assert: no error; exactly two rejections; two recorded `learned window` lines, the second reporting a smaller window than the first (parse the `~%d tokens` numbers or record the window on each line); the final accepted request is at or under the lower limit.
    - Tune the sizes until the test **fails** with the accounting fix disabled (temporarily drop `prunedAccounted` from the estimate) and passes with it. State the chosen numbers in a comment.
  - `TestRunUnknownWindowUnrecoverableEndsWithLearnedMessage`: `ContextWindow` 0. History is a single user message of 100,000 bytes (not prunable). The completer always returns the length error. Assert `errors.Is(err, ErrContextTooLarge)`; the message contains `learned from a provider rejection`, `context_window` and `maximum context length`; and the completer was called exactly once.
  - `TestRunConfiguredWindowRejectionStillRetriesOnce`: `ContextWindow` 200,000, a completer that always rejects, prunable history. Assert exactly 2 calls, the provider error is returned (not `ErrContextTooLarge`), and no `learned window` line is emitted. This pins "configured window: behaviour unchanged".

- [ ] **Step 2: Run them and see them fail.**
  `go test ./internal/agent/ -run 'TestLearnContextWindow|TestRunUnknownWindow|TestRunLearnedWindow|TestRunConfiguredWindowRejection'`
  Expected: FAIL (helper undefined; the unknown-window path still uses the single configured retry).

- [ ] **Step 3: Implement the design above.**

- [ ] **Step 4: Run the agent package.**
  `go test ./internal/agent/`
  Expected: PASS, including every existing configured-window test (`TestRunPrunesOldToolResultsWhenOverBudget`, `TestRunErrsWhenPruningInsufficient`, `TestRunRecoversFromProviderLengthRejection`, the truncation and summarize tests) with no assertion changes.

- [ ] **Step 5: Run the e2e context tests.**
  `go test ./internal/e2e/ -count=1 -run 'TestContext|TestCompaction'`
  These now take the learned path because they set no `context_window`: `TestContextOverflow_Persistent_Stops`, `TestContextOverflow_NoToolHistory_FailsOnce` (in `model_test.go`), and `TestContextOverflow_StreamedErrorEvent`.
  - `NoToolHistory_FailsOnce` and `StreamedErrorEvent` must still see exactly 1 request and exit 1.
  - `Persistent_Stops`: expected main-model requests are read, rejected, pruned retry rejected, then abort, so 3. Summarizer requests (no tools offered) may add to `srv.count()`. If the `<= 4` bound fails only because of summarizer requests, count only requests that offer tools (`len(r.Tools) > 0`) and keep the bound at 4. Also assert that `r.stdout + r.stderr` contains `learned window`. Do not loosen the bound for any other reason. A higher main-request count means the termination argument is broken.

- [ ] **Step 6: Commit.**
  `git commit -m "fix(agent): learn the context window from a provider length rejection when none is configured"`

---

## Task 5: Learned window, end to end

**Suggested tier:** standard.

**Files:**
- Modify: `internal/e2e/context_test.go`

- [ ] **Step 1: Write the e2e test.** `TestContextOverflow_UnknownWindow_LearnsAndFinishes`. Use the paging pattern from `paging()`: each main request reads the next 200 lines of `bigFile` and reports `usageChunk(len(r.Raw)/4, 5)`. Summarizer requests (no tools) get `text("SUMMARY-MARKER")`. Add a byte limit: when a main request's `len(r.Raw)` exceeds `limitBytes`, return `status(400, `{"error":{"message":"This model's maximum context length is N tokens"}}`)`. After 12 successful reads, answer `finished paging`. Set no `context_window`. Choose `limitBytes` so the limit is crossed after about 3-4 reads (each read adds about 16KB). Assert:
  - exit 0, and stdout contains `finished paging`;
  - `r.stdout + r.stderr` contains `learned window`;
  - at most 2 requests were rejected (count requests whose `len(r.Raw) > limitBytes`);
  - the last request contains `prunedMarker` and `len(raw) <= limitBytes`.
  Add a second subtest or test with `context_window` set well above the limit's token size that shows the configured path keeps the single-retry behaviour. Only add it if it is not already covered by `TestContextOverflow_AfterToolHistory_BoundedNoInfiniteLoop`; that test already pins the configured path, so this is optional.

- [ ] **Step 2: Run it.** `go test ./internal/e2e/ -count=1 -run 'TestContextOverflow_UnknownWindow'`. Expected: PASS. If it fails, debug the loop from Task 4, not the test's assertions.

- [ ] **Step 3: Commit.** `git commit -m "test(e2e): an unknown context window is learned from the provider and the run finishes"`

---

## Task 6: Surfaces that describe the old defaults

**Suggested tier:** economy.

**Files:**
- Modify: `internal/tui/models_command.go`, `internal/tui/models_command_test.go`
- Modify: `internal/tui/models_editor.go`, plus `internal/tui/models_editor_test.go` if it asserts the hint
- Modify: `internal/model/registry.go` (`ContextWindow` comment ~line 18)
- Modify: `internal/config/schema.go` (`ContextWindow` comment ~line 41)
- Modify: `docs/designs/context-management.md` (~lines 57-67)
- Modify: `evals/livecodebench/bench.py` (`--fuse-context-window` help text ~line 803)

- [ ] **Step 1: Update the tests first.**
  - `TestLimitsCell`: a zero window renders `default · ctx unknown`; `131072` still renders `16384 out · 128k ctx`.
  - If a models-editor test renders the focused context-window field, assert the new hint text. Also assert that the rendered line fits an 80-column view (`lipgloss.Width(line) <= 80`), following the TUI width learnings.
  - Leave `models_discover_test.go`'s `"128k ctx"` alone: it comes from a catalog entry with a real window.

- [ ] **Step 2: Run them and see them fail.** `go test ./internal/tui/ -run 'TestLimitsCell|ModelsEditor'`

- [ ] **Step 3: Implement.**
  - `limitsCell`: a zero window renders `ctx unknown`. Rewrite its doc comment: a zero context window is unknown; fuse learns one from the provider if a request is rejected for length, and setting it is recommended.
  - Editor hint for `fieldContextWindow`: `context size (blank = unknown, learned on overflow; set if known)`. Keep it short enough to fit 80 columns next to the label.
  - `parseNonNegative` doc comment: "empty means zero (unset)" instead of "harness default".
  - `registry.go` and `schema.go` `ContextWindow` comments: `0 = unknown (learned from a provider length rejection; set it when known)`.
  - `docs/designs/context-management.md`: replace "per-model `context_window` config (default 128k)" with text that says an unset window is unknown: no proactive pruning, and on the first provider length rejection the loop learns a window from the rejected request's size, then prunes, summarizes and aborts against it for the rest of the run. Update the "Context-length error recovery" bullet: with a configured window the loop prunes hard and retries once; with an unknown window it learns and lowers a window instead.
  - `bench.py` help text: `(0 = unset: fuse learns the window from the provider on overflow)`.
  - Grep for the old wording, excluding `docs/superpowers/`, `docs/results/`, `docs/reviews/` and `.docket/`: `128k default`, `128k harness`, `harness default (128k)`, `default 128k`, `DefaultContextWindow`. Nothing may remain. (`evals/livecodebench/README.md`'s "128k" lines are about the output cap, not the context window; leave them.)

- [ ] **Step 4: Run the tests.** `go test ./internal/tui/ ./internal/model/ ./internal/config/`. Expected: PASS.

- [ ] **Step 5: Commit.** `git commit -m "docs(models): describe an unset context window as unknown, not 128k"`

---

## Final gate

- [ ] `gofmt -l .` prints nothing.
- [ ] `make lint` passes.
- [ ] `make test` passes (this includes `internal/e2e`). Watch for any test that now **hangs**. A hang means a test ended only because of the removed 100-turn backstop: give it an explicit `max_turns` in its config and say why in a comment, as Task 2 did for `TestDeniedCallLoop_Varying_BoundedByExplicitTurnCap`. Do not reintroduce a default cap.
- [ ] Grep check over the feature tree (excluding `.docket/`, `docs/superpowers/`, `docs/results/`, `docs/reviews/`): no `headlessTurnBackstop`, `MaxTurnsExplicit`, `DefaultContextWindow` or `128k default`, and no em dash added by this change (`git diff origin/main -- . | grep '^+' | grep -c $'\xe2\x80\x94'` reports 0).

## Spec coverage map

| Spec item | Task |
|---|---|
| Unset `max_turns` is unlimited in every entry point; explicit `N` caps everywhere; `0` stays unlimited | 1, 2 |
| Remove `headlessTurnBackstop`, simplify `resolveMaxTurns`, remove dead `SetMaxTurnsExplicit` | 1 |
| Doom-loop detector still ends a headless run, including under `--approve-all` | 1 (unit), 2 (e2e) |
| `oneShotBudgetInteractive` keeps deciding the doom-loop posture | 1 (existing test kept) |
| Remove `DefaultContextWindow`; unknown window means no pruning, no summarization trigger, no `ErrContextTooLarge` | 3 |
| Learn the window from a rejected request's size; prune, summarize and retry; normal thresholds against the learned window; lowers on a later rejection; loop-local only | 4, 5 |
| Configured window unchanged, including the single retry | 3, 4 (pinned by tests) |
| Unrecoverable case: `ErrContextTooLarge` naming the learned window and `context_window` | 4 |
| One log line when a window is learned (size, model) | 4 |
| Subagents do not share a learned window | 4 (by construction: loop-local state) |
| TUI models list and editor text; `ContextWindow` comments; docs | 6 |
| configcheck or validation output of the effective window or turn cap | Checked at plan time: `internal/configcheck` and `internal/config/validate.go` report neither, so nothing to change |
