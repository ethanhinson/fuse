---
slug: containment-proof-needs-a-real-resolved-path
hook: "A path-containment proof is only sound over an operand that IS a filesystem path, resolved the way the shell will resolve it — an unexpanded `~` or a non-path operand (a process name) both resolve against the cwd and silently prove in-workspace."
topics: [security, permissions, shell, parsing, go]
changes: [68, 70]
created: 2026-08-17
updated: 2026-08-20
promotion_state: candidate
promoted_to:
---

## Apply

When a permission layer deterministically allows a command by *proving* its operands stay inside
a trusted root, that proof has two preconditions the code must establish before it runs — not
after:

1. **The operand is resolved as the shell will resolve it.** Shell parsers hand back the literal
   token; tilde expansion is done by *bash at execution*, not by the parser. So a literal `~` or
   `~/x` reaching a `filepath.Abs`-style resolver is joined onto the **cwd** — and `touch ~/x`
   proves "in workspace" while actually writing to `$HOME`. Expand `~`/`~/…` against the real
   home before any containment check.
2. **The operand is a path at all.** Path-scoping an operand that names something else — a
   *process name* for `pkill`/`killall`, a unit, a container — resolves that name relative to the
   cwd, which trivially lands inside the workspace and proves containment for an operation with no
   filesystem scope whatsoever. Operands that are not paths must route to the classifier or ask;
   they can never be deterministically allowed by a containment argument.

Both failures are silent and both fail *open*: they widen the deterministic-allow set, which is
exactly the set that never prompts. Test the bypass shapes explicitly (`touch ~/x`, `pkill name`)
as behavior-table rows alongside the ordinary in-root and out-of-root cases.

## War story

- 2026-08-17 (#68, PR #72) — Shrinking auto-mode's terminal-deny list to catastrophic-only meant
  many previously-denied commands now had to earn a *deterministic* allow via workspace/write-root
  containment. Two pre-existing holes surfaced while widening that path. Rewriting the `rm -rf ~`
  test row exposed the tilde bypass: the parser never expands `~`, so `~/x` path-resolved against
  the cwd and wrongly proved in-workspace — a live out-of-workspace write primitive with zero
  prompts. Separately, once `pkill` left the deny list, its *name* operand would have been
  path-scoped against the cwd and silently allowed; the fix splits the family — `kill` with only
  signal flags and numeric PIDs (≠ 1) is deterministically allowed, while `pkill`/`killall` route
  to the classifier because their operands are names, not paths. The spec had missed the second
  hole entirely; it was found by asking what each operand *is* before asking where it points.
- 2026-08-19 (#70, PR #76) — The third member of the family: ANSI-C quoting. `syntax.SglQuoted`
  carries `Dollar bool` and an **undecoded** `Value`, and neither `classifyWordParts` nor
  `literalWord` consulted `Dollar` — so `rm -rf $'\x2f'` yielded the literal arg `\x2f`, which
  `filepath.Abs` joined onto the cwd and proved in-workspace, while bash decodes it to `/` and
  executes `rm -rf /`. A containment proof ran over a string the shell would never resolve to.
  Fixed fail-safe: `Dollar == true` is opaque/non-literal in both functions (decoding would also be
  correct but is more machinery than the fail-closed direction needs). The change's own invariant —
  opaque operands are never path-resolved — is now ADR-0050; this bug was new code copying a
  pre-existing clause into the one function whose whole job is deciding which words may be resolved.
