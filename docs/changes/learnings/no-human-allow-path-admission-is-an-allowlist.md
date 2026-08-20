---
slug: no-human-allow-path-admission-is-an-allowlist
hook: "An admission set on a no-human deterministic-allow path must be an allowlist — a denylist's failure mode is a silent permission bypass nobody sees, and enumeration is exactly how it rots."
topics: [security, permissions, allowlist, shell]
changes: [70]
created: 2026-08-20
updated: 2026-08-20
promotion_state: candidate
promoted_to:
---

## Apply

Ask of any admission set (env-var names, hosts, commands, flags) gating an automatic allow: **when
this set is wrong, who finds out?** An allowlist that is wrong costs one human prompt — the human
notices and the entry gets added. A denylist that is wrong auto-approves the attack silently — with
no human on the path, nobody notices until an exploit. So on a deterministic-allow path with no
human in the loop, the set must be an allowlist of names *proven inert*; a denylist is legitimate
only where the fail direction points toward the human (deny lists that tighten, or ask-by-default
paths where a miss still prompts).

The tell that a denylist is rotting: each review round finds another family member to enumerate.
The second miss is the proof — stop extending the list and invert the mechanism. Over-denial on the
inverted set is cheap and self-correcting; keep a pinned test of the admitted set so a later
"helpful" addition fails the suite instead of shipping silently.

Related: [[canonicalize-once-before-every-matching-layer]] (re-audit matchers when a hint becomes an
authorization), [[fail-closed-guard-calibrate-benign-set]] (carve the benign 80% into the allowlist
so the prompt cost stays tolerable).

## War story

- 2026-08-19 (#70, PR #76) — The shell-parse widening's D1 task shipped a dangerous-env-name
  denylist (`LD_*`, `PATH`, `GIT_SSH_COMMAND`, …). The build worker already had to extend it live —
  the plan's list missed git's exec-hook family (`GIT_PAGER`, `GIT_EXTERNAL_DIFF`, `PAGER`,
  `GIT_CONFIG_*`), each of which auto-approved an arbitrary exec via safelisted `git log`/`git
  diff`. The deep review then found the *same* rot one family over: the toolchain exec hooks
  (`CC=/tmp/evil make`, `GOFLAGS=-toolexec=/tmp/evil go build`) — commands that reach the heuristic
  allow with no human, executing an arbitrary out-of-tree binary. Two enumeration misses in one
  change was the proof; the fix inverted to an allowlist of provably inert names (`TERM`, `TZ`,
  `LANG`, `LC_*`, `NO_COLOR`, `CGO_ENABLED`, …) with everything unrecognized costing one prompt.
  Recorded architecturally as ADR-0049.
