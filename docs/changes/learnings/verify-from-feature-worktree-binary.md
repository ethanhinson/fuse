---
name: verify-from-feature-worktree-binary
slug: verify-from-feature-worktree-binary
title: A feature "not working" during a worktree build is a stale-binary suspect first
hook: "make build/install act on whatever checkout is the CWD — when a human reports a worktree-built feature missing or broken, check which binary they ran (main checkout vs .worktrees/<slug>) before hunting a code defect"
promotion_state: retained
changes: [13]
created: 2026-08-05
updated: 2026-08-05
topics: [workflow, verification, worktrees, go]
---

## Apply

Docket builds every change in `.worktrees/<slug>`, but `make build` / `make install` (`go build` / `go install ./cmd/fuse`) operate on the current working directory's checkout. A human verifying a feature usually rebuilds from the primary checkout on `main` — producing a binary without the branch's changes. Before triaging a "feature doesn't work" report against feature-branch code, confirm the binary's provenance: reproduce with a binary built from the worktree (`cd .worktrees/<slug> && go build -o fuse ./cmd/fuse`), and diff-check whether the symptom even exists on the branch (`git show main:<file>` vs the worktree file). Only a symptom reproduced from the worktree binary is a code defect.

## War story

- 2026-08-05 (#13, PR #11) — User reported `fuse help` erroring with `model "": unknown model ""` and `fuse shell` showing no banner while the startup-banner branch sat unmerged in its worktree. Triage nearly became a fabricated fix; the real cause was a binary rebuilt from the `main` checkout, where the dispatch had no `help` case so `help` fell through to the task-run path. The worktree binary was verified correct end-to-end; the only action needed was rebuilding from `.worktrees/startup-banner`.
