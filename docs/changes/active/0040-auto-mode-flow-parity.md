---
id: 40
slug: auto-mode-flow-parity
title: Auto-mode flow parity — in-workspace edits auto-approve
status: proposed
priority: high
type: fix
created: 2026-08-09
updated: 2026-08-09
depends_on: []
related: [17]
discovered_from: []
adrs: [5, 6]
spec: docs/superpowers/specs/2026-08-09-auto-mode-flow-parity.md
plan:
results:
trivial: false
auto_groomable:
branch:
pr:
blocked_by:
reconciled: false
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-08-09-auto-mode-flow-parity.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/2026-08-09-auto-mode-flow-parity.md) |
| ADRs | [ADR-0005](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0005-per-segment-allow-rule-evaluation.md), [ADR-0006](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0006-fuse-local-yml-tighten-only-trust-boundary.md) |
<!-- docket:artifacts:end -->

## Why

Fuse's `auto` permission mode stops far more often than Claude Code's accept-edits/auto
flow, even though the security model is sound (and in places stronger). The stopping is
structural, not excess caution: `write_file`/`edit_file` are not recognized as in-workspace
edits, so every file write is handed to a block-biased LLM classifier that runs with zero
context — and a handful of those benign asks then trips the escalation valve, pausing auto
mode mid-task. The result is an autonomous mode that constantly interrupts routine, task-
implied edits.

The fix keeps every security layer. It reuses machinery the pipeline already has (the
symlink-aware `withinWorkspace` containment check, already applied to bash mutations) to
auto-approve in-workspace edits, and finally plumbs the user's messages into the classifier
so its gray-area verdicts are informed rather than blind. Full findings with file:line
citations are in `reports/2026-08-09-auto-mode-flow-parity.md` (integration branch).

Follow-up to change 0017 (auto-mode). Preserves ADR-0005 (per-segment allow evaluation) and
ADR-0006 (`.fuse.local.yml` tighten-only trust boundary) unchanged.

## What changes

- **Path-scope the edit tools** — route `write_file`/`edit_file` through the existing
  workspace-containment heuristic: an in-workspace path auto-approves, an escape (`../`,
  out-of-root symlink, garbled `path`) still asks the human.
- **Extend the safe-list** — add `segment_read` (read-only), `web_search` (a controlled query
  to a known, configured engine — no arbitrary egress), and the orchestration tools (`skill`,
  `pipeline_run`, whose children are independently re-gated).
- **Keep `web_fetch` gated on the domain** — it pulls a model-chosen arbitrary URL, so the host
  is the risk surface (malware, drive-by, prompt-injection, SSRF). A static host-deny floor
  (malware / IP-literal / loopback / RFC-1918 private ranges) runs before a domain-reputation-
  aware classifier verdict; new `permissions.auto.fetch_deny` / `fetch_ask` host globs let a
  user tighten it further.
- **Give the classifier context** — plumb the user's messages into the classifier (the
  0017 D7 intent, currently `nil`) via the `context.Context`, preserving input hygiene
  (tool results / actor reasoning still excluded).
- **Accept-edits posture + valve retune** — in-workspace edits never feed the escalation
  valve; only genuine classifier denies count toward it.

## Out of scope

- OS-level sandboxing (Seatbelt/Landlock/bubblewrap).
- Two-stage classifier CoT (a noted future upgrade from 0017).
- Any change to bash segment evaluation, the (bash) egress boundary, or the dangerous-command list.
- A live domain-reputation API for `web_fetch` (v1 uses a built-in host floor + the classifier's
  own knowledge; a network lookup is a future upgrade).

## Open questions

- Final `valveTotalLimit` value once only true gray-area denies count (defer to measurement
  after the core fix lands — see spec D4).
- The exact contents of the built-in `web_fetch` host-deny floor (which malware/parked-domain
  hosts to seed beyond the IP-literal / loopback / RFC-1918 SSRF entries — settle at build).

## Reconcile log

<!-- Appended by docket-implement-next's reconcile pass: dated entries of what changed. -->
