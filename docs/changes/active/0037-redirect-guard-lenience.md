---
id: 37
slug: redirect-guard-lenience
title: Redirect fail-closed guard — allow /dev/null targets and fd-dups, keep failing closed on real files
status: implemented
priority: critical
type: fix
created: 2026-08-06
updated: 2026-08-06
depends_on: []
related: [17, 35]
discovered_from: [17]
adrs: [5]
spec:
plan: docs/superpowers/plans/0037-redirect-guard-lenience.md
results: docs/results/2026-08-06-redirect-guard-lenience-results.md
trivial: true
auto_groomable:
branch: feat/redirect-guard-lenience
pr: https://github.com/ethanhinson/fuse/pull/17
claimed_at: 2026-08-06T05:13:39Z
blocked_by:
reconciled: true
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Plan | [0037-redirect-guard-lenience.md](https://github.com/ethanhinson/fuse/blob/feat/redirect-guard-lenience/docs/superpowers/plans/0037-redirect-guard-lenience.md) |
| Results | [2026-08-06-redirect-guard-lenience-results.md](https://github.com/ethanhinson/fuse/blob/feat/redirect-guard-lenience/docs/results/2026-08-06-redirect-guard-lenience-results.md) |
| PR | [#17](https://github.com/ethanhinson/fuse/pull/17) |
| ADRs | [ADR-0005](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0005-per-segment-allow-rule-evaluation.md) |
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

### 2026-08-06

Reconciled against current `origin/main`. Ground truth confirmed:

- `internal/permissions/shellparse.go` `collectStmt` still carries the blanket
  guard `if len(st.Redirs) > 0 { return ErrUnparseable }` (lines 111-113) — the
  exact target of this change. No prior work has softened it.
- Vendored `mvdan.cc/sh/v3 v3.13.1` `syntax.Redirect{OpPos, Op, N, Word, Hdoc}`;
  `Op` is a `RedirOperator` with constants: `RdrOut` (`>`), `AppOut` (`>>`),
  `RdrIn` (`<`), `RdrInOut` (`<>`), `DplIn` (`<&`), `DplOut` (`>&`),
  `Hdoc`/`DashHdoc`/`WordHdoc`, `RdrAll` (`&>`), `AppAll` (`&>>`). `2>` parses as
  `Op=RdrOut, N="2"`; `2>&1` as `Op=DplOut, N="2", Word="1"`; `>&2` as
  `Op=DplOut, Word="2"`. Benign set = literal `/dev/null` target under
  `RdrOut`/`AppOut`/`RdrIn`/`RdrAll` (any `N`), plus pure fd-dup
  `DplIn`/`DplOut` whose `Word` is numeric with no path. Here-docs and every
  other op stay fail-closed.
- Existing `TestSplitSegments_FailClosed` rows already assert the regression
  contract this change must preserve: `echo x > /etc/passwd`,
  `grep foo bar >> ~/.zshrc`, `ls > out.txt`, and `ls 2>/dev/null > /etc/x` all
  stay `ErrUnparseable` under the new per-redirect check (the last mixes a
  benign `2>/dev/null` with a real-file `> /etc/x`, which still fails closed).
- Scope is `shellparse.go` + `shellparse_test.go` only. In-flight PR #16
  (change 35, live-mode-switch) touches `gate.go`/`run.go` — no overlap, no
  conflict. Feature branch cuts from `origin/main`.

Design in `## What changes` holds as written; no scope change. Proceeding to
plan + TDD build.
