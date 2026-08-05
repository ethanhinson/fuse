---
id: 17
slug: auto-mode
title: Auto mode — layered safe/unsafe classification for autonomous tool approval
status: proposed
priority: medium
type: feat
created: 2026-08-05
updated: 2026-08-05
depends_on: []
related: [3, 12, 16]
discovered_from: []
adrs: []
spec:
plan:
results:
trivial: false
auto_groomable:
branch:
pr:
blocked_by:
reconciled: false
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
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
  when interactive; deny with a clear error when not (aligns with 0016's
  non-TTY posture).
- Config surface for the mode and its rule lists in `PermissionsConfig`.

## Out of scope

- OS-level sandboxing (Seatbelt/Landlock/bubblewrap) as an enforcement
  backstop — the right final layer, but its own change if pursued.
- The one-shot CLI approval surface itself (change 0016).
- The MCP server HITL relay (`internal/hitl`).
- Replacing or removing the existing smart mode defaults.

## Open questions

- Ordering vs. 0016 (one-shot approvals): auto mode needs a prompt/deny
  fallback channel in every entry point — does it depend on 0016 or subsume
  it?
- Which model runs the classifier, and through what plumbing (the subagent
  runtime? a direct cheap-model call?)? Latency and token cost per tool call.
- Single-stage or two-stage classifier (cheap block-biased filter → CoT
  review on flagged actions, per Claude Code's design)?
- Does auto mode replace smart mode's internals (the AST layer is strictly
  better than first-token matching) or sit beside it?
- Where do user rule lists live, and is trust config kept out of
  repo-committed files so a cloned repo can't self-authorize?
- Escalation thresholds: after N blocks, pause auto mode and fall back to
  prompting?

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
- **Cline**: the actor model itself sets a `requires_approval` flag per
  command — the weakest design (self-assessment by the thing being gated).
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

## Reconcile log
