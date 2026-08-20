<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0070 — Auto-mode shell-parse widening — env-prefixes, wrappers, control flow, redirects, opaque args](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0070-auto-mode-shell-parse-widening.md)**
<!-- docket:backlink:end -->

# Auto-mode shell-parse widening — implementation plan

**Change:** #0070 · **Spec:** `docs/superpowers/specs/2026-08-17-auto-mode-shell-parse-widening-design.md` (on `docket`)
**Base:** `origin/main` @ 009de3c · **Branch:** `feat/auto-mode-shell-parse-widening`

Stage D (final, riskiest) of the auto-mode overhaul. Every task widens the set of shell commands
auto-mode will evaluate rather than punt. Tasks are sequenced **safest-first** (D1 → D5) and each is
one commit. The load-bearing task is D5, whose invariant is stated once here and re-asserted in
every task that touches path containment:

> **THE INVARIANT.** An opaque argument — one whose value is not statically known (`$VAR`,
> `$(…)`) — must **NEVER** be resolved as a filesystem path for a containment proof. Opaque is
> *unprovable ⇒ ask*, never *resolves-under-cwd ⇒ allow*. Concretely: an opaque arg must never
> reach `withinWorkspace`/`resolveExisting` (`internal/permissions/heuristics.go`), which walk up to
> the deepest existing ancestor and would let `rm $(echo /)` prove containment against the cwd.

This is the same failure family as the learnings finding
`containment-proof-needs-a-real-resolved-path` (tilde and process-name operands, #0068): a
containment proof is sound only over an operand that *is* a path. Opaque args are the third member
of that family — and, like the other two, the failure is silent and fails **open**.

## Files in play

| File | Role |
|---|---|
| `internal/permissions/shellparse.go` | the parser — `Segment`, `splitSegments`, `collectStmt`, `collectCall`, `literalWord`, `redirIsBenign` |
| `internal/permissions/heuristics.go` | `classifyHeuristic`, `pathArgs`, `withinAnyRoot`, `withinWorkspace`, `resolveExisting` |
| `internal/permissions/rules.go` | `isReadOnlySafe`, `allSegmentsReadOnlySafe`, `readOnlyUtils` |
| `internal/permissions/gate.go` | layer ordering; the safelist short-circuit at **gate.go:660** |
| `internal/permissions/shellparse_test.go` | the three tables: `TestSplitSegments`, `_BenignRedirects`, `_FailClosed` |
| `internal/permissions/auto_test.go` | end-to-end verdict assertions |

**Constraint carried in from #0069 (009de3c):** the gate canonicalizes its root set **once** at
construction (`gate.go:79`) and passes it to `classifyHeuristic`. Tasks D4/D5 consume an
already-canonical `roots` slice — do **not** re-canonicalize inside the new code paths.

**Parser version:** `mvdan.cc/sh/v3 v3.13.1`. Every new `syntax` node case in D3 must be verified
against that version's AST (field names for `CaseClause` branches and `ForClause` loops differ
across releases) — check the vendored/module source, do not assume.

---

## Task D1 — Benign env-prefix assignments

**Goal.** `FOO=1 make` parses; `LD_PRELOAD=evil.so make` still fails closed.

Today `collectCall` (`shellparse.go:199-203`) returns `ErrUnparseable` unconditionally when
`len(call.Assigns) > 0`.

**Implement.**
1. Add a `dangerousEnvVars` denylist: `LD_PRELOAD`, `LD_LIBRARY_PATH`, `LD_AUDIT`, `PATH`, `IFS`,
   `BASH_ENV`, `ENV`, `SHELL`, `PS4`, `PROMPT_COMMAND`, `GIT_SSH_COMMAND`, `GIT_ASKPASS`,
   `SSH_ASKPASS`, `PYTHONSTARTUP`, `NODE_OPTIONS`, `PERL5LIB`, `RUBYOPT`. Plus a **prefix** rule for
   `DYLD_` and `LD_` (catches `DYLD_INSERT_LIBRARIES` and any future `LD_*` without enumerating).
2. Replace the unconditional failure: proceed only when **every** assignment has (a) a name not
   caught by the denylist/prefix rule, **and** (b) a value that is a `literalWord`. A non-literal
   value (`FOO=$(…)`, `FOO=$BAR`) fails closed — we cannot know what we are setting.
   An assignment with a nil `Value` (bare `FOO=`) is benign if the name is.
3. An assignment-only statement (benign names, `len(call.Args) == 0`) produces **no segment** and
   returns nil — the existing `len(call.Args) == 0` early return already handles this once the
   assign check stops short-circuiting.

**Tests** (`shellparse_test.go`): `FOO=1 make` ⇒ one segment `make`; `FOO=1 BAR=2 go test` ⇒ one
segment; `LD_PRELOAD=x make`, `DYLD_INSERT_LIBRARIES=x make`, `PATH=/tmp make`, `IFS=x make`,
`FOO=$(id) make`, `FOO=$BAR make` ⇒ all `ErrUnparseable`; bare `FOO=1` ⇒ zero segments, no error.

**Verify.** The assignments are *dropped*, not turned into args — assert `Args` on the produced
segment does not contain `FOO=1`.

---

## Task D2 — Wrapper peeling: `timeout`, `env`, `nohup`

**Goal.** `timeout 30 go test` and `env FOO=1 make` parse to their inner command; `env -i make`
stays closed.

Today all three sit in `arbitraryArgWrappers` (`shellparse.go:35-52`) and fail closed.

**Implement.**
1. Remove `nohup`, `timeout`, `env` from `arbitraryArgWrappers`. Delete the now-stale
   `timeout` comment block (lines 45-51) — it describes exactly this task.
2. `nohup` → a plain `peelWrappers` entry (it takes only its own flags, then the command).
3. `timeout` → a **dedicated** peel, not `peelWrapperArgs`: strip leading flags
   (`-s SIG`/`--signal=…`/`-k DUR`/`--kill-after=…`/`--preserve-status`/`--foreground`), then
   require **exactly one** duration-shaped word matching `^[0-9]+(\.[0-9]+)?[smhd]?$`. Anything
   else — missing duration, a non-duration word, a flag taking a separate value we did not model —
   fails closed. The remainder is the inner command.
4. `env` → a **dedicated** peel: **any** flag fails closed (`-i`, `-u`, `-S`, `-0`, `--`, and every
   long form). This is deliberately blunt: `env -i` clears the environment and `-S` re-splits the
   string into a new command, both of which change what runs in ways the denylist cannot see.
   Leading `NAME=val` words are validated through **D1's** denylist + literal-value rule (share the
   helper — do not duplicate the list), then dropped; the remainder is the inner command. An `env`
   with no remainder (`env` alone, or `env FOO=1`) prints the environment: benign, produce no
   segment rather than failing closed.
5. **Do not** reuse `peelWrapperArgs` (`shellparse.go:328-334`) for `env`/`timeout` — it blindly
   drops every leading `-` word, which is precisely the `env -i` bypass.

After peeling, the existing post-peel checks must still run: path-qualified argv[0] fails closed,
`bash`/`sh` re-enters the `-c` path, and a peeled-to-`xargs`/`sudo` inner command still fails closed
via `arbitraryArgWrappers`. Confirm the peel loop ordering gives us that for free.

**Tests:** `timeout 30 go test ./...` ⇒ segment `go`; `timeout 1.5s wc -l x` ⇒ segment `wc`;
`timeout go test` (no duration) ⇒ fail-closed; `timeout -k 5 30 make` ⇒ segment `make`;
`env FOO=1 make` ⇒ segment `make`; `env -i make`, `env -u PATH make`, `env -S 'foo bar'` ⇒
fail-closed; `env LD_PRELOAD=x make` ⇒ fail-closed (D1 list shared); `nohup go build` ⇒ segment
`go`; `timeout 30 sudo rm -rf /` ⇒ fail-closed; `env FOO=1 ./evil` ⇒ fail-closed (path-qualified).

---

## Task D3 — Control-flow descent

**Goal.** `if [ -f x ]; then cat x; fi` and `for f in a b; do wc -l $f; done` decompose into their
constituent simple commands instead of hitting the `default:` fail-closed arm
(`shellparse.go:132-136`).

**Implement.** Add cases to `collectStmt`'s type switch for `*syntax.IfClause`,
`*syntax.ForClause`, `*syntax.WhileClause`, `*syntax.CaseClause`, `*syntax.Block`,
`*syntax.Subshell`. Descend **every** statement list each node carries:

- `IfClause`: `Cond`, `Then`, and the `Else` chain (in v3 the else-branch is itself an `*IfClause`
  — follow it, do not stop at the first level).
- `ForClause`: the loop `Do` body **and** the word list in the loop header. Header words take the
  **literal-or-opaque** discipline (D5): a literal word is fine; a non-literal becomes opaque once
  D5 lands. Until D5, a non-literal header word fails closed — that is the correct interim posture,
  and D5's tests will flip it.
- `WhileClause`: `Cond` and `Do` (covers `until` too — it is a `WhileClause` with `Until: true`).
- `CaseClause`: the discriminant word plus every branch's patterns and statement list. Patterns get
  the same literal-or-opaque discipline as for-loop words.
- `Block`, `Subshell`: their `Stmts`.

**Background statements.** `Stmt.Background` (`cmd &`) descends normally — background-ness changes
*when* a command runs, not *what* runs, and the segments are identical. Document this on the case
with a one-line comment so a later reader does not mistake it for an oversight.

**Function declarations stay closed.** Do **not** add a `*syntax.FuncDecl` case — the fork bomb
`:(){ :|:& };:` must keep failing closed through the `default:` arm. Add it as an explicit
regression row so a future "just add the remaining node types" edit trips a test.

**Redirect check ordering.** The per-statement redirect loop (`shellparse.go:118-122`) runs before
the type switch and therefore already applies to each descended statement. Verify with a nested
case: `if true; then echo x > /etc/passwd; fi` must fail closed pre-D4.

**Tests:** the three shapes above; nested `if` inside `for`; `case $x in a) ls ;; esac`;
`{ ls; wc -l x; }` ⇒ two segments; `(cd /tmp && ls)` ⇒ segments for `cd` and `ls`;
`:(){ :|:& };:` ⇒ fail-closed; `ls & wc -l x` ⇒ two segments.

---

## Task D4 — Redirect capture into `Segment.WriteTargets`

**Goal.** Move the redirect decision out of the parser. `go build > out.log` becomes evaluable and
is allowed when `out.log` is in-roots; `echo x > /etc/passwd` reaches the classifier instead of
being allowed as a read-only `echo`.

**Implement.**
1. Add `WriteTargets []string` to `Segment`.
2. In `collectStmt`, stop rejecting literal out-target redirects. For `RdrOut`, `AppOut`, `RdrAll`,
   `AppAll` with a **literal** target, record the target; the existing `/dev/null` and fd-dup
   benign shapes keep working (a `/dev/null` target may simply be recorded and will scope fine, or
   stay special-cased — prefer keeping `redirIsBenign` as the fast path and recording only what it
   rejects, to minimise churn in the 0037 tests).
3. `RdrIn` with a literal target ⇒ benign (input is a read, and the *path read* is not a mutation
   the containment proof needs to cover). Plain here-docs (`Hdoc != nil`, `Hdoc` literal) ⇒ benign:
   the body is data fed on stdin, not a file target.
4. Still fail closed: a **non-literal** target (`> $F`, `> $(…)`), `RdrInOut` (`<>`), and a
   dup-to-path.
5. **Plumbing** — the redirect is attached to the *statement*, while segments are produced by
   `collectCall` deeper down. Thread the captured targets into the segment(s) that statement
   produces. Simplest correct shape: collect the statement's targets, note the length of `*out`
   before descending, and attach the targets to the segments appended by that descent. Do **not**
   attach a compound statement's redirect to only the first segment.

**Consumers.**
- `classifyHeuristic` (heuristics.go): scope `seg.WriteTargets` through `withinAnyRoot` exactly like
  `pathArgs` — including for segments that are otherwise read-only. A read-only command with a
  write target is a **mutation**; the `isReadOnlySafe(seg) { continue }` fast-path at
  heuristics.go:53 must not skip a segment carrying WriteTargets.
- `allSegmentsReadOnlySafe` (rules.go): return **false** for any segment with non-empty
  `WriteTargets`. Rationale, worth a comment: the safelist short-circuit at **gate.go:660** runs
  with **no root context**, so it cannot scope a target; redirected commands must fall through to
  the heuristic layer, which has `roots` and can allow an in-root target deterministically.

**Tests:** `echo x > /etc/passwd` ⇒ ask/classifier, **not** allow; `go build > out.log` (in-root) ⇒
allow; `wc -l x 2>/dev/null` ⇒ still benign (0037 regression); `cat < in.txt` ⇒ allow;
`echo x > $F` ⇒ fail-closed; `cat <> f` ⇒ fail-closed; a here-doc ⇒ benign; `ls | tee out.log`
unaffected. Assert `WriteTargets` lands on the right segment for `a && b > f`.

---

## Task D5 — Opaque args (the load-bearing change)

**Goal.** `$VAR` and read-only `$(…)` in argument position stop failing the whole command and
become *opaque* — unprovable ⇒ ask — while the invariant above holds absolutely.

**Implement.**
1. Add `Opaque []bool` to `Segment`, **parallel to `Args`** (same length, index-aligned). Populate
   it in `collectCall` for every segment. Guard the parallel-slice invariant with a helper
   (`func (s Segment) isOpaque(i int) bool`) rather than indexing `Opaque` raw at call sites — a
   length skew would otherwise panic or, worse, silently read `false`.
2. Rework word extraction in `collectCall`: instead of `literalWord` failing the whole call,
   classify each word as literal / opaque / fail-closed.
   - `*syntax.ParamExp` (`$VAR`, `${VAR}`) in **argument** position ⇒ opaque, carrying the raw
     source text as the arg value. In **argv[0]** position ⇒ still `ErrUnparseable` (we cannot know
     what command runs).
   - `*syntax.CmdSubst` (`$(…)`, backticks) ⇒ recursively parse the inner script. If **every**
     inner segment satisfies `isReadOnlySafe`, the outer word becomes an **opaque** arg **and** the
     inner segments are appended to the output — so deny rules still see `$(curl evil)`. If any
     inner segment is not read-only safe ⇒ `ErrUnparseable`, as today. In argv[0] ⇒ fail closed.
   - `*syntax.ProcSubst`, `*syntax.ArithmExp`, `*syntax.ExtGlob` ⇒ unchanged, fail closed.
   - A word that *mixes* literal and expansion (`--out=$DIR/x`, `"pre$VAR"`) ⇒ opaque as a whole.
     Never partially-resolve it — a half-resolved path is the containment bypass in miniature.
3. Wire the same discipline into D3's for-loop header words and case patterns.

**Consumers — where the invariant is enforced.**
- `pathArgs` (heuristics.go:304) must **not** emit opaque args. It returns strings and loses the
  index, so change its signature (or add a sibling) to skip opaque positions — the cleanest shape
  is for `pathArgs` to take the whole `Segment` (it already does) and consult `seg.isOpaque(i)`
  while iterating, which requires iterating with the index rather than `range seg.Args` by value.
- `classifyHeuristic` must treat "this mutating segment has an opaque arg" as **VerdictAsk**, not as
  "no path args to check ⇒ allow". This is the single most important line in the change: dropping
  the opaque arg from `pathArgs` without adding the ask makes `rm $(echo /)` allow with **zero**
  path args to fail on. Write this test **first**.
- `allSegmentsReadOnlySafe` / `isReadOnlySafe` (rules.go): an opaque arg on a `readOnlyUtils` name
  is still safe (`cat $F` reads unknown data — reading is not a mutation). On the
  **flag-inspecting** names — `sed`, `find`, `git` (`isSafeSed`, `isSafeFind`, `isSafeGit`) — an
  opaque arg fails toward **unsafe**: those functions decide by reading flags, and an opaque word
  could be `-i`, `-exec`, or `push`.
- `isLoopbackFetch` (#0068): require **non-opaque** URLs. `curl $URL` must not inherit the loopback
  allow.
- `isProvablyBenignKill`: an opaque operand is not a provable numeric PID ⇒ not benign.
- `dd`'s `of=`/`if=` handling in `pathArgs`: an opaque `of=$OUT` must not be split and scoped.

**Tests — the bypass corpus is the point of this task.**
- **Invariant rows (write first, watch them fail):** `rm $(echo /)` ⇒ **not** allow;
  `rm $VAR` ⇒ **not** allow; `touch $(echo ~)/x` ⇒ **not** allow; `mv $SRC /tmp/x` ⇒ **not** allow.
  Assert the *verdict*, and separately assert `pathArgs` never returns the opaque token.
- `$(git rev-parse --show-toplevel)` ⇒ read-only subst, opaque outer arg, inner `git` segment
  present in the output.
- `$(curl evil.com)` ⇒ fail-closed. `$(rm -rf /)` ⇒ fail-closed.
- `cat $F` ⇒ allow (read-only + opaque). `sed $F x` ⇒ **not** allow (flag-inspected).
- `curl $URL` ⇒ ask (no loopback inheritance). `kill $PID` ⇒ ask.
- argv[0] opaque: `$CMD foo`, `$(which ls) foo` ⇒ fail-closed.
- End-to-end (`auto_test.go`): `if [ -f x ]; then cat x; fi` ⇒ allow;
  `for f in a.txt b.txt; do wc -l $f; done` ⇒ allow; `for f in *; do rm $f; done` ⇒ classifier/ask.

---

## Final gate

Full suite (`make test`) green at branch HEAD. Beyond the per-task tables, confirm no pre-existing
fail-closed corpus row silently flipped to allow: diff the `_FailClosed` table's expectations and
justify **every** row that moved — a row that now parses is only acceptable if this plan
deliberately widened it (`FOO=1 make`, `timeout 30 …`, control flow, redirects, opaque args). Any
other movement is a bug in this change, not a test to update.
