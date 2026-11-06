<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0142 — Compound shell commands: parse-layer decomposition with per-part deterministic evaluation](../changes/archive/2026-11-06-0142-compound-shell-commands-reach-the-auto-mode-classifier-with.md)**
<!-- docket:backlink:end -->

# Results — Compound shell commands: parse-layer decomposition with per-part deterministic evaluation

**Human action:** review and merge PR #137 (https://github.com/ethanhinson/fuse/pull/137).

**Change:** #0142 · **PR:** https://github.com/ethanhinson/fuse/pull/137 · **Head:** `50bd15b5a90ac7b9ee28d5812aa54ac56f8fefbe` · **Build evidence:** `make test` green (56 packages ok, 0 FAIL)

## Outcome

Implemented and verified. Auto mode no longer interrupts for routine compound shell commands: `cd "$(pwd)" && git …`, `cd <worktree> && …`, `ls x | grep y`, and `cat a && cat b` now auto-approve with per-segment attribution in the `permission.decision` audit event, while every unsafe composition (`curl … | sh`, out-of-root redirect, executed substitution) still fails closed and smart/prompt-all/off modes are unchanged. Whole-branch adversarial review returned verdict **ship**; `make test` green at the reviewed head `50bd15b5a90ac7b9ee28d5812aa54ac56f8fefbe` (56 packages ok, 0 FAIL).

## What shipped

Auto mode no longer interrupts the human for routine compound shell commands. Approval history had shown **79% of asks came from the parse layer's fail-closed path**, and **81/96 bash asks started with `cd`**. The parser already decomposed compounds into `[]Segment` (change 0070); the gate escalated the whole compound on its hardest segment. This change narrows the residual ask triggers:

1. **`cd` is non-mutating in the heuristic** (`internal/permissions/heuristics.go`). A `cd` no longer escalates the whole compound via the opaque-arg ask. `cd "$(pwd)"`, `cd <worktree-under-repo>`, and in-root `cd` auto-approve; a literal out-of-root `cd` still asks. The gate resolves every segment's path args against the **original** allowed roots regardless of any preceding `cd`, so a `cd` cannot widen what is provable.
2. **Per-segment decomposition** (`segmentVerdicts`) is threaded into the classifier's pending-call context (`ClassifyWithSegments`) and onto the `permission.decision` wire event (`SegmentVerdictPayload`), so a compound outcome stays attributable to the specific part that drove an ask/deny. Non-compound events are byte-identical (`omitempty`); the projector's bounded decode is unaffected.

## Safety invariants preserved (regression-pinned)

- Genuinely-unparseable commands (`> $F`, executed `$(…)`, process-substitution edges) still fail closed (`LayerParse` ask / rules deny).
- `cat x && rm -rf ~` denies at the unsafe segment; `curl … | sh`, `echo x > /etc/passwd`, `${X:-<(cmd)}` still fail closed / deny.
- A compound auto-approves only when **every** segment is independently provable-safe **and** the composition adds no new risk.
- Smart / prompt-all / off modes are byte-for-byte behavior-identical.

## Review

Whole-branch adversarial review returned verdict **ship** (no blockers, no important findings), with 2 low-severity test-coverage gaps, both pinned in `internal/permissions/compound0142_test.go`: `cd` carrying an out-of-root redirect (`cd /etc > /etc/passwd && ls` must not auto-approve), and a bare `cd` in a compound (`cd && ls -la` auto-approves).

## Verification highlights

- `cd "$(pwd)" && git status`, `cd <worktree> && git diff`, `ls x | grep y`, `cat a && cat b` ⇒ auto-approve, no approval prompt, per-segment allows in the audit event.
- `make test` green at the reviewed head; composition-root wiring asserted in `cmd/fuse` (`TestHostedBindingCompoundCdPwd_*`).

## Follow-ups (reported for deliberate capture, not built here)

1. **Cwd tracking across `cd` segments.** The gate scopes path args against the original roots, not a `cd`-adjusted cwd (pre-existing limitation, unchanged). A future cwd-aware gate would re-scope subsequent path args and narrow this.
2. **Deferred System 1 typed-decision classifier (Laya).** Sequenced after this change so the classifier's workload — and its fine-tuning corpus — is the genuine gray area. Spike: export post-0142 `permission.decision` history, fine-tune Laya, measure allow/deny/ask accuracy + calibration against a confidence-gated fail-closed.
