---
slug: reconcile-verify-claims-against-origin-not-working-tree
hook: "On a just-in-time reconcile, verify the spec's code claims against `origin/<integration_branch>` (the branch the feature branch cuts from), NOT the local working tree — a stale checkout predating a recently-merged dependency will report the dependency's whole infrastructure 'missing' and trigger a false fundamental-invalidation halt. Use `git show origin/<branch>:<path>` / `git ls-tree origin/<branch>`, not on-disk reads."
topics: [reconcile, spec-drift, build-loop, git, verification]
changes: [59]
created: 2026-08-11
updated: 2026-08-11
promotion_state: candidate
promoted_to:
---

## Apply

`docket-implement-next`'s reconcile pass (and any pre-build code-claim verification) must check the
spec's load-bearing code claims against **`origin/<integration_branch>`** — the exact ref the feature
branch is cut from — not the local working tree. The two diverge whenever a **dependency merged
recently**: if the working checkout predates that merge, the dependency's packages, files, and
symbols are simply **absent on disk** even though they exist on the base the build will actually use.

The failure mode is a **false fundamental-invalidation**: a broad code-scan reads the stale tree,
reports the entire dependency infrastructure "missing" (`internal/toolidentity/` gone, functions
undefined, files not found), and the reconcile is tempted to halt as though the spec were void. It
isn't — the base is correct; only the observation was stale.

Fix: verify with git plumbing against the remote base, not the filesystem —
`git show origin/<integration_branch>:<path>`, `git ls-tree -r origin/<integration_branch>`,
`codeindex`/grep scoped to the base. Reserve on-disk reads for the feature worktree once it is cut
(which is itself from `origin/<integration_branch>`, so it is correct). If a scan reports a
dependency "missing," re-confirm against `origin` **before** concluding drift.

## War story
- 2026-08-11 (#59, PR #56) — an early broad code-verify sweep read the stale local working tree (a
  `main` checkout predating the #52/#55 merge) and reported #52's entire identity/credential
  infrastructure absent — `internal/toolidentity/`, `buildToolIdentitySource`, `loopauth.Principal`,
  the per-call delegation seam — as though the spec described a future state. Re-verifying directly
  against `origin/main` (`git show` / `git ls-tree`) showed everything present. The feature branch
  cuts from `origin/main`, so the base was always correct; only the pre-merge working tree lagged.
  Recorded in the change's `## Reconcile log` and results as a build caution. (The same stale-tree
  read tripped a verification subagent in the driving session before the correction.)
