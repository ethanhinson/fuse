---
id: 17
slug: auto-mode
title: Auto mode — layered safe/unsafe classification for autonomous tool approval
status: implemented
priority: medium
type: feat
created: 2026-08-05
updated: 2026-08-06
depends_on: []
related: [3, 10, 12, 16]
discovered_from: []
adrs: [5, 6]
spec: docs/superpowers/specs/2026-08-05-auto-mode-design.md
plan: docs/superpowers/plans/2026-08-05-auto-mode-plan.md
results: docs/results/2026-08-05-auto-mode-results.md
trivial: false
auto_groomable:
branch: feat/auto-mode
claimed_at: 2026-08-06T03:33:30Z
pr: https://github.com/ethanhinson/fuse/pull/15
blocked_by:
reconciled: true
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-08-05-auto-mode-design.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/2026-08-05-auto-mode-design.md) |
| Plan | [2026-08-05-auto-mode-plan.md](https://github.com/ethanhinson/fuse/blob/feat/auto-mode/docs/superpowers/plans/2026-08-05-auto-mode-plan.md) |
| Results | [2026-08-05-auto-mode-results.md](https://github.com/ethanhinson/fuse/blob/feat/auto-mode/docs/results/2026-08-05-auto-mode-results.md) |
| PR | [#15](https://github.com/ethanhinson/fuse/pull/15) |
| ADRs | [ADR-0005](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0005-per-segment-allow-rule-evaluation.md), [ADR-0006](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0006-fuse-local-yml-tighten-only-trust-boundary.md) |
<!-- docket:artifacts:end -->

## Why

The permission gate (`internal/permissions`) has three modes: `smart`, `off`,
and `prompt-all`. Smart mode's classification is shallow — a hardcoded
read-only safe list (`read_file`, `list_directory`, `grep`, `codeindex_*`)
plus first-token glob matching on bash commands. First-token matching is the
classic bypassable prefix check: an allow pattern for `bash:git` approves
`git status && rm -rf ~` because only the first token is inspected. There is
no middle ground between prompt fatigue (`prompt-all` / smart's narrow safe
list) and no gate at all (`off`).

An **auto mode** would let an agent run largely unattended by actually
ascertaining what is safe: deterministic static analysis of the command,
risk heuristics, and an LLM classifier for the residual gray area — the
layered architecture every mature harness (Claude Code, Codex CLI, Cursor)
has converged on. This is the feature that makes long autonomous runs viable
without `off`'s blanket risk.

## What changes

**Settled direction (human, 2026-08-05): model the design on Grok Build's
permission pipeline** (`xai-org/grok-build`) — the fullest open-source
implementation of the layered stack. Concretely: an ordered authorization
pipeline of hooks → deny > ask > allow rules with default-Deny → remembered
per-project grants → built-in read-only list → mode policy; per-segment
command splitting on `&&`/`||`/`;`/`|`/newlines including inside `bash -c`;
fail-closed single-unit prompting for unparseable commands (`$()`,
backticks, control flow); a dangerous-command list that re-prompts over
remembered grants; and an auto-mode classifier for the residual gray area.
**One deliberate deviation:** Grok matches *allow* rules against the whole
string only — its docs admit `Bash(git *)` auto-approves
`git status && rm -rf /`. Fuse evaluates allow rules per-segment too
(Claude Code's behavior), closing that hole.

- A new permission mode `auto` in the gate, alongside smart/off/prompt-all.
- **Deterministic static layer** (first, fast, auditable): deny-first
  allow/ask/deny rules; real shell parsing (e.g. `mvdan.cc/sh` gives a bash
  AST in Go) that splits compound commands on `&&`/`||`/`;`/`|`/newlines and
  requires *every* sub-command to independently pass; per-command flag
  inspection (`find -delete`, `git` read-only subcommands only); stripping of
  a fixed set of side-effect-free wrappers; fail-closed on anything
  unparseable, containing command substitution, oversized, path-qualified
  argv[0], or wrapped by an arbitrary-arg runner (`bash -c`, `xargs`, `npx`).
- **Heuristic layer**: read-only vs. mutating classification, path scoping to
  the workspace (symlink-aware), network egress treated as a hard risk
  boundary.
- **LLM classifier layer** (probabilistic, in the middle — never the floor):
  a fast model judges residual actions against user intent (escalation,
  exfiltration, irreversibility). Its input excludes tool results (prompt-
  injection defense) and its verdict is enforced by the gate, never returned
  to the actor model as advisory text.
- **Fallback posture**: blocked/uncertain actions surface as approval prompts
  when interactive; deny with a clear error when not.
- **Subsumes change 0016** (killed as absorbed): the `AlwaysApprove` one-shot
  bypass is removed from every construction site, one-shot runs get a TTY y/N
  approval prompt, non-TTY runs deny-by-default with an explicit
  `--approve-all` opt-in — the configured permission policy means the same
  thing in every entry point.
- **Subsumes change 0032** (killed as absorbed; revised 2026-08-05 after
  first use): the permission mode is a **session-first** surface — a mode
  indicator in the shell TUI, Shift+Tab cycling smart ("standard") ↔ auto,
  and a `/mode` slash command for all modes. The config file is only the
  startup default; the session override wins, is never written back, and
  auto works with zero config (deterministic layers + fail-closed asks when
  no classifier is configured). Spec D10.
- **Per-project defaults (spec D11)**: a `projects:` map in the user-level
  `~/.fuse/config.yml` overrides the permissions subtree per project path
  (longest-ancestor cwd match) — so auto can be the default in trusted
  projects and not others, granted from a file the repo cannot touch.
- **Trust boundary**: permission-loosening keys (`mode`, `auto_approve`,
  `session_allow`, `auto.*`) are ignored from the repo-plantable
  `.fuse.local.yml` with a warning; tightening keys stay honored.
- Config surface for the mode and its rule lists in `PermissionsConfig`
  (`permissions.auto.classifier_model` etc.).

## Out of scope

- OS-level sandboxing (Seatbelt/Landlock/bubblewrap) as an enforcement
  backstop — the right final layer, but its own change if pursued.
- The MCP server HITL relay (`internal/hitl`); `fuse mcp-server`'s no-socket
  `AlwaysApprove` fallback (follow-up candidate).
- Two-stage classifier; signed policy envelopes; hook system.

## Research notes (input for the brainstorm)

How existing tools decide safe vs. unsafe — surveyed 2026-08-05:

- **Claude Code**: deny → ask → allow rule precedence, first match wins;
  compound-command parsing requires each subcommand to match independently;
  wrapper stripping of a known-safe set only; content-field rules rejected
  outright as bypassable. Its `auto` mode adds a separate classifier model
  whose input is user messages + tool calls + CLAUDE.md with **tool results
  stripped** and no actor chain-of-thought; two-stage (fast block-biased
  yes/no, then CoT on flagged); on entering auto mode, broad code-exec allow
  rules like `Bash(*)` are dropped. Trust config is only read from user or
  managed settings, never project files. A separate Haiku-class prompt
  extracts the command prefix and flags `command_injection_detected` for
  substitution/comment/newline smuggling. Docs: code.claude.com/docs/en/
  {permissions, auto-mode-config}; sandbox: github.com/anthropic-experimental/
  sandbox-runtime.
- **Codex CLI**: deterministic argv analysis (`is_safe_command.rs`) — an
  always-safe basename allowlist plus flag-inspecting conditionals (`find`
  unsafe with `-exec`/`-delete`; `git` only read-only subcommands with
  read-only args; `sed` only the `-n Np` form); `bash -lc` scripts are parsed
  and every sub-command must be known-safe. Known bug worth designing out:
  basename-collapse let `./sed` match the safe list (issue #28732) — a path
  separator in argv[0] must force the approval path. Approval policy and
  sandbox mode are orthogonal axes.
- **Gemini CLI**: TOML policy engine (allow/deny/ask_user, priority tiers,
  commandPrefix/commandRegex); redirection operators force confirmation per
  sub-command. Real-world CVE (Tracebit): allowlisted `grep` prefix +
  `; exfil` appended on the same line + whitespace display-truncation ran
  silently — the canonical prefix-match bypass.
- **Cursor**: three-tier auto-review — allowlist check → run in sandbox if it
  fits limits → AI classifier for the rest; plain-English allow/block
  instructions; unconditional protections (file deletion, out-of-workspace)
  even in permissive modes. Known bypass: denylisted commands wrapped in a
  subshell or a script file — prefer allowlists, denylists are advisory.
- **Cline** (source-inspected, `cline/cline`): purely probabilistic — the
  actor model tags each command `<requires_approval>` and the gate consumes
  that flag against category toggles (`AutoApprovalSettings`); no shell
  parsing, no allow/denylist, no chaining analysis anywhere in the approval
  path, and the underlying SDK default-approves unlisted tools (fail-open).
  The weakest design of everything surveyed: self-assessment by the thing
  being gated. (Docs describing a maxRequests cap are stale — marked legacy/
  removed in source.)
- **Grok Build** (source-inspected, `xai-org/grok-build`, Rust): the fullest
  open-source implementation of the layered stack, deliberately Claude-Code-
  compatible (reads `.claude/settings.json` permissions). Ordered pipeline:
  PreToolUse hooks → deny > ask > allow rules (default action hardened to
  **Deny**, comment cites CWE-1188) → remembered per-project grants →
  built-in read-only list → mode policy. Splits commands on
  `&&`/`||`/`;`/`|`/newlines; deny/ask rules check **every segment and the
  whole string** (including inside `bash -c` scripts); un-splittable
  commands (`$()`, backticks, control flow) prompt as a single unit
  (fail-closed). Dangerous-command list (`rm`, `chmod`, `kill`, `git push`…)
  re-prompts even over remembered grants. **Documented deliberate hole:**
  allow rules match the whole string only, not per-segment — their docs
  admit `Bash(git *)` auto-approves `git status && rm -rf /`. Novel idea:
  **Ed25519-signed, identity-bound managed policy** with compiled-in pinned
  pubkeys — cryptographic integrity for enterprise policy, stronger than
  Claude's path-based "never read trust config from the repo."
  Key files: `xai-grok-config-types/src/permission.rs`,
  `xai-grok-config/src/{shell,signed_policy}.rs`.
- **OpenCode** (source-inspected, `sst/opencode`, the active lineage):
  allow/ask/deny rule map per tool with glob patterns
  (`bash: {"git *": allow, "rm *": deny, "*": ask}`), default **ask**, but
  **last-match-wins in user key order** — a later broad `"*": allow`
  silently overrides earlier denies (foot-gun vs. deny-always-first).
  Bash matching is token-regex, not a shell AST — source TODOs admit the
  gap ("Port tree-sitter bash parser-based approval reduction"), so
  compound-command/substitution bypasses are open today. Clean idea worth
  stealing: a separate `external_directory` permission axis — workspace-
  escape is its own approval dimension, decoupled from command safety.
  Subagents get their own permission scoping. Key files:
  `packages/opencode/src/permission/index.ts`,
  `packages/core/src/tool/bash.ts`.
- **Codehamr** (source-inspected, `codehamr/codehamr`, Go): the zero-gate
  extreme — `bash.go` runs the model's command straight through
  `sh -c` with no approval/permission code anywhere in the repo; local-first
  design that trusts the local model and the watching human. Only
  operational bounding: timeout clamp (120s default / 3600s cap),
  process-group kill, bounded head+tail output capture — resource patterns
  worth copying regardless of approval design, but no auto-mode model.
- **Research**: dual-LLM / CaMeL patterns (arxiv 2506.08837) — privileged
  model never sees untrusted content; red-teaming study (arxiv 2509.05755)
  achieved RCE on every tested agent via tool-description + tool-result
  injection, and found Claude's actor could argue past the advisory guard
  verdict — a guard verdict must be enforced by the harness, not the model.

Synthesis: the converged stack is *mode → deterministic deny-first rules →
shell AST per-subcommand/per-flag analysis → LLM classifier → OS sandbox
backstop*, with deterministic layers as floor and ceiling and the
probabilistic layer only in the middle. Fail closed on anything the static
layers cannot resolve. Pitfalls to design out: compound-command prefix
bypass, command substitution/comments/newlines, env-var assignment prefixes,
basename collapse, arbitrary-arg wrappers, denylist wrapping, symlink
escapes, display truncation of the approved command, and untrusted content
reaching the classifier.

Round-2 deltas (source-level survey of Cline / Grok Build / OpenCode /
Codehamr):

- The trust spectrum is now fully visible — Codehamr (zero gate) → Cline
  (model self-assessment only) → OpenCode (deterministic rules, token-regex
  parsing, AST a TODO) → Grok Build (the full stack + signed policy). The
  weak ends are exactly the ones that got RCE'd in the red-team study.
- The single highest-value correctness requirement: **allow rules must be
  evaluated per-segment against a parsed command, never against the raw
  string.** Grok Build ships the whole-string allow bypass knowingly;
  Gemini's CVE was this; only Claude Code closes it fully.
- "Safe command" has three distinct sources — static list (auditable floor),
  model self-assessment (injectable, never sufficient alone), separate
  classifier (semantic coverage, needs hardening). Blend list + classifier;
  never rely on self-assessment.
- Default action must be deny or ask, never allow (Grok's CWE-1188
  hardening; OpenCode's default ask; Cline's fail-open SDK default is the
  counter-example). Deny should win globally regardless of rule order —
  OpenCode's last-match-wins ordering is a documented foot-gun.
- Ideas worth stealing: Grok's Ed25519-signed identity-bound managed policy
  (cryptographic policy integrity vs. path-based trust); OpenCode's
  `external_directory` as its own permission axis; Codehamr's resource
  bounding (timeout clamp, process-group kill, bounded output capture).

## Reconcile log

### 2026-08-05 — reconcile pass (build claim)

Reconciled the change + spec against current `main` code (Explore ground-truth
pass). Every spec assumption verified TRUE against the tree:

- `internal/permissions` structure intact — `gate.go`, `policy.go`,
  `patterns.go`, `cache.go`; three modes `smart`/`off`/`prompt-all` live in
  `policy.go`; `firstToken`/`matchesAny` in `patterns.go`; the hardcoded
  read-only safe list (`read_file`, `list_directory`, `grep`, `codeindex_*`)
  is present.
- `PermissionsConfig` in `internal/config/schema.go` (lines ~28–35) is the
  config surface to extend.
- `AlwaysApprove` construction sites confirmed at `cmd/fuse/main.go:144,146,179`
  and `cmd/fuse/shell.go:150` — the removal targets for D8.
- The LiteLLM gateway entry point is `internal/model/adapter.go:212`
  (`Adapter.Complete`) with per-attempt timeout, response-header timeout,
  retries, and labeled traces — the classifier plumbing (D7) rides this.
- `.fuse.local.yml` CWD merge is `internal/config/loader.go:23`, with **no**
  trust filtering today — D9 is net-new, as designed.
- Expected-absent confirmed absent (these are exactly what 0017 builds): auto
  mode, `--approve-all`, trust-boundary filtering.

**One drift item, fixed this pass:** the spec's Config-surface sketch used
`classifier_model: haiku`, but no `haiku` alias exists in the model registry
(`internal/model/registry.go` `DefaultRegistry` aliases: `deepseek-flash`,
`deepseek-pro`, `kimi`, `glm`, `qwen-cloud`, `qwen-coder`, `qwen-local`,
`llama`, `claude`, `sonnet-5`, `minimax`, `claude-max`). Swapped the example to
the real alias `deepseek-flash`. Behavior is unchanged — unset ⇒ session default
model + startup warning.

No scope change; no work found already done elsewhere; no new constraints to
fold in. Module `github.com/ethanhinson/fuse`, Go 1.26.5; tests are
table-driven with `stubTool`/`newTestRegistry` helpers (informs the plan).
