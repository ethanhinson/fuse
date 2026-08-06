<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0017 — Auto mode — layered safe/unsafe classification for autonomous tool approval](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0017-auto-mode.md)**
<!-- docket:backlink:end -->

# Auto mode — layered safe/unsafe classification — results

Change: #0017 · Branch: feat/auto-mode · PR: (opened at close-out) · Plan: docs/superpowers/plans/2026-08-05-auto-mode-plan.md · ADRs: 0005, 0006

Auto mode adds a fourth permission mode (`auto`) alongside `smart`/`off`/`prompt-all`,
built as the converged layered stack: deterministic deny-first rules over a real shell
AST (per-segment) → read-only safe-list + heuristics → LLM classifier for the residual
gray area → fail-closed fallback. It also removes the `AlwaysApprove` one-shot bypass
(subsuming change 0016) and adds a repo-plantable-config trust boundary.

## Verify (human)

Automated tests cover the pipeline (bypass corpus, classifier stubs, loader trust-boundary
tests — all green). These are the manual checks worth doing at the merge gate, since they
exercise real-model / real-TTY behavior the suite stubs out:

- [ ] Run `fuse` interactively with `permissions.mode: auto` and confirm a plainly-safe
      compound read command (`git status && git diff`) auto-approves, while a mixed command
      (`git status && rm -rf ~`) is rejected/prompted — the per-segment allow evaluation
      (ADR-0005) in action.
- [ ] Confirm a redirect target is never silently approved: `echo hi > /tmp/x` prompts/denies
      rather than classifying as the read-only `echo` (review finding C1).
- [ ] Plant a `.fuse.local.yml` in a scratch dir setting `permissions.mode: off` (and an
      `auto_approve: ["bash:*"]`), run `fuse` from that dir, and confirm the loosening keys are
      ignored WITH a logged warning while any tightening key (e.g. `always_prompt`) is honored
      (ADR-0006 / D9).
- [ ] With `permissions.auto.classifier_model` unset, confirm fuse falls back to the session
      default model and emits the startup warning (no `haiku` alias exists; `deepseek-flash`
      is the real registry alias — see reconcile log).
- [ ] Exercise the escalation valve: confirm that 3 consecutive (or 20 total) classifier
      blocks in a session pauses auto mode and surfaces to the human.
- [ ] Non-TTY one-shot run denies by default; `--approve-all` opt-in is required to bypass
      (replaces the removed `AlwaysApprove`).

**D10 — in-session mode switching (interactive, TTY-only; the suite stubs the TTY):**

- [ ] Start `fuse` interactively at the default `smart`. Press **Shift+Tab**: the status line
      flips to `mode: auto` and a `mode: auto` transcript line appears. Issue a read-only command
      (e.g. ask the agent to run `git status`) and confirm it auto-approves WITHOUT a prompt on the
      very next turn — this is the regression that reopened 0017 (toggle to auto, still prompted),
      now guarded by `TestSessionModeFlipToAuto_NextGateAutoApprovesReadOnlyBash`.
- [ ] Press **Shift+Tab** again → back to `smart`. From `off` or `prompt-all` (set via config or
      `/mode`), the first Shift+Tab lands on `smart`.
- [ ] Type `/mode` (bare) → it prints the active mode and lists `smart, auto, prompt-all, off`.
      `/mode auto`, `/mode prompt-all`, `/mode off`, `/mode smart` each switch and echo; `/mode bogus`
      prints a usage line and leaves the mode unchanged (NOT silently smart).
- [ ] Start with the gateway unconfigured (no classifier), switch into `auto`: the status line
      shows the **degraded** marker (`mode: auto (degraded — no classifier)`), and gray-area
      commands fail closed to a prompt rather than reaching a classifier.
- [ ] While a slash-completer overlay is open (type `/` then a letter), press Shift+Tab — the mode
      must NOT flip (review S1 fix); the completer owns the keyboard.

**D11 — per-project permission overrides (config-driven; worth one live check):**

- [ ] In `~/.fuse/config.yml` add a `projects:` block keyed on an absolute project path
      (e.g. `/Users/you/dev/trusted-repo`) with `permissions.mode: auto`. Run `fuse` from
      *inside* that dir (or a subdir) and confirm the effective mode is `auto`; run it from an
      unrelated dir and confirm it stays at the global default — the longest matching path-segment
      ancestor wins, a raw prefix sibling (`/a/bc` under `/a/b`) never matches.
- [ ] Confirm a `projects:` block planted in a repo-plantable `.fuse.local.yml` is IGNORED with a
      warning (only the trusted home config sinks projects) — a cloned repo cannot self-elevate.

## Findings

- **C1 (CRITICAL, review round 1 — fixed):** `collectStmt`/`collectCall` in
  `internal/permissions/shellparse.go` extracted `CallExpr.Args` but never inspected
  `Stmt.Redirs`, so a write-redirect target (`echo x > /etc/passwd`, `grep foo bar >> ~/.zshrc`)
  was invisible to the read-only classifier and reached `VerdictAllow` via
  `allSegmentsReadOnlySafe`. Fixed by an early `if len(st.Redirs) > 0 { return ErrUnparseable }`
  guard at the top of `collectStmt` — before the switch, so it fires on every statement position
  (top-level, each operand of a compound command, and inside a peeled `bash -c` body; `Redirs`
  attaches to `*syntax.Stmt`, which is the recursion chokepoint). Focused re-review confirmed the
  bypass is fully closed with no regression and no residual redirect-shaped hole.
- **ADR-0005 — per-segment allow-rule evaluation** (deliberate deviation from the Grok Build
  reference). Grok evaluates allow rules against the whole command string only (its docs admit
  `Bash(git *)` auto-approves `git status && rm -rf /`); fuse evaluates allow rules per-segment
  against a parsed AST (Claude Code's behavior), so a compound command auto-approves only if
  every segment independently matches. Closes the single highest-value bypass class in the
  surveyed designs (Gemini's CVE, Cursor's denylist wrap).
- **ADR-0006 — `.fuse.local.yml` tighten-only trust boundary.** The CWD-merged, repo-plantable
  `.fuse.local.yml` cannot loosen policy: loosening keys (`mode`, `auto_approve`, `session_allow`,
  `auto.*`) are stripped with a warning at load time; tightening keys stay honored. A cloned repo
  can never weaken the gate of the human who runs fuse inside it.
- **D10 — in-session mode switching (subsumes killed change 0032).** First real use showed the
  config file is the wrong PRIMARY activation surface — mode must be a live session surface. The
  fix: a session-owned `*permissions.SessionMode` holder (mutex-guarded both sides) is the single
  source both the TUI (indicator, Shift+Tab, `/mode`) and per-turn gate construction read. The
  shell holds **no long-lived root gate** — `ShellModel.startPrompt` rebuilds the agent+gate each
  turn via `buildGate → sessionGateMode → sm.Get()`, so `SessionMode.Set` alone bites the next
  turn (no live `PermissionGate.SetMode` call needed on the shell path). The classifier is now
  wired whenever *constructible* (gateway configured), not gated on `mode == auto`, so a
  mid-session switch into auto is fully powered. Live-switch semantics were the top review item and
  cleared with no blockers; the reopening symptom (toggle to auto, still prompted) is now covered by
  an integration-shaped regression test.
- **Review S1 (should-fix — fixed):** Shift+Tab fell through the completer-navigation guard (which
  only consumes Up/Down/Esc/Enter) and flipped the mode with the completer overlay open. Fixed with
  an explicit completer-active re-check in the `KeyShiftTab` case + corrected comment.
- **D11 — per-project permission overrides in user config.** A `projects:` map in the trusted home
  config keys absolute project paths to their own `permissions` subtree; on load, the entry whose
  key is the longest path-segment ancestor of (or equal to) the current working directory is merged
  as trusted (full subtree incl. `mode` and `auto.*`). Trust boundary: **only the trusted home
  merge sinks `projects:`** — a `projects:` block in the repo-plantable `.fuse.local.yml` is parsed,
  ignored, and warned, so a cloned repo can never self-elevate via a project entry. Matching is by
  path segment, not raw prefix (`/a/bc` never matches a `/a/b` key), with `filepath.Clean` +
  `EvalSymlinks` on both sides and unresolvable keys skipped; longest-key-wins on overlap. The merge
  reuses the existing `mergePermissions` (byte-identical refactor — home → project → local-tighten →
  env precedence preserved). Focused D11 review: **no blockers** — trust boundary, path matching,
  refactor fidelity, and precedence all PASS, tests green.
- **D11 review nits (both fixed in-file, non-blocking):** (a) `TestProjectOverrideNoMatch` now uses
  a real resolvable sibling temp dir key so it exercises the resolved-but-non-ancestor branch rather
  than reaching the no-op via the unresolvable-key skip; (b) `applyProjectOverride`'s `bestKey`
  (only ever read for its length) renamed to a `bestLen int`.

## Follow-ups

Deliberate deferrals from this change (out of scope, PR-body noted, candidates for their own
changes — not auto-captured; `auto_capture` is disabled in this repo):

- **Conversation-history threading into `Classify`** — the classifier currently judges the
  residual action against user intent without the full conversation-history context threaded in;
  richer intent context is a follow-up.
- **`fuse mcp-server` no-socket `AlwaysApprove` fallback** — the MCP server's no-socket path still
  falls back to approve; wiring it to the configured policy is deferred (the HITL relay,
  `internal/hitl`, is out of scope for 0017).
- **`timeout` wrapper peeling** — `timeout` is kept fail-closed ("timeout-then-unknown"); peeling
  it safely requires stripping its own flags and mandatory duration argument in `shellparse.go`
  plus new corpus rows. Deferred as a conservative posture (costs a prompt, never a bypass).

D10 review nits (non-blocking; reported, not auto-captured — `auto_capture` disabled):

- **N1 — `fuse mcp-server` classifier wiring.** `cmd/fuse/mcp_server.go` still calls
  `permissions.New` with no auto-mode options, so `mode: auto` under `mcp-server` runs with a nil
  classifier regardless of gateway config. Pre-existing (not a D10 regression, not touched by this
  diff); the "classifier-regardless-of-mode" intent does not reach the mcp-server path. Candidate
  follow-up.
- **N2 — dormant classifier allocation.** `autoModeOptions` now allocates a `NewClassifier` on
  every gateway-configured gate build even for non-`auto`/non-flippable paths (one-shot / mcp,
  which can never flip mid-session). Inert (only reached from `resolveAuto` when `mode == Auto`) and
  intentional for flip-readiness on the shell path, but wasted work off the shell path. Optional
  micro-optimization: skip when `sm == nil && cfg mode != auto`.
- **N3 — `/mode <valid> <extra>` drops trailing tokens** silently (matches `/model`'s behavior).
  Harmless.

Larger out-of-scope items carried from the spec: OS-level sandboxing (Seatbelt/Landlock/
bubblewrap) as an enforcement backstop; two-stage classifier; signed policy envelopes; hook system.
