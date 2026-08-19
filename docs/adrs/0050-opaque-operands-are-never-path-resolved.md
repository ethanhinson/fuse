---
id: 50
slug: opaque-operands-are-never-path-resolved
title: Opaque operands are never resolved as filesystem paths for containment proofs
status: Accepted
date: 2026-08-19
supersedes: []
reverses: []
relates_to: [49]
change: 70
---

## Context

fuse's auto-mode permission layer deterministically allows a mutating command when it can *prove*
that every path operand stays inside a trusted root (`withinWorkspace` / `resolveExisting` in
`internal/permissions/heuristics.go`). Change 0070 widened the shell parser so that `$VAR` and
read-only `$(…)` arguments no longer fail the whole command; they become **opaque** args
(`Segment.Opaque []bool`, parallel to `Args`) so the surrounding command can still be evaluated.

That widening created a direct route to a fail-open bypass. `resolveExisting` walks up to the
deepest *existing* ancestor when a leaf does not exist, so handing it the raw source text of
`$(echo /)` or `$VAR` resolves it against the cwd — which is the workspace — and **proves
containment for an operand that is not a path at all**. `rm $(echo /)` would have auto-approved
with no human present.

This is the third recorded member of a family fuse has now been bitten by three times, and the
family — not just this instance — is what this ADR names:

1. **An unexpanded `~` reaching the resolver** (change 0068): the parser hands back the literal
   token, bash expands it at execution, so `touch ~/x` proved "in workspace" while writing to
   `$HOME`.
2. **A non-path operand** (change 0068): `pkill <name>` path-scoped a *process name* against the
   cwd, trivially proving containment for an operation with no filesystem scope whatsoever.
3. **An opaque operand** (this change).

All three fail **silently** and all three fail **open** — they widen the deterministic-allow set,
which is precisely the set that never prompts.

## Decision

An operand whose value is not statically known — an **opaque** operand — is **never resolved as a
filesystem path for a containment proof**. Opaque is *unprovable ⇒ ask the human*, never
*resolves-under-cwd ⇒ allow*.

The invariant is enforced at every consumer rather than at the resolver alone:

- `pathArgs` does not emit opaque positions.
- `classifyHeuristic` treats "this mutating segment has an opaque arg" as `VerdictAsk`. This is the
  load-bearing line: dropping the opaque arg from `pathArgs` *without* the ask would make
  `rm $(echo /)` allow, with zero path args left to fail on.
- `isReadOnlySafe` keeps an opaque arg safe on a genuinely read-only utility (`cat $F`) but fails
  toward unsafe on flag-inspecting names (`sed`, `find`, `git`), where an opaque word could be
  `-i`, `-exec`, or `push`.
- `isLoopbackFetch` requires non-opaque URLs.
- `isProvablyBenignKill` rejects an opaque operand as an unprovable PID.

Adversarial review subsequently found and closed three further leaks of the same shape: nested
command substitutions inside `${X:-$(…)}` parameter expansions, ANSI-C `$'\x2f'` quoted words
carrying undecoded escape text, and an opaque token glob-matching into an `auto_approve` pattern at
the rules layer before the opaque-ask could run.

## Consequences

- **The general rule, which is the durable content:** a containment proof is sound only over an
  operand that **is** a path, **resolved the way the shell will resolve it**. Before any containment
  check runs, two preconditions must be established — the operand is a path at all, and its value is
  actually known.
- Any future consumer that reads `Segment.Args` must consult `Segment.Opaque`. This is a standing
  obligation on new code, not a one-time fix. The package now carries one deliberate, named
  exemption (`isDdToDevice`, prefix-matching in the deny direction only), made structural via an
  accessor precisely so the discipline stays visible.
- The cost is prompts: `rm $VAR` asks even when `$VAR` is harmless. That is the intended trade — the
  alternative is a silent out-of-workspace write primitive.
- Relates to ADR-0049 (allowlist admission on no-human deterministic-allow paths), recorded for the
  same change: both are instances of choosing the fail-closed direction on a path that runs with no
  human present.
