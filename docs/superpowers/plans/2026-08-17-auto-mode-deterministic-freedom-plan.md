# 0068 — Auto-mode deterministic freedom — plan

Spec: `docs/superpowers/specs/2026-08-17-auto-mode-deterministic-freedom-design.md` (docket branch). Problem statement, real-denial data, and design rationale live there; this is the task order.

## Tasks

1. **Config keys** — `internal/config/schema.go` AutoConfig: `allow_push bool`, `write_roots []string`. `internal/config/loader.go`: extend the trusted-branch presence predicate (the D1 gotcha — a trusted file setting ONLY a new key must not be dropped); both keys trusted-source-only, named in the `.fuse.local.yml` ignore warning. Loader tests first.
2. **Write-roots plumbing** — `internal/permissions`: `WithWriteRoots` gate option (canonicalized roots), `withinAnyRoot` looping the existing `withinWorkspace`; `classifyHeuristic` + edit-tool branch take the root set; `CloneForChild` propagates. Heuristics + gate tests.
3. **Scratchpad** — cmd/fuse: per-session `~/.fuse/tmp/<session-id>/` (shell: `tree.RootID()`; one-shot: `os.MkdirTemp`), `EvalSymlinks`-canonicalized, GC-swept like sessions; threaded into `buildGate`/`autoModeOptions` and appended to the system-prompt `extra` block ("use it instead of /tmp"). `/tmp` itself NOT a default root.
4. **Rules-layer shrink** — `internal/permissions/rules.go`: `dangerousNames` → {mkfs(+prefix), shutdown, reboot, halt, poweroff, sudoedit}; add `isCatastrophicRm` (recursive+force rm resolving to /, $HOME, or a workspace ancestor) and `isDdToDevice` (`of=/dev/*`); other `dd of=` targets extracted for scoping. `kill` with only signal flags + numeric PIDs (≠1) → deterministic allow; `pkill`/`killall` → classifier.
5. **Loopback curl/wget** — heuristics: pre-egress check; all URL operands provably loopback (shared check extracted from fetchhost) AND any `-o/--output/-O` target in-roots → allow; else egress ask → classifier (no longer hard deny).
6. **git** — drop `reset`/`clean` from `dangerousGitSubcommands`; `push` → allow iff `cfg.AllowPush` (after deny/ask patterns), else unconditional-egress routing (ask → classifier) so cwd-resolving operands can't heuristic-allow it.
7. **Tests** — rewrite affected `TestEvalRules_Precedence` rows; new rows per spec (`rm -rf /` deny, `rm -rf ./build` allow, symlink-escape rm, `dd of=/dev/disk0` deny, `kill 1234` allow, `kill 1` not-allow, `pkill node` → classifier, `curl localhost:3000/health` allow, `curl https://example.com` → classifier); scratch-root heuristics + edit-tool cases; loader trust tests. Full `make test`.

## Verification

`make test`; live one-shot (cheap gateway model) replaying the observed failure shapes: `curl -s localhost:4000/v1/models` (deterministic allow, no prompt), `mkdir` + `write_file` into the advertised scratch dir (allow), `kill <started pid>` (allow), `rm -rf /` (deny), `git push` without config (no terminal deny; classifier/ask routing). Confirm `permission.decision` events attribute the new allows to rules/heuristic layers.
