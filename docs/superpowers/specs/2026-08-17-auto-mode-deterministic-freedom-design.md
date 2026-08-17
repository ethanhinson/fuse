<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0068 — Auto-mode deterministic freedom — scratchpad + write_roots + rules-layer shrink to catastrophic-only](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0068-auto-mode-deterministic-freedom.md)**
<!-- docket:backlink:end -->

# Auto-mode deterministic freedom — config, scratchpad, rules-layer shrink — design

**Change:** #0068 · Stage B of the auto-mode overhaul (arc: #0067 → #0068 → #0069 → #0070)

## Problem

The rules layer terminal-denies routine dev operations. Real session data: `curl` was 39 of 62 rules-layer denies (14+ targeting localhost — agents probing their own dev servers/gateways), `kill`/`pkill` another 4; `/tmp` usage drove most of the 19 bash classifier denies. Product bar (settled 2026-08-17): Claude Code / Cursor parity — routine dev ops are never hard-blocked. Terminal denies shrink to catastrophic-only; everything else: deterministic scoped-allow where provable → context-aware classifier → ask. SSRF floor, workspace scoping, config deny/`always_prompt`, and fail-closed-for-unprovable all stay.

## Design

### D1. Config keys (foundation)

`AutoConfig` (`internal/config/schema.go:45-57`) gains:
- `allow_push bool` — opt-in deterministic allow for `git push`.
- `write_roots []string` — extra scoped-mutation roots (e.g. `/tmp`).

Both are LOOSENING → trusted-source-only (home config; never `.fuse.local.yml`). **Gotcha:** the trusted-branch presence predicate at `loader.go:129` must be extended (`|| raw.Auto.AllowPush || len(raw.Auto.WriteRoots) > 0`) or a trusted file setting only a new key is silently dropped. Write that test first (pattern: `loader_test.go:362-413`; aggregate warn-name stays `permissions.auto`).

### D2. Per-session scratchpad + write-roots plumbing

- Scratch dir `~/.fuse/tmp/<session-id>/` created in cmd/fuse — interactive shell uses `tree.RootID()` (available shell.go:154 before the build closure); one-shot/mcp paths use `os.MkdirTemp` under `~/.fuse/tmp/`. **Canonicalize via `filepath.EvalSymlinks`** (macOS `/tmp`→`/private/tmp`; uncanonicalized roots break containment). GC older than 7 days via the existing `session.SweepOld` pattern (shell.go:147). `/tmp` itself is NOT a default root — opt-in via `write_roots`.
- Gate: new option `WithWriteRoots(roots []string)` (pre-canonicalized, beside `WithWorkspaceRoot` gate.go:170-175); propagated in `CloneForChild` (gate.go:642-677).
- Heuristics: `withinAnyRoot(arg, roots)` looping the existing `withinWorkspace`/`resolveExisting`/`isWithin` (heuristics.go:147-201, reused unchanged). `classifyHeuristic` takes the root set (`append([]string{workspaceRoot}, writeRoots...)` at gate.go:486); the edit-tool branch (gate.go:440) switches too, so `write_file` into scratch auto-approves.
- Wiring: `autoModeOptions`/`buildGate` (run.go:387-442) gain the scratch param; 4 buildGate callers updated.
- System prompt advertisement appended to the `extra` block (shell.go:69 skillBlock; mirror at the one-shot site): "Scratch directory: <dir> — use it for temporary files instead of /tmp."

### D3. Rules-layer shrink (`internal/permissions/rules.go`)

- `dangerousNames` (rules.go:48-69) shrinks to: `mkfs` (+ `mkfs.*` prefix), `shutdown`, `reboot`, `halt`, `poweroff`, `sudoedit`. Removed (`rm`, `chmod`, `chown`, `chgrp`, `kill`, `pkill`, `killall`, `dd`, `truncate`, `curl`, `wget`) flow to heuristics/classifier.
- New shape-based terminal denies in `isDangerous` (rules.go:141-149):
  - `isCatastrophicRm`: `rm` with recursive+force-ish flags (any short-group or long form) where any operand resolves (via `resolveExisting`) to `/`, `$HOME`, or an **ancestor of the workspace root** → deny. Plain `rm` falls to the heuristic layer: in-workspace ⇒ allow (consistent with existing in-workspace mutation policy), outside ⇒ ask ⇒ classifier.
  - `isDdToDevice`: `dd` with `of=/dev/...` → deny; other `dd` gets `of=` target extraction so out-of-workspace writes stay unprovable ⇒ classifier.
  - Fork-bomb already fails closed at the parser (function decls hit shellparse default case) — corpus test pins it.
- `kill` with only signal flags (`-9`, `-TERM`, `-s sig`) + numeric PIDs (≠1) → deterministic allow (targets one process by ID — the dominant dev-server-restart shape). `pkill`/`killall` (pattern-matched, unprovable) → classifier.
- `curl`/`wget` loopback allow (home: `classifyHeuristic` step before the egress check, heuristics.go:32-36): every URL-shaped operand's host provably loopback (extract/share the loopback check from `fetchhost.go:89-101` — loopback ONLY, not RFC-1918) AND any `-o/--output/-O` target within allowed roots → allow. Anything non-loopback or unprovable keeps `isEgress` ⇒ ask ⇒ classifier (context-aware, no longer hard deny). `egressNames` unchanged. The allow requires non-opaque URLs (forward-compat with #0070's opaque args).
- `git`: drop `reset`/`clean` from `dangerousGitSubcommands` (rules.go:73-77) — workspace-local; operands resolve under cwd ⇒ heuristic allow. `push`: allow iff `cfg.AllowPush` (checked in `evalRules` AFTER deny/ask patterns so config deny/`always_prompt` still win); otherwise treat `push` as **unconditional egress** in `isEgress` (heuristics.go:81-93, drop the host-qualified requirement for `push` only) so it routes ask ⇒ classifier — required because `git push origin main`'s operands (`origin`, `main`) resolve under cwd and would otherwise be deterministically allowed by mutating-path scoping. `clone`/`fetch`/`pull` keep the host-qualified-only rule.

## Tests

- Loader: trusted-only key tests first (see D1 gotcha).
- Rewrite affected `TestEvalRules_Precedence` rows (rules_test.go:16-109): "bare curl coarse deny" and "git push deny" become routing tests.
- New rows: `rm -rf /` deny; `rm -rf ~` deny; `rm -rf ./build` heuristic allow; symlink-escape `rm` regression; `dd of=/dev/disk0` deny; `kill 1234` allow; `kill 1` not-allow; `pkill node` → classifier (stub pattern auto_test.go:74); `curl localhost:3000/health` deterministic allow; `curl https://example.com` → classifier.
- Heuristics: `TestClassifyHeuristic_WriteRootAllowed` mirroring the in-workspace case (heuristics_test.go:63) with a `t.TempDir()` second root; symlink-escape-from-scratch.
- End-to-end auto_test.go rows for the observed real denials.

## Risks / notes

- Biggest risk is over-allowing via the heuristic path once names leave the terminal list — explicit escape tests (symlink-out `rm`, `chmod` on `/etc` → operand `/etc` not in roots ⇒ classifier, acceptable).
- Ordering: D1 → D2 → D3 (D3's `-o`-to-scratch coverage needs D2).
- Related enforcement backstop: #0063/#0064 (bash container substrate + egress control) will make the policy layer defense-in-depth rather than the only line; this change is the policy-layer half and does not wait for them.
