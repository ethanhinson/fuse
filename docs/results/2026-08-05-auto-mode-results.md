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

Larger out-of-scope items carried from the spec: OS-level sandboxing (Seatbelt/Landlock/
bubblewrap) as an enforcement backstop; two-stage classifier; signed policy envelopes; hook system.
