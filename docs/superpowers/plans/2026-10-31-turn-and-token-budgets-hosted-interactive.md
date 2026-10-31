<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0090 — Turn and token budgets are enforced in hosted interactive loops](../../changes/active/0090-turn-and-token-budgets-are-enforced-in-hosted-interactive-lo.md)**
<!-- docket:backlink:end -->

# Turn and token budgets in hosted interactive loops - Plan

Note: authored inline by the implementer (plan-writer dispatch not used in this run).

1. Agent: add SetUsageSink and SetMaxTurnsExplicit; loop calls the sink after each model call.
2. Loop: in interactive mode treat maxTurns as a per-exchange cap; on the cap emit loop.parked reason max_turns and park; reset on the next message.
3. Event: add Reason to LoopParkedPayload.
4. Runtime inproc: keep an explicit cap for interactive loops, drop only a binding backstop.
5. Bindings: wire the sink to the tree node at every SetNodeIdentity site; NodeRenderer.Tokens stops counting.
6. Tests: per-exchange cap, non-interactive ErrMaxTurns unchanged, TUI counts once.
7. ADR for per-exchange max_turns.
