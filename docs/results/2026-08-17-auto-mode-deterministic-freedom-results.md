# 0068 — Auto-mode deterministic freedom — results

Change: `docs/changes/active/0068-auto-mode-deterministic-freedom.md` (docket branch) · Spec: `docs/superpowers/specs/2026-08-17-auto-mode-deterministic-freedom-design.md` · Plan: `docs/superpowers/plans/2026-08-17-auto-mode-deterministic-freedom-plan.md`

## What landed

- **Config** (`internal/config`): `permissions.auto.allow_push` and `permissions.auto.write_roots`, trusted-source-only; the loader's trusted-branch presence predicate extended so a home file setting only the new keys still lands (tested first, per the spec gotcha).
- **Rules shrink** (`internal/permissions/rules.go`): `dangerousNames` → `mkfs`(+`mkfs.*`), `shutdown`, `reboot`, `halt`, `poweroff`, `sudoedit`. New shape-based catastrophic denies: `rm` with recursive/force flags resolving to `/`, `$HOME`, or a workspace ancestor; `dd of=/dev/*`. `git reset`/`clean` freed (workspace-local); `git push` allows iff `allow_push` (config deny/`always_prompt` still win), else routes as unconditional egress → classifier.
- **Heuristics** (`internal/permissions/heuristics.go`): root set widens to workspace + write roots (`withinAnyRoot`); `kill` with only signal flags + numeric PIDs (≠1) deterministically allows while `pkill`/`killall` route to the classifier (their operands are names, not paths — path-scoping them would wrongly prove containment); `curl`/`wget` with provably-loopback URLs, known-benign flags (grouped shorts handled), and in-root outputs deterministically allow; `dd`'s `of=`/`if=` values extracted for scoping.
- **Tilde bypass fix**: a literal `~`/`~/x` (the parser never expands tilde, bash does at execution) now expands against the real home before containment checks — previously `touch ~/x` path-resolved against the cwd and wrongly proved in-workspace. Regression-tested.
- **Scratchpad** (`cmd/fuse/scratch.go`): per-session canonicalized `~/.fuse/tmp/session-*/` created lazily, swept after 7 days, included as the first write root on every binding (`buildGate`), and advertised in the system prompt at the single `buildAgentCore` chokepoint.

## Verification

- `make test` green across all 38 packages. New coverage: 27-row `TestFreedom_Bash` behavior table (loopback curl allows / public curl classifies / unknown-flag & out-of-root-output unprovable; kill/pkill split; rm scoping + catastrophic floor + tilde regression; dd shapes; git reset/clean/push), write-roots + symlink-escape + child-inheritance, allow_push end-to-end, loader trust tests, scratch creation/sweep/advertisement.
- **Live one-shot runs** (deepseek-flash, auto mode):
  - `curl -s localhost:4000/v1/models | head -c 120` — executed with **zero prompts** (the #1 observed real denial class, 39/62 rules denies in baseline logs).
  - `kill 99999` — policy-allowed deterministically (failed only on the nonexistent PID).
  - `rm -rf /` — terminal rules-layer deny with the catastrophic hint.
  - Scratchpad: the model read the advertised path from its system prompt, wrote `probe.txt` there and read it back, all auto-approved.

## Deviations from spec

- `kill`-family handling lives in the heuristic layer, not a rules-layer helper — same verdicts, but it also closes an over-allow the spec missed: once `pkill` left the deny list, its name operand would have path-scoped against the cwd and silently allowed.
- The scratch dir is `~/.fuse/tmp/session-<random>/` on every binding (lazy `sync.Once` + `os.MkdirTemp`) rather than keyed by `tree.RootID()` in the shell — functionally equivalent (GC by age, advertised in-prompt), avoids threading a session id through 8 gate-construction call sites.
- Added the tilde-expansion fix (not in the spec; found while rewriting the `rm -rf ~` test row — a pre-existing containment bypass).

## Notes for the stack

- `permission.decision` events (#0067) now attribute the new allows to `rules`/`heuristic`/`edit_scope`; the #0069 classifier retune is the next deny-volume lever (web_fetch + gray-area bash prompts).
- The loopback-curl allow requires non-opaque URLs by construction today; #0070's opaque-args work must keep it that way (spec already notes the invariant).
