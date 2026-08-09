<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0040 — Auto-mode flow parity — in-workspace edits auto-approve](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0040-auto-mode-flow-parity.md)**
<!-- docket:backlink:end -->

# Auto Mode Flow Parity — In-Workspace Edits Auto-Approve

**Spec for change 0040** · groomed 2026-08-09 (design settled in `reports/2026-08-09-auto-mode-flow-parity.md`; brainstorm skill unavailable — designed inline from the report)

Follow-up to **change 0017** (auto-mode). Preserves **ADR-0005** (per-segment allow evaluation) and **ADR-0006** (`.fuse.local.yml` tighten-only trust boundary) unchanged.

---

## Problem

Fuse's `auto` permission mode stops far more often than Claude Code's accept-edits/auto flow, even though the security model is sound (and in places stronger). The stopping is **structural**, not excess caution:

1. **`write_file` / `edit_file` are not recognized as in-workspace edits.** Both are non-bash tools absent from the safe-list (`internal/permissions/policy.go:71-98`), so every write/edit is handed to the block-biased LLM classifier on every call (`gate.go:405`). Claude Code's defining auto-mode behavior is auto-approving *in-workspace* edits. The pipeline already contains the exact check needed — `classifyHeuristic → withinWorkspace` (`heuristics.go:147`, symlink-aware containment in `workspaceRoot`) — but it only runs for **bash**, never the edit tools. The gate already holds `workspaceRoot` (`gate.go:71`, wired at `run.go:425`); both tools expose a plain `path` arg (`write.go:34`, `edit.go:35`).
2. **The classifier judges blind.** `classifyOrAsk` calls `Classify(ctx, nil, ...)` — user history is hard-wired to `nil` (`gate.go:468`). Spec 0017 D7 says the classifier "sees the user's messages"; that plumbing was never done. With only `tool + args` and a "be block-biased" system prompt (`classifier.go:54-59`), a bare `write_file internal/foo.go` reads as uncertain → ask.
3. **Read-only / orchestration non-bash tools aren't safe-listed.** `segment_read` (read-only), `web_search`, `web_fetch`, `skill`, `pipeline_run` all route to the classifier → ask.
4. **The escalation valve then pauses auto mode entirely.** 3 consecutive / 20 total classifier blocks pause auto (`gate.go:99-102`). Because #1–#3 push routine safe actions into the classifier, benign blocks accrue and trip the valve mid-task.

Net effect on a typical "implement feature X" run: reads flow, but the first edit of every file hits the blind classifier → ask; a handful trips the valve → auto pauses. This is the reported "stops a lot."

The full findings with file:line citations live in `reports/2026-08-09-auto-mode-flow-parity.md` (repo root, integration branch).

---

## Decisions

### D1 — Path-scope the edit tools (the core fix)

In `resolveAuto`'s non-bash branch (`gate.go:405-410`), before falling to the classifier, add an edit-tool branch that extracts `path` and runs the existing `withinWorkspace`:

- `write_file` / `edit_file` with a `path` resolving **inside** the canonicalized `workspaceRoot` ⇒ **allow**.
- A path that escapes the root (`../`, absolute-outside, in-workspace symlink whose target escapes) ⇒ **ask**.
- Missing/garbled `path`, or unparseable args ⇒ **ask** (fail toward the human).

Reuses `withinWorkspace` / `resolveExisting` verbatim — symlink-aware, and already handles not-yet-created files by resolving the deepest existing ancestor. No new bypass class: it admits only paths the same check bash mutations already trust.

### D2 — Extend the safe-list

Add to `onSafeList` / `safeList` (`policy.go`): `segment_read` (read-only), and `web_search` / `web_fetch` / `skill` / `pipeline_run` (network-read + orchestration, matching the existing `spawn_agent` rationale — children are independently re-gated by a cloned gate). Each entry carries a rationale comment like the existing `spawn_agent` / blackboard entries. A user who wants any of them gated again can demote via `permissions.always_prompt`.

### D3 — Give the classifier its context (fulfills 0017 D7)

Plumb the user's messages to `classifyOrAsk` so gray-area verdicts are informed rather than blind-block-biased. To avoid widening the `agent.ToolExecutor` interface (`agent.go:17-19`, implemented widely), carry the user turns on the `context.Context`:

- A new unexported `ctxKey`; `agent/loop.go:executeToolBounded` attaches the current user messages before `a.tools.Execute` (`loop.go:607`/`618`).
- `classifyOrAsk` reads them off `ctx` (nil-safe: absent ⇒ today's behavior).
- **Input hygiene preserved** — `buildMessages` already drops non-user roles (`classifier.go:197-205`), so tool results and actor reasoning still never reach the classifier.

### D4 — Accept-edits posture + valve retune

- In-workspace writes/edits (D1) never reach the classifier, so they never feed the valve — this alone stops benign work from tripping it.
- Only a classifier **deny** counts toward the valve, never an **ask** (today `recordBlock` fires only on deny — keep, and verify D1's asks bypass `classifyOrAsk` entirely, which they do).
- Leave `valveConsecutiveLimit = 3` (a real signal). Revisit `valveTotalLimit` after measurement now that only true gray-area denies count.

---

## Security review

- **No new bypass.** D1 auto-approves only paths resolving inside the canonicalized workspace root, using the same symlink-resolving check bash mutations already trust. An in-workspace symlink pointing out is caught (the link is resolved).
- **ADR-0006 trust boundary intact.** No loosening key is read from `.fuse.local.yml`; `workspaceRoot` is process-derived (`run.go:425`).
- **ADR-0005 unchanged.** Per-segment bash evaluation is untouched; D1 adds allow paths for the edit tools only, never relaxing segment evaluation.
- **Classifier hygiene intact (D3).** Only `role=="user"` turns are forwarded.
- **Escape hatches preserved.** Out-of-workspace edits, egress, dangerous commands still stop; `always_prompt` re-arms any safe-listed tool.

---

## What changes (files)

- `internal/permissions/gate.go` — non-bash edit-tool branch (D1); ctx-read for classifier context (D3).
- `internal/permissions/policy.go` — safe-list additions (D2).
- `internal/permissions/classifier.go` — accept forwarded user messages (already parameterized as `Classify(ctx, userMessages, …)`).
- `internal/agent/loop.go` — attach user messages to ctx before tool execute (D3).
- Tests across `internal/permissions/*_test.go` and any agent seam test.

No config schema change: `AutoConfig` already carries `deny` / `ask` / `classifier_model`.

---

## Testing notes

- **Edit-tool scoping** (`heuristics_test.go` / `gate_test.go`): `write_file` / `edit_file` with (a) in-workspace path → allow, (b) `../` escape → ask, (c) symlink-out-of-root → ask, (d) not-yet-created in-workspace file → allow, (e) missing/garbled `path` → ask.
- **Safe-list** (`safelist_test.go`): each newly added tool → allow in smart & auto.
- **Classifier context** (`classifier_test.go`): stub gateway asserts forwarded messages contain the user turns and still contain **no** tool-result / assistant messages.
- **Valve** (`valve_test.go`): a run of in-workspace edits does not advance the valve; only classifier denies do.
- **Regression:** the existing bypass corpus (spec 0017 "Testing notes") stays green.

---

## Out of scope

- OS-level sandboxing (Seatbelt/Landlock/bubblewrap).
- Two-stage classifier CoT (spec 0017's noted future upgrade).
- Any change to bash segment evaluation, egress boundary, or the dangerous-command list.
- Config schema additions.

---

## Recommended sequencing (for the build plan)

1. **D1 + D2** first — isolated to `internal/permissions/`, no agent seam; removes ~90% of the stopping and is independently shippable/testable.
2. **D3** next — adds the agent→ctx seam; improves residual gray-area quality.
3. **D4** last — guided by measurement once D1–D3 land.

May build as one PR or split D1+D2 / D3+D4; the plan step decides.
