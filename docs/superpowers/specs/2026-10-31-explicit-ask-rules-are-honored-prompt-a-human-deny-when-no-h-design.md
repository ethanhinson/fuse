# Explicit ask rules are honored

Change 0087 — Explicit ask rules are honored: prompt a human, deny when no human is bound

## Problem

`internal/permissions/rules.go` `evalRules` returns `VerdictAsk` both for an explicit ask or always_prompt match (around lines 98-108) and for "no rule matched" (around line 118). `gate.go` (around 678-684) only acts on Deny and Allow, so an explicit ask falls through to the safelist (around 687), where a read-only command such as `cat` is allowed. Separately, every hosted loop binds `permissions.AlwaysApprove` (`loop_server.go` around 55, `gate.go` around 41): even a correctly surfaced ask would be routed to the approver (`gate.go` around 578-583) and auto-approved. There is no approval protocol on the loop wire.

## Design

- Add a distinct verdict, `VerdictAskExplicit`, returned by `evalRules` for an explicit ask rule or always_prompt match, before the safelist step. `resolveAuto` returns `(VerdictAskExplicit, LayerRules, reason)` where the reason names the matched pattern.
- `resolve` branches on the gate's approval provenance:
  - human approver: prompt exactly as an ask does today.
  - policy stand-in (`DecidedByPolicy`: loop server, `--approve-all`, headless mcp-server): deny, layer `rules`, reason `matched ask rule <pattern>; no approver bound`. Precedent: the piped-stdin deny fallback in `cmd/fuse/approval.go`.
- "No rule matched" is unchanged (still `VerdictAsk`, still falls through, still policy-allowed when headless).
- The `permission.decision` event carries the verdict, layer and reason; the tool result returned to the model carries the reason so the model can adapt.
- Record an ADR: an explicit ask with no human approver bound is a deny, never an implicit allow.

## Testing

- Table test: `bash:cat*` in auto.ask plus `cat x.txt` gives explicit ask, beating the safelist; same for always_prompt.
- Gate test with a human approver: prompts.
- Gate test with AlwaysApprove and policy provenance: deny event with `layer=rules` and the pattern in the reason; the tool did not run.
- loop-serve-net integration test: spec `permissions.mode: auto`, `permissions.auto.ask: ["bash:cat*"]`; a scripted fake gateway emits a `bash` call `cat missing.txt`; assert `permission.decision{verdict: deny, layer: rules}` on the stream and that the command was not executed.
- Regression: a command with no matching rule still follows the old path (policy allow when headless).

## Compatibility

Hosted loops with ask rules set will start denying those commands instead of allowing them. Call this out in the PR.
