---
id: 37
slug: redirect-guard-lenience
title: Redirect fail-closed guard — allow /dev/null targets and fd-dups, keep failing closed on real files
status: in-progress
priority: critical
type: fix
created: 2026-08-06
updated: 2026-08-06
depends_on: []
related: [17, 35]
discovered_from: [17]
adrs: [5]
spec:
plan:
results:
trivial: true
auto_groomable:
branch: feat/redirect-guard-lenience
pr:
claimed_at: 2026-08-06T05:05:35Z
blocked_by:
reconciled: false
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
<!-- docket:artifacts:end -->

## Why

0017's C1 review fix fails closed on ANY `syntax.Stmt.Redirs` entry
(`internal/permissions/shellparse.go`, blanket `len(st.Redirs) > 0` guard).
That closed the out-of-workspace write primitive (`echo x > /etc/passwd`),
but it also prompts on the single most common benign redirect idiom: first
real auto-mode use stalled a long run on
`wc -l internal/**/*.go 2>/dev/null | tail -5` — a wholly read-only pipeline
that only silences stderr. The review had flagged the lenient variant
(fd-dups + /dev/null) as acceptable; strictness was chosen and field use
says it was too strict.

## What changes (design settled, 2026-08-06)

- In `collectStmt`, replace the blanket reject with a per-redirect check.
  A redirect is BENIGN iff:
  - its target word is literally `/dev/null` (any fd, any of `>`, `>>`,
    `2>`, `&>`, `<`), or
  - it is a pure fd-duplication (`2>&1`, `>&2` — `DplOut`/`DplIn` ops with a
    numeric target, no file word).
  Any other redirect — any word naming a real path, variable, or
  substitution — still returns `ErrUnparseable` (fail closed, unchanged).
- The `/dev/null` match is on the literal word only: a target containing
  substitution, a variable, or glob chars is NOT literal and fails closed.
- Corpus rows (TDD red first): `wc -l a.go 2>/dev/null | tail -5` parses and
  classifies read-only; `ls > /dev/null` parses; `make 2>&1 | tail` parses
  (make itself still asks — but the redirect no longer blocks parsing);
  `echo x > /etc/passwd`, `grep a b >> ~/.zshrc`, `cat a > $F`,
  `ls > /dev/null.txt`, `ls 2>/dev/nul` all still ErrUnparseable.
- Regression: the four original C1 rows stay red-path (non-allow).

## Out of scope

- Any other parser lenience (here-docs, input redirects from files, real
  output files in-workspace — all still fail closed; separate discussion).

## Reconcile log
