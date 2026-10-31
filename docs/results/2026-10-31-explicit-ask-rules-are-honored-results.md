<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0087 — Explicit ask rules are honored: prompt a human, deny when no human is bound](../changes/active/0087-explicit-ask-rules-are-honored-prompt-a-human-deny-when-no-h.md)**
<!-- docket:backlink:end -->

# Explicit ask rules are honored - results

**Human action:** None required before merge; see Important item for a hosted-behavior check.

## Outcome

An explicit auto-mode ask rule (`auto.ask` or `always_prompt` match) is now its own verdict, `VerdictAskExplicit`. It skips the safelist, heuristics and classifier. With a human approver bound it prompts; with only a policy stand-in (hosted loop servers, `--approve-all`, headless mcp-server) it is denied at layer `rules` with reason `matched ask rule <pattern>; no approver bound`. "No rule matched" is unchanged. Recorded as ADR-0064.

## Human actions and testing

- **Optional:** hosted loops with ask rules now deny those commands instead of allowing them. Prerequisite: a spec with `permissions.mode: auto` and `permissions.auto.ask: ["bash:cat*"]` served by `fuse loop-serve-net`. Step: have the model run `cat x.txt`. Expected: a `permission.decision` event with verdict deny, layer rules, and a tool result naming the pattern.

## Verification performed

- `make test` green at the PR head.
- Review: lean tier, 0 blockers; two minor findings, see below.

## Known issues and follow-ups

- The spec's loop-serve-net fake-gateway test was implemented as a composition-root test (`cmd/fuse/explicit_ask_binding_test.go`), which does not exercise the wire stream. Confirmed gap, low risk. Next action: add a wire-level test if wanted. No existing change fits (checked by judgment; backlog match not run).
- The not-executed assertion in that test is output-text based; the Denied and decision assertions carry the proof. Suggested next action: switch to a sentinel file.
