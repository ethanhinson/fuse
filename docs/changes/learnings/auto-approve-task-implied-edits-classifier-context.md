---
slug: auto-approve-task-implied-edits-classifier-context
hook: "In an unattended mode there is no human to ask — structurally auto-approve task-implied in-workspace edits behind a trust boundary you already own, and feed the classifier the user's request instead of running it blind."
topics: [permissions, security, agents, ux]
changes: [40]
created: 2026-08-09
updated: 2026-08-09
promotion_state: candidate
promoted_to:
---

## Apply

When building or debugging an autonomous ("auto"/accept-edits) permission mode, do not hand a whole category of task-implied actions to a block-biased LLM classifier — especially one running with zero context. Two moves:

1. **Structurally auto-approve the task-implied, bounded case.** Identify categories of action the task obviously implies (in-workspace `write_file`/`edit_file`) and route them through a hard containment boundary you already trust for other tools — reuse it, don't re-derive it (fuse reused the symlink-aware `withinWorkspace` check already applied to bash mutations). In-bounds auto-approves; an escape (`../`, out-of-root symlink, garbled path) still asks. Put distinct read-only or controlled-egress tools (`segment_read`, `web_search`, orchestration tools whose children are independently re-gated) on the safe-list; keep genuinely arbitrary-egress tools (`web_fetch`) gated on their real risk surface (the host).
2. **Never run the classifier blind.** Plumb the user's request into the classifier (through `context.Context` if a signature change is undesirable), preserving input hygiene — still exclude tool results and actor reasoning. And make only genuine *denies* feed any escalation/backstop counter, never the benign auto-approved path, or a handful of benign asks will trip the valve and stall the run.

In an unattended mode, "ask the human" is not a safe default — there is no human, so an over-eager gate doesn't add safety, it kills the run. Reserve the classifier for genuine gray areas, and give it what a human would see.

Related: [[fail-closed-guard-calibrate-benign-set]] (carve the benign case *within* a syntax class), [[runaway-backstop-follows-interaction-model]] (a backstop tuned for the attended model misfires when the mode changes), [[live-control-reads-state-at-decision-point]].

## War story

- 2026-08-09 (#40, PR #43) — Fuse's `auto` mode stopped far more than Claude Code's accept-edits flow even though the security model was sound. The cause was structural: `write_file`/`edit_file` were not recognized as in-workspace edits, so every write went to a context-blind, block-biased classifier, and a handful of those benign asks tripped the escalation valve and paused auto mode mid-task. 0040 reused the existing `withinWorkspace` containment check to auto-approve in-workspace edits (escapes still ask), plumbed the user's messages into the classifier via `context.Context` (the 0017 D7 intent, previously `nil`), added `segment_read`/`web_search`/`skill`/`pipeline_run` to the safe-list while keeping `web_fetch` host-gated (static SSRF + embedded blocklist floor before a reputation-aware verdict), and retuned the valve so only true classifier denies count toward escalation. Every security layer was preserved; ADR-0005 and ADR-0006 unchanged.
