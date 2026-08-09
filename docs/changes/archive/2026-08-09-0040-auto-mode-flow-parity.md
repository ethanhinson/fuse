---
id: 40
slug: auto-mode-flow-parity
title: Auto-mode flow parity — in-workspace edits auto-approve
status: done
priority: high
type: fix
created: 2026-08-09
updated: 2026-08-09
depends_on: []
related: [17]
discovered_from: []
adrs: [5, 6]
spec: docs/superpowers/specs/2026-08-09-auto-mode-flow-parity.md
plan: docs/superpowers/plans/2026-08-09-auto-mode-flow-parity-plan.md
results:
trivial: false
auto_groomable:
branch: feat/auto-mode-flow-parity
pr: https://github.com/ethanhinson/fuse/pull/43
blocked_by:
claimed_at: 
reconciled: true
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-08-09-auto-mode-flow-parity.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/2026-08-09-auto-mode-flow-parity.md) |
| Plan | [2026-08-09-auto-mode-flow-parity-plan.md](https://github.com/ethanhinson/fuse/blob/feat/auto-mode-flow-parity/docs/superpowers/plans/2026-08-09-auto-mode-flow-parity-plan.md) |
| PR | [#43](https://github.com/ethanhinson/fuse/pull/43) |
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
  (hardcoded SSRF guard + an **embedded, license-clean malware/phishing blocklist**) runs before
  a domain-reputation-aware classifier verdict; new `permissions.auto.fetch_deny` / `fetch_ask`
  host globs let a user tighten it further. The spec (D2a) names the concrete, license-verified
  data sources — StevenBlack/hosts (MIT) + optionally hagezi TIF (GPL-3.0) for the blocklist,
  Majestic Million (CC BY 3.0) for the known-good popularity nudge — all embeddable; restrictive-
  ToU sources (URLhaus/OpenPhish/PhishTank/Spamhaus) are runtime-query-only and never bundled.
  No live reputation API is included: the only free option (Google Safe Browsing) is
  non-commercial-only and would make fuse un-embeddable commercially, so it's excluded outright
  rather than left as a seam — the embedded blocklist + classifier are the signal.
- **Give the classifier context** — plumb the user's messages into the classifier (the
  0017 D7 intent, currently `nil`) via the `context.Context`, preserving input hygiene
  (tool results / actor reasoning still excluded).
- **Accept-edits posture + valve retune** — in-workspace edits never feed the escalation
  valve; only genuine classifier denies count toward it.

## Out of scope

- OS-level sandboxing (Seatbelt/Landlock/bubblewrap).
- Two-stage classifier CoT (a noted future upgrade from 0017).
- Any change to bash segment evaluation, the (bash) egress boundary, or the dangerous-command list.
- Any live domain-reputation API (deliberately excluded — the only free option, Google Safe
  Browsing, is non-commercial-only; a paid provider like Google Web Risk is its own future
  change if ever wanted — see spec D2a).

## Open questions

- Final `valveTotalLimit` value once only true gray-area denies count (defer to measurement
  after the core fix lands — see spec D4).
- Whether to keep hagezi TIF (GPL-3.0) in the embedded blocklist or stay MIT-only with
  StevenBlack + Peter Lowe's list, to avoid shipping GPL data (spec D2a lays out both; decide
  at build based on the project's licensing preference).

## Reconcile log

<!-- Appended by docket-implement-next's reconcile pass: dated entries of what changed. -->

### 2026-08-09 — reconcile (docket-implement-next)

Verified the spec's file:line citations against current `main`-tree code (via a read-only
exploration pass). Result: **spec valid, no scope change, no obsolescence.**

- **Citations confirmed (minor line drift, none material):**
  - `internal/permissions/policy.go` safe-list (`safeList` / `onSafeList`) at ~71-98; current
    entries `read_file`, `list_directory`, `grep`, `spawn_agent`, `ask_user` plus dynamic
    `codeindex_*` / `blackboard_*` prefixes. The seven tools this change touches
    (`write_file`, `edit_file`, `segment_read`, `web_search`, `web_fetch`, `skill`,
    `pipeline_run`) are confirmed **absent** — the fix's premise holds.
  - `internal/permissions/gate.go`: `resolveAuto` non-bash branch (~404-410) → `classifyOrAsk`
    (~457-484) calls `Classify(ctx, nil, name, command)` at ~469 — the D3 `nil` history is real;
    `workspaceRoot` field at ~72 wired via `run.go:425`; valve `valveConsecutiveLimit=3` /
    `valveTotalLimit=20` (~100-101), `recordBlock` (~104-112).
  - `internal/permissions/heuristics.go`: `withinWorkspace` (~140-158) + `resolveExisting`
    (~160-183), symlink-aware, deepest-existing-ancestor resolution — the D1 machinery exists.
  - `internal/permissions/classifier.go`: `Classify(ctx, userMessages, toolName, command)` at
    ~163 — **`userMessages` is already a parameter**, so D3 is a plumb-through, not a signature
    change; `buildMessages` (~194-199) already drops non-user roles (hygiene intact).
  - `internal/agent/loop.go`: `executeToolBounded` (~605) → `a.tools.Execute` at 607/618.
  - `internal/agent/agent.go`: `ToolExecutor` interface (~17-19) — D3's ctx-carry avoids
    widening it.
  - `internal/config/schema.go`: `AutoConfig` has `ClassifierModel`, `Deny`, `Ask`; **no**
    `fetch_deny`/`fetch_ask` yet — this change adds them (tightening keys, per ADR-0006).
  - Tool arg shapes: `write_file`/`edit_file` expose a `path` arg; all seven tool names are
    registered.
  - Report `reports/2026-08-09-auto-mode-flow-parity.md` exists on the integration branch.
  - Test scaffolding present: `heuristics_test.go`, `gate_test.go`, `safelist_test.go`,
    `classifier_test.go`, `valve_test.go` all exist.
- **New files to create (correctly absent today):** `internal/permissions/fetchhost.go`,
  `internal/permissions/reputation/`.
- **ADR-0005 / ADR-0006 unchanged and honored** — no relaxation of per-segment bash evaluation;
  `fetch_deny`/`fetch_ask` classified as *tightening* keys (ADR-0006's fail-safe rule for new
  permission keys), so they may merge from `.fuse.local.yml`.
- **Open question resolved (licensing):** default to **MIT-only** for the embedded blocklist —
  StevenBlack/hosts (MIT) + Peter Lowe's list (explicit redistribution permission), **dropping
  hagezi TIF (GPL-3.0)** so fuse ships no GPL data. Keeps the whole embed license-clean for a
  commercial build; Majestic Million (CC BY 3.0) stays for the popularity nudge with its credit
  line. The plan may add TIF later behind a build tag if wanted; the SSRF guard + config
  `fetch_deny` + classifier remain independent layers, so a slightly narrower list is acceptable.
- **Open question deferred (as designed):** `valveTotalLimit` retune stays deferred to
  post-landing measurement (spec D4) — not a build blocker.

Related change 0017 (auto-mode) is already `done`; no in-flight work overlaps this change.
