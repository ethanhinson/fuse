<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0070 — Auto-mode shell-parse widening — env-prefixes, wrappers, control flow, redirects, opaque args](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/archive/2026-08-20-0070-auto-mode-shell-parse-widening.md)**
<!-- docket:backlink:end -->

# Auto-mode shell-parse widening — design

**Change:** #0070 · Stage D of the auto-mode overhaul (arc: #0067 → #0068 → #0069 → #0070) · Riskiest stage; sub-items are separately shippable commits, sequenced safest-first.

## Problem

`splitSegments` (`internal/permissions/shellparse.go`) fails closed → ask on shapes that dominate real agent shell usage: env-prefix assignments (`FOO=1 make`), wrappers (`timeout 30 go test`, `env`, `nohup`), command substitution (`$(git rev-parse --show-toplevel)`), redirections to files (`> out.log`), control flow (`for`/`if`/`while`), and `$VAR` arguments. Each ask is a human prompt (interactive) or a dead end (headless). Claude Code / Cursor parity: these shapes should be evaluated, not punted, wherever safety is provable — and stay fail-closed where it is not.

## Design

All in `internal/permissions/shellparse.go` + consumers (`heuristics.go`, `rules.go`). Land after #0068 so `Segment` field additions happen once against the new rules layer.

### D1. Benign env-prefix assignments (shellparse.go:199-203 as of 009de3c)

Replace the unconditional `ErrUnparseable` on `call.Assigns`: proceed when every assignment's name is NOT in a `dangerousEnvVars` denylist — `LD_PRELOAD`, `LD_LIBRARY_PATH`, `LD_AUDIT`, `DYLD_*` (prefix), `PATH`, `IFS`, `BASH_ENV`, `ENV`, `SHELL`, `PS4`, `PROMPT_COMMAND`, `GIT_SSH_COMMAND`, `GIT_ASKPASS`, `SSH_ASKPASS`, `PYTHONSTARTUP`, `NODE_OPTIONS`, `PERL5LIB`, `RUBYOPT` — AND every value is a `literalWord`. Otherwise fail closed. Assignment-only statements with benign vars produce no segment.

### D2. Wrapper peeling (shellparse.go:35-52, 56-60 as of 009de3c)

Remove `timeout`, `env`, `nohup` from `arbitraryArgWrappers`:
- `nohup` → plain `peelWrappers` entry.
- `timeout` → dedicated peel: strip flags, require exactly one duration-shaped word (`^[0-9]+(\.[0-9]+)?[smhd]?$`), else fail closed.
- `env` → dedicated peel: **any** flag fails closed (`-i`, `-S`, `-u`, `--` tricks); leading `NAME=val` words pass through the D1 denylist; remainder is the inner command.
- Do NOT reuse the blind `peelWrapperArgs` (shellparse.go:328-334, drops `-` words) for env/timeout.

### D3. Control-flow descent (shellparse.go:123-137 default case, as of 009de3c)

Add cases for `*syntax.IfClause`, `*syntax.ForClause`, `*syntax.WhileClause`, `*syntax.CaseClause`, `*syntax.Block`, `*syntax.Subshell` — descend every stmt list (cond, body, else, case branches; for-loop word lists via literal-or-opaque discipline). Loop variables in bodies become opaque args (D5). Background statements (`Stmt.Background`) descend too — background-ness doesn't change what runs; document. `CaseClause` patterns need the same literal-or-opaque discipline.

### D4. Redirect capture (shellparse.go:118-121, 153-180 as of 009de3c)

Move the decision out of the parser: literal out-target redirects (`RdrOut`, `AppOut`, `RdrAll`, `AppAll`) captured into new `Segment.WriteTargets []string`. `RdrIn` with literal target and plain here-docs become benign (input is a read; body is data). Non-literal targets, `RdrInOut`, dup-to-path still fail closed. Consumers: `classifyHeuristic` scopes `WriteTargets` through `withinAnyRoot` like `pathArgs`; `allSegmentsReadOnlySafe` returns false for any segment with WriteTargets (the safelist short-circuit at gate.go:660 as of 009de3c has no root context — redirected commands must reach the heuristic layer, which allows in-root targets deterministically). `echo x > /etc/passwd` ⇒ ask ⇒ classifier.

### D5. Opaque args (the load-bearing change)

Extend `Segment` with `Opaque []bool` parallel to `Args`:
- `ParamExp` (`$VAR`) in **argument** position ⇒ opaque arg carrying raw source text; in **argv[0]** ⇒ still `ErrUnparseable`.
- `CmdSubst` ⇒ recursively parse the inner script; if every inner segment `isReadOnlySafe` ⇒ the outer word becomes an opaque arg AND the inner segments are appended to the output (deny rules still see them); non-read-only inner ⇒ `ErrUnparseable` as today.
- Consumers: `pathArgs`/`withinAnyRoot` treat opaque as **unprovable ⇒ ask** — **THE INVARIANT: an opaque arg must NEVER be resolved as a path for containment** (`rm $(echo /)` must not heuristic-allow; `resolveExisting` walking raw `$(…)` text up to cwd would wrongly prove containment). `allSegmentsReadOnlySafe`: opaque arg on a `readOnlyUtils` name is still safe (`cat $F` reads unknown data — fine); on flag-inspected names (`sed`, `find`, `git`) opaque fails toward unsafe. The #0068 loopback-curl allow requires non-opaque URLs.

## Tests

- Extend the three shellparse_test.go tables (TestSplitSegments:15, _BenignRedirects:137, _FailClosed:207): `FOO=1 make` vs `LD_PRELOAD=x make`; `timeout 30 go test`; `env FOO=1 make`; `env -i make` fail-closed; `$(git rev-parse HEAD)` read-only-subst; `$(curl …)` fail-closed; control-flow descent; redirect capture; `wc x 2>/dev/null` still benign.
- Bypass-regression corpus: opaque-containment (`rm $(echo /)`), `env -i`, fork-bomb (`:(){ :|:& };:` stays closed via function-decl default case).
- End-to-end auto_test.go: `if [ -f x ]; then cat x; fi` ⇒ allow; `for f in a.txt b.txt; do wc -l $f; done` ⇒ allow (read-only + opaque); `for f in *; do rm $f; done` ⇒ classifier.

## Risks / notes

- Highest-risk change of the arc — every widening is an attack-surface decision. Sequence D1→D2→D3→D4→D5 as separate commits, riskiest (opaque/substitution) last, each with corpus tests.
- mvdan.cc/sh node coverage: verify each new `syntax` case against the parser version in go.mod (confirmed at reconcile: `mvdan.cc/sh/v3 v3.13.1`).
- Reconcile note: #0069 landed as 009de3c and canonicalizes the root set once at gate construction (`gate.go:79`) before passing it to `classifyHeuristic`. D4/D5 consumers therefore receive an already-canonical `roots` slice — do not re-canonicalize, and do not let an opaque arg reach `resolveExisting`.
- Adversarial review of the opaque-arg invariant is the review focus for this change.
