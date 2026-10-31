# Turn and token budgets in hosted interactive loops

Change 0090 — Turn and token budgets are enforced in hosted interactive loops

## Problem

- `max_turns`: non-interactive hosted loops already enforce it (`buildAgentCore` in `run.go` around 809, through `resolveMaxTurns`). Interactive loops call `SetMaxTurns(0)` (`internal/runtime/inproc.go` around 727-730), so the spec value is dropped. (`cmd/fuse/orchestrator_shell.go` around 80 is the local TUI orchestrator; leave it alone.)
- Tokens: the only usage path is `loop.go` around 582, through `Renderer.Tokens`. Hosted agents and children use `discardRenderer` (`loop_server.go` around 43, 408-410 and 453). Only `internal/tui/renderer.go` around 195 calls `node.UpdateTokens` (`tree.go` around 158). The scheduler reads `tree.SessionTokens()` (`scheduler.go` around 205, 232 and 536), so `session_tokens` and `pool.tokens` never trip in loop-server, loop-serve-net, one-shot or mcp-server.

## Design

### Per-exchange max_turns

- Interactive loops stay uncapped by default. When the spec (or operator config) sets `max_turns`, it caps model turns per exchange: the turns that run in answer to one user message.
- Add an `exchangeTurns` counter in the agent loop, reset where the injector wakes the loop for a new message (`loop.go` around 619-623). The whole-run `turn` counter stays.
- On the cap in interactive mode, emit `loop.parked` with `Reason: "max_turns"` and park. Do not return `ErrMaxTurns`, which would end the session. The next Send gets a fresh allowance.
- Record an ADR.

### Usage accounting owned by the agent

- Add `Agent.SetUsageSink(func(in, out int))`, called wherever usage arrives from the model client.
- Every binding that holds both the agent and its tree node wires the sink to `node.UpdateTokens`: the loop_server BuildAgent and childBuilder, runtime_binding, research_probe and the shell.
- `NodeRenderer.Tokens` stops calling `UpdateTokens`, so the TUI does not double-count; the TUI keeps reading the tree.

## Testing

- Hosted interactive loop, spec `max_turns: 2`, a fake gateway that always calls a tool: after 2 model calls, `loop.parked{reason: max_turns}`; a second Send gets 2 fresh turns.
- Non-interactive loop: unchanged (`ErrMaxTurns`).
- Tokens: the fake gateway reports usage 600 in and 400 out, and the operator sets `throughput.session_tokens: 1500`. The second model call is refused with the session-ceiling error, and `tree.SessionTokens() == 1000` after the first.
- TUI renderer test: usage is counted once.

## Compatibility

Operators with `throughput.session_tokens` set will see hosted loops trip for the first time. Call this out in the PR.
