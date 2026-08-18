---
slug: shared-worktree-red-suite-verify-detached
hook: "A red (or green) suite run in a SHARED feature worktree may reflect another writer's uncommitted files, not any committed state — during 0071 two full-suite runs came back red solely because a concurrent worker's uncommitted RED tests sat in the tree. Before acting on a suite verdict from a worktree other agents may touch, re-verify the branch TIP in a clean detached worktree (git worktree add --detach <tmp> <sha>); trust only verdicts bound to a sha, never to a directory."
topics: [build-loop, worktree, concurrency, test-evidence, dispatch]
changes: [71]
created: 2026-08-18
updated: 2026-08-18
promotion_state: candidate
promoted_to:
---

## Apply

Suite verdicts are only meaningful when bound to a commit sha. A worktree that more than
one agent can write (duplicate dispatches, a shadow run, a human poking around) can hold
uncommitted files that flip the suite either direction: uncommitted red tests fail a tree
whose committed state is green; uncommitted fixes pass a tree whose committed state is
broken. The failure smells like a real regression and burns fix-loop budget on phantom
work.

Rules: when a suite result disagrees with expectations — or before recording build
evidence — check `git status --porcelain` in the worktree first; if it is not clean, or
other writers are plausible, re-run at the tip in a disposable detached worktree and
record THAT verdict with its sha. Build-evidence blocks must name the sha they certify
(they do), and the certifying run must have executed against exactly that sha.

## War story

- 2026-08-18 (#71, PR #74): a shadow implementer instance and duplicate task workers
  repeatedly shared `.worktrees/turn-scoped-trace-roots-interactive-loops`. Two
  full-suite runs were red only from an in-flight worker's uncommitted RED tests; the
  controller nearly halted on a "live writer mid-flight" condition. Resolution: verify
  tip `99e80b9` in a clean detached worktree (`make test` exit 0) and re-record the
  evidence block against that sha. Branch history was coherent throughout — only the
  working tree lied.
