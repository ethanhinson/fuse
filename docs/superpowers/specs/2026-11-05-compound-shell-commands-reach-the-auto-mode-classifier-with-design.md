# Compound shell commands: parse-layer decomposition with per-part deterministic evaluation

Change 0142 — Compound shell commands: parse-layer decomposition with per-part deterministic evaluation

## Problem

In auto mode, compound shell commands are the top source of human approval
interrupts. A review of the real approval history (1,890 `permission.decision`
events across recent sessions) shows the pattern precisely:

- **101 asks total, 96 of them bash.**
- **80 of the 101 asks (79%) came from the parse layer** — the fail-closed
  "unparseable ⇒ ask the human" path — not the classifier and not genuine gray
  area. The classifier saw only 102 decisions and asked the human just 16
  times.
- **81 of the 96 bash asks start with `cd`**: `cd <dir> && <routine command>`
  (`cd "$(pwd)" && docket …`, `cd <worktree> && git rev-parse HEAD`,
  `cd … && gh pr view`). The compound operators in the asks are `&&` (83),
  `|` (62), `>` (55), `;` (48), `$(` (36).

The individual parts of these commands — `ls`, `cat`, `grep`, `zcat`,
`git rev-parse`, `docket`, `gh` — are on the safelist/heuristic layers and
would auto-approve **if run separately**. The gate's deterministic layers
reason first-token-at-a-time, so the moment a command is compound the parser
refuses it wholesale and the gate punts to the human. The user shepherds every
multi-step command by hand: the exact prompt fatigue auto mode exists to
eliminate.

Change 0070 widened shell-parse; this change makes the auto-mode gate *use*
that decomposition instead of treating "compound" as "unparseable".

## Design

### Where it hooks in

The auto-mode authorization pipeline is ordered: deterministic rules layer
(catastrophic deny, scoped allow) → shell-parse heuristics → LLM classifier
(gray area) → human ask. The parse layer today is a single accept/refuse
verdict; a refuse with a human bound records a `LayerParse` ask. The parser
already decomposes compound commands into segments — pipeline stages,
`&&` / `||` / `;` operands, and command substitutions — but the gate collapses
that to a single unparseable verdict instead of evaluating the segments.

### What changes

1. **Evaluate compounds per-segment against the deterministic layers.** When
   the parser decomposes a command into segments, the auto-mode gate evaluates
   **each segment** against the same deterministic rules and heuristics it
   would apply to a standalone command. The compound is auto-approved only when
   **every** segment is independently provable-safe **and** the composition
   introduces no new risk. This moves `cd <in-workspace dir> && git status`,
   `ls x | grep y`, and `cat a && cat b` off the human prompt entirely — the
   79% of today's asks whose parts are all routine.

2. **Composition is not free.** The gate still reasons about how segments
   combine, not only the parts: the canonical unsafe compositions are a
   producer piped into a shell (`curl … | sh`), a command substitution whose
   result is executed, and a redirect that writes outside the workspace.
   A compound containing any such composition is not auto-approved.

3. **Residual gray area still gets judgment — and now structure.** A compound
   that is parseable but not deterministically provable (a segment that is
   gray-area on its own, e.g. a non-loopback `curl`) routes to the classifier —
   and now carries the decomposed structure (each segment with its per-segment
   deterministic verdict) into the classifier's pending-call context, so the
   model verdicts the genuinely-hard segment instead of the whole opaque
   string. This is the secondary, narrower role for the classifier-context
   work; the deterministic decomposition is the primary win.

4. **Attribute compound decisions in the audit record.** The
   `permission.decision` event for a compound carries the per-segment verdicts
   (and which segment drove an ask or deny), so the outcome stays attributable
   to the specific part — not just the raw string. `LayerParse` stops being the
   catch-all ask bucket for routine compounds.

### What does not change

- The deterministic catastrophic-deny set and the permission floor are
  evaluated before any of this, exactly as today.
- A command the parser *genuinely cannot* decompose (malformed, unsupported
  syntax) keeps the existing fail-closed path: `LayerParse` ask with a human
  bound, rules deny with a policy stand-in.
- Smart, prompt-all, and off modes are untouched; this is auto-mode only.
- The generative classifier's model, prompt base contract (0069), and the
  human prompt UX itself are unchanged.

## Boundaries

- **Not** widening the deterministic scoped-allow rules themselves (new
  provable-safe command *shapes*) — that is a 0068-style follow-up.
- **Not** replacing the generative gray-area classifier with a System 1
  typed-decision model (e.g. Laya) — a deliberately deferred follow-up. That
  idea is sequenced *after* this change precisely because this change shrinks
  the classifier's workload to the genuine gray area, which is both the right
  model workload and the clean fine-tuning distribution. The System 1 spike
  (export `permission.decision` history, fine-tune, measure allow/deny/ask
  accuracy + calibration) is evaluated once this lands.
- **Not** session-scoped / remembered approvals ("always allow this shape") —
  a separate grant-model change.
- **Not** changing the human prompt UX.

## Verification

- A compound of independently-safe segments (`cd <worktree> && git status`,
  `ls x | grep y`, `cat a && cat b`) is auto-approved with no human prompt, and
  the audit record shows per-segment allows — not a `LayerParse` ask.
- A compound with one unsafe segment (`cat x && rm -rf ~`) is denied at that
  segment; a compound with one gray-area segment (`make test && curl https://x`)
  routes that segment to the classifier, and the classifier sees the decomposed
  structure.
- An unsafe composition (`curl … | sh`, executed `$(…)`) is not auto-approved
  even when each part is individually safe.
- A genuinely unparseable command keeps the existing bypass-classifier /
  `LayerParse`-ask path.
- Smart / prompt-all / off modes are byte-for-byte behavior-identical.
