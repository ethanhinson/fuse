<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0087 — Explicit ask rules are honored: prompt a human, deny when no human is bound](../../changes/archive/2026-10-31-0087-explicit-ask-rules-are-honored-prompt-a-human-deny-when-no-h.md)**
<!-- docket:backlink:end -->

# Explicit ask rules are honored - Implementation Plan

Spec: docs/superpowers/specs/2026-10-31-explicit-ask-rules-are-honored-prompt-a-human-deny-when-no-h-design.md

- [x] Task 1: add VerdictAskExplicit and evalRulesDetail (returns matched pattern) in internal/permissions/rules.go; update precedence tests.
- [x] Task 2: branch resolveAuto on approval provenance (human prompts, policy stand-in denies at layer rules naming the pattern) in internal/permissions/gate.go; gate tests for human, policy, always_prompt, and no-rule-matched regression.
- [x] Task 3: composition-root test (cmd/fuse) proving the hosted binding denies an explicit ask and does not run the command.
- [ ] Task 4: record ADR, results, PR.
