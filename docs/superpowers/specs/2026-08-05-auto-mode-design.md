<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0017 — Auto mode — layered safe/unsafe classification for autonomous tool approval](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0017-auto-mode.md)**
<!-- docket:backlink:end -->

# Auto Mode — Layered Safe/Unsafe Classification for Autonomous Tool Approval

**Spec for change 0017** · groomed 2026-08-05 (inline brainstorm — `superpowers:brainstorming` unavailable this session)

---

## Overview

A new permission mode `auto` for the HITL gate (`internal/permissions`), modeled on
Grok Build's permission pipeline (`xai-org/grok-build`) — the fullest open-source
implementation of the layered safe/unsafe stack — with one deliberate hardening
deviation (per-segment allow matching). The deterministic core (a real shell parser
with per-segment rule evaluation) **replaces** smart mode's first-token glob matching
for all modes; auto mode adds an LLM classifier for the residual gray area and a
deny-and-continue fallback.

This change **subsumes killed change 0016**: the `AlwaysApprove` one-shot bypass is
removed and a CLI approval surface added, so the configured permission policy means
the same thing in every entry point.

## Decisions (settled in the groom, with rationale)

### D1 — Grok Build's authorization pipeline, adapted to Fuse

Ordered evaluation per tool call; first decisive layer wins:

1. **Disabled list** — `permissions.disabled` tools are not registered (unchanged).
2. **Session cache** — allow-for-session grants (unchanged, highest user-grant
   precedence; dangerous-list commands re-prompt anyway, see D6).
3. **Rules: deny > ask > allow** — user-configured patterns, evaluated
   **per-segment** (D2). A rule with a missing/unknown action is **Deny**
   (Grok's CWE-1188 hardening: omitting `action` must never create a
   catch-all allow). Deny wins regardless of order or specificity.
4. **Built-in read-only list** — auto-approve (current safe list, extended: see D5).
5. **Dangerous-command list** — force `ask` even over session grants (D6).
6. **Mode policy** — `auto`: residual actions go to the classifier (D7);
   `smart`: residual actions prompt (unchanged semantics, new engine);
   `prompt-all`: everything prompts; `off`: everything approves.
7. **Fallback surface** — prompt when an approval channel exists; deny with a
   clear error when none does (D8).

Fuse has no hook system, so Grok's PreToolUse-hook layer is omitted — nothing
replaces it; the pipeline starts at the disabled list.

### D2 — Per-segment evaluation for ALL rule kinds (the deviation from Grok)

Grok matches deny/ask rules per-segment but allow rules against the whole string
only — its own docs admit `Bash(git *)` auto-approves `git status && rm -rf /`.
Fuse evaluates **allow rules per-segment too** (Claude Code's behavior): a compound
command is auto-approved only if *every* segment independently matches an allow rule
or the read-only list. This closes the bypass class behind Gemini CLI's CVE and
Cursor's denylist bypass. One denied segment rejects the whole command.

### D3 — Real shell parsing via `mvdan.cc/sh/v3`, fail-closed

Commands are parsed into an AST and split on `&&`, `||`, `;`, `|`, `|&`, and
newlines — including inside `bash -c` / `sh -c` inline scripts. Fail-closed rules
(the command prompts as a single opaque unit, or is denied in auto mode's
non-interactive posture):

- Parse failure, or command over a size cap (~10k chars).
- Command substitution (`$(…)`, backticks), process substitution, background `&`,
  redirection to a file, control-flow constructs (`for`/`while`/`if`).
- argv[0] containing a path separator (`./sed`, `/tmp/git`) — the Codex
  basename-collapse bug designed out; basename matching applies only to bare names.
- Arbitrary-arg runners (`xargs`, `env … cmd`, `npx`, `docker exec`, `find -exec`,
  `watch`, `nohup`, `sudo`): never peeled, never inherit the inner command's allow.
- Env-var assignment prefixes (`FOO=bar cmd`) are stripped **for allow/read-only
  matching only**; deny/ask rules also match the unstripped form.
- A fixed side-effect-free wrapper set (`timeout`, `nice`, `stdbuf`, `time`) IS
  peeled before matching, per Grok/Claude.

### D4 — The AST engine replaces first-token matching for every mode

`patterns.go`'s `firstToken`/`matchesAny` is **removed outright** — no compat shim,
no legacy path (human call: Fuse has no users yet; dead code paths are worse than a
clean break). `auto_approve` / `always_prompt` patterns keep their existing YAML
shape (`bash:git *`-style) but are evaluated by the new per-segment engine, so smart
mode inherits the safety fix for free.

### D5 — Read-only built-in list (extended)

Current safe list (`read_file`, `list_directory`, `grep`, `codeindex_*`) plus a
word-boundary-matched read-only shell command set, per segment:
`ls cat head tail grep rg find pwd echo wc which stat du diff sort uniq tr cut nl`
plus read-only `git` forms (`status log diff show branch --list rev-parse`), with
Grok/Codex's flag carve-outs: `find` is NOT read-only with
`-exec/-execdir/-ok/-delete/-fls/-fprint*`; `git` not read-only with `-c`/`-C`/
`--config-env`; `sort` not with `--compress-program`; `tee` excluded entirely.
Word-boundary matching (`ls` ≠ `lsof`).

### D6 — Dangerous-command list

`rm chmod chown chgrp pkill kill killall git push git reset git clean dd mkfs
truncate shutdown reboot` force a prompt even when covered by a session grant or a
remembered pattern. An explicit user `allow` rule or `off` mode still runs them —
the list guards against over-broad grants, not against explicit intent.

### D7 — The classifier (auto mode's probabilistic middle layer)

- **Plumbing**: a direct chat call through the existing LiteLLM gateway to a
  configurable model alias, `permissions.auto.classifier_model` (references the
  models registry; falls back to the session default model with a startup warning
  if unset). Not the subagent runtime — too heavy per tool call.
- **Single-stage, block-biased**: one structured verdict call
  (`allow | deny | ask` + one-line reason). Two-stage CoT review (Claude Code's
  design) is a future upgrade, not v1.
- **Input hygiene** (the round-1/round-2 hardening, non-negotiable):
  the classifier sees the user's messages and the pending tool call — **never tool
  results, never the actor's reasoning**. Its verdict is enforced by the gate;
  it is never surfaced to the actor as advisory text the model could argue past.
- **Bounded** per the `bound-every-model-call` learning: per-attempt timeout
  (default 10s), response-header timeout, one retry, labeled trace entry.
  Timeout/error ⇒ treat as `ask` (fail closed to the fallback surface, D8).
- **Verdict caching**: identical (tool, normalized-command) verdicts cached per
  session to bound cost on repetitive calls.

### D8 — Fallback surface & escalation (subsumes 0016)

- `permissions.AlwaysApprove` is removed from every construction site: one-shot
  root agent, one-shot child agents (`cmd/fuse/main.go`), and shell child agents
  (`cmd/fuse/shell.go`). The configured mode is enforced everywhere.
- **Interactive TTY (one-shot)**: pending approvals surface as a terminal y/N
  prompt with the existing preview line, honoring `session_allow`. The shell TUI
  flow is unchanged. Child/subagent approvals route to the same parent channel.
- **Non-TTY (piped stdin, CI)**: deny with a clear structured error. Explicit
  opt-in flag `--approve-all` on `fuse run` restores approve-all for scripted use
  (the deliberate, visible footgun — equivalent to `mode: off` for that run).
- **Deny-and-continue**: a classifier deny or non-TTY deny is returned to the model
  as a tool error naming the layer that blocked it, so the model can try a safer
  path.
- **Escalation valve**: 3 consecutive or 20 total classifier blocks in a session
  pauses auto mode — falls back to prompting when interactive, aborts the run with
  a summary when not (Claude Code's thresholds).
- `fuse mcp-server` without a HITL socket keeps its current `AlwaysApprove`
  fallback — out of scope here, noted as a follow-up candidate.

### D9 — Trust boundary: `.fuse.local.yml` cannot loosen policy

The CWD-merged `.fuse.local.yml` is repo-plantable, so **permission-loosening keys
are ignored from it with a startup warning**: `permissions.mode`,
`permissions.auto_approve`, `permissions.session_allow`, and the whole
`permissions.auto.*` block resolve from `~/.fuse/config.yml` + built-ins only.
Tightening keys (`always_prompt`, `disabled`) stay honored from either file.
Implemented as a clean field-level merge policy in the loader — no legacy flag, no
dead path (per the human's direction). Grok's signed-policy envelope was considered
and rejected as enterprise machinery a local single-user tool doesn't need yet.

## Config surface (sketch)

```yaml
permissions:
  mode: auto            # off | prompt-all | smart | auto
  session_allow: true
  auto_approve: ["bash:git *"]   # allow rules — per-segment now
  always_prompt: ["bash:git push*"]
  disabled: []
  auto:
    classifier_model: haiku      # models-registry alias; unset ⇒ session default + warning
    deny: []                     # extra deny patterns, per-segment
    ask: []                      # extra ask patterns, per-segment
```

## Testing notes

- A table-driven **bypass corpus** is the heart of the test suite: compound
  commands (`git status && rm -rf ~`), substitution (`git diff $(curl …)`),
  comments/newlines (`git status# $(id)`, `pwd\nrm -rf .`), env prefixes
  (`URL=evil curl $URL`), path-qualified argv0 (`./sed`), wrappers
  (`xargs rm`, `timeout 5 ls`), word-boundary (`lsof` vs `ls`), redirection.
  Every entry asserts the *layer* that catches it, not just the outcome.
- Classifier tests stub the gateway; verify input contains no tool results,
  verdicts are enforced (not echoed to the actor), and timeout ⇒ ask.
- Loader tests: loosening keys in `.fuse.local.yml` are ignored with a warning;
  tightening keys merge.

## Out of scope

- OS-level sandboxing (Seatbelt/Landlock/bubblewrap) — its own future change.
- The MCP server HITL relay internals; `fuse mcp-server`'s no-socket
  `AlwaysApprove` fallback (follow-up candidate).
- Two-stage classifier; signed policy envelopes; hook system.

## Resolved open questions (from the stub)

- 0016 relationship → **subsumed**; 0016 killed as absorbed.
- Classifier plumbing → configurable gateway alias, single-stage (D7).
- Replace smart internals → yes, engine replaces first-token matching (D4).
- Rule/trust config location → user-level only for loosening keys (D9).
- Escalation thresholds → 3 consecutive / 20 total (D8).
