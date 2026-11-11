<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0155 — No limit applies unless someone sets one: unlimited headless turns and no assumed context window](../../changes/active/0155-no-limit-applies-unless-someone-sets-one-unlimited-headless.md)**
<!-- docket:backlink:end -->

# No limit applies unless someone sets one

## Problem

fuse applies two limits that nobody configured:

1. **Headless turn backstop.** `resolveMaxTurns` in `cmd/fuse/run.go` returns `headlessTurnBackstop` (100) when `max_turns` is unset and the run is headless (one-shot, non-TTY, `--approve-all`, mcp-server, research-probe). The interactive shell is unlimited in the same situation.
2. **Assumed context window.** `Agent.run` in `internal/agent/loop.go` substitutes `DefaultContextWindow` (128,000) when the model has no `context_window`. It then prunes at 85% of that guess and ends the run with `ErrContextTooLarge` at 95% of it.

Both hurt unattended callers. A caller that drives many headless runs must write `max_turns: 0` into every generated config, and a model alias that lacks `context_window` is silently run as a 128k model. In one long unattended run on models with much larger windows, six reviewer runs ended with "conversation context too large: ~135k tokens against a 128k window even after pruning". The run was healthy; the limit was invented.

## Decisions

### 1. Headless runs have no turn cap unless `max_turns` is set

- `max_turns` unset means unlimited in every entry point. `max_turns: N` (N > 0) caps every entry point. `max_turns: 0` stays valid and means unlimited, as today.
- Remove `headlessTurnBackstop`. `resolveMaxTurns` no longer needs the `interactive` argument for the cap; simplify it and its callers, and remove what becomes dead (for example `SetMaxTurnsExplicit` if nothing still needs to know whether the value was explicit; check its readers first).
- The doom-loop detector is the protection against a stuck headless run. Confirm with a test that a headless run with no turn cap still ends with `ErrLoopDetected` on repeated identical tool calls, including under `--approve-all` (the loop approval hook stays nil there, so detection ends the run rather than asking).
- `oneShotBudgetInteractive` keeps deciding the doom-loop approval posture. Only the turn cap part of that decision goes away.
- Hosted or per-tenant loop limits are explicit configuration and do not change.

### 2. An unconfigured context window is unknown, not 128k

- Remove `DefaultContextWindow`. `ContextWindow == 0` means "unknown".
- **Unknown window, before any provider rejection:** no proactive pruning, no summarization trigger and no `ErrContextTooLarge`. Requests are sent as they are.
- **Unknown window, provider rejects for context length** (`isContextLengthErr`): set a learned window for this run from the token size of the rejected request (use the same hybrid accounting the loop already keeps: last exact usage plus the estimate for messages added since). From then on the run behaves as if that learned window had been configured: prune, summarize when configured, retry the call, and apply the normal 85% and 95% thresholds against the learned window. Learning lowers the window again if a later request is still rejected. The learned window lives in loop state only; it is not written to config.
- **Configured window:** behaviour is unchanged, including the existing single retry on a context-length rejection.
- If a context-length rejection cannot be recovered (pruning cannot get under the learned window), the run ends with `ErrContextTooLarge` as today, and the message says the window was learned from a provider rejection and names the `context_window` setting, so the operator can set the real value.
- Emit one event or log line when a window is learned (size learned, model), so the behaviour is visible in traces.
- Subagents: a child agent inherits the model's configured window as today. A learned window is per loop; do not share it across agents in this change.

### 3. Surfaces that describe the old defaults

- `internal/tui/models_editor.go` and `internal/tui/models_command.go` say "blank = 128k default" and render "128k ctx" for zero. Change them to say the window is unknown and fuse will learn it from the provider if needed, and that setting it is recommended.
- Comments on `ContextWindow` in `internal/config/schema.go`, `internal/model/registry.go`, `internal/agent/agent.go`.
- README and docs that mention the 100 turn headless backstop or the 128k default.
- `fuse` config validation or `configcheck` output, if it reports the effective window or turn cap.

## Non-goals

- Removing `max_turns`, `max_run_tokens`, `max_run_duration` or `max_spawns`. They stay as opt-in settings.
- Discovering a model's window from a gateway or provider API.
- Changing how pruning or summarization select what to drop when the window is known.

## Testing

- `resolveMaxTurns`: unset gives unlimited for interactive and headless; explicit values pass through.
- A headless agent loop with no cap runs past 100 turns with a scripted model, and still stops on the doom-loop detector.
- Unknown window: a conversation larger than 128k tokens is sent without pruning and without `ErrContextTooLarge`.
- Unknown window with a scripted context-length rejection: the loop learns a window, prunes, retries and continues; a second rejection lowers the learned window; an unrecoverable case ends with the new message.
- Configured window: existing pruning, summarization and abort tests pass unchanged.
- TUI model list and editor text for a model with no window.

## Risks

- A stuck headless run that keeps making different tool calls is no longer stopped at 100 turns. Callers that want a ceiling set `max_turns`, `max_run_tokens` or `max_run_duration`.
- With an unknown window, the first context overflow costs one rejected request before pruning starts. Setting `context_window` avoids it.
- Provider error texts vary; `isContextLengthErr` already carries that risk for the configured-window retry. If a provider's rejection is not recognised, the run fails with the provider's error, which names the real limit.
