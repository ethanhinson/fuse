<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0037 — Redirect fail-closed guard — allow /dev/null targets and fd-dups, keep failing closed on real files](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0037-redirect-guard-lenience.md)**
<!-- docket:backlink:end -->

# Plan — 0037 redirect-guard-lenience

> Authored by docket-implement-next's **auto-fallback** (the configured
> `superpowers:writing-plans` skill is not installed on this machine).

## Goal

Replace the blanket `if len(st.Redirs) > 0 { return ErrUnparseable }` guard in
`internal/permissions/shellparse.go` `collectStmt` with a per-redirect
classification: a redirect is BENIGN (does not force fail-closed) iff it targets
literal `/dev/null`, or is a pure fd-duplication (`2>&1`, `>&2`). Any other
redirect still returns `ErrUnparseable` (fail closed, unchanged). Read-only
pipelines that only silence stderr (`wc -l a.go 2>/dev/null | tail -5`) parse
and classify correctly instead of stalling the auto-mode run.

## Ground truth (from reconcile)

- Vendored `mvdan.cc/sh/v3 v3.13.1`. `syntax.Redirect{OpPos, Op, N, Word, Hdoc}`.
- `Op` is a `RedirOperator`; relevant constants:
  - `RdrOut` (`>`), `AppOut` (`>>`), `RdrIn` (`<`), `RdrInOut` (`<>`)
  - `DplIn` (`<&`), `DplOut` (`>&`) — fd-dup ops
  - `RdrAll` (`&>`), `AppAll` (`&>>`) — stdout+stderr to a file
  - `Hdoc` / `DashHdoc` / `WordHdoc` — here-docs
- `2>` → `Op=RdrOut, N="2"`. `2>&1` → `Op=DplOut, N="2", Word="1"`.
  `>&2` → `Op=DplOut, Word="2"`. `&>` → `Op=RdrAll`.
- `literalWord(*syntax.Word) (string, bool)` already exists and returns
  `ok=false` for any word carrying substitution / param-expansion / glob — reuse
  it to enforce "literal `/dev/null` only."

## Benign classification rule (the new `redirIsBenign` helper)

A `*syntax.Redirect` r is benign iff **exactly one** of:

1. **`/dev/null` file target.** `r.Op` ∈ {`RdrOut`, `AppOut`, `RdrIn`, `RdrAll`}
   AND `r.Hdoc == nil` AND `r.Word != nil` AND `literalWord(r.Word)` returns
   `("/dev/null", true)`. The literal check rejects `/dev/null.txt`,
   `/dev/nul`, `$F`, `/dev/null` reached via a variable/glob/substitution.
   (`AppAll` `&>>` to `/dev/null` — accept it too: same benign target. Include
   `AppAll` in the op set.)
2. **Pure fd-duplication.** `r.Op` ∈ {`DplIn`, `DplOut`} AND `r.Word != nil`
   AND `literalWord(r.Word)` is all-ASCII-digits (a bare fd number like `1` or
   `2`), with no `/`. `r.N` (the source fd, e.g. the `2` in `2>&1`) is
   unconstrained — a numeric fd literal by grammar.

Everything else — here-docs (`Hdoc`/`DashHdoc`/`WordHdoc`, i.e. `r.Hdoc != nil`
or those ops), `RdrInOut` (`<>`), any non-`/dev/null` file word, a dup whose
target is a path or non-numeric — is NOT benign → the statement fails closed.

Guard becomes: reject the statement iff **any** redirect in `st.Redirs` is not
benign. (So a mixed `ls 2>/dev/null > /etc/x` still fails: the `> /etc/x`
redirect is not benign.)

## Tasks (TDD — red corpus first)

### Task 1 — Red: benign redirect corpus (new failing test)

In `internal/permissions/shellparse_test.go`, add `TestSplitSegments_BenignRedirects`
asserting these parse with **no error** and yield the expected read-only
segment(s):

- `wc -l a.go 2>/dev/null | tail -5` → 2 segments (`wc`, `tail`), no error.
- `ls > /dev/null` → 1 segment (`ls`), no error.
- `ls >> /dev/null` → 1 segment, no error.
- `cat < /dev/null` → 1 segment, no error.
- `make 2>&1 | tail` → 2 segments (`make`, `tail`), no error. (`make` itself is
  classified elsewhere; the redirect must not block *parsing*.)
- `echo hi >&2` → 1 segment (`echo`), no error.
- `ls &> /dev/null` → 1 segment, no error.

Assert segment Name(s) via the existing table-test shape (see
`TestSplitSegments`). This test FAILS on the current blanket guard (every row
returns `ErrUnparseable`). Run `go test ./internal/permissions/ -run BenignRedirects`
and confirm red.

### Task 2 — Add fail-closed regression rows (extend existing table)

In `TestSplitSegments_FailClosed`, add rows that MUST stay `ErrUnparseable`
under the new lenience (guarding the boundary):

- `{"dev-null lookalike file", "ls > /dev/null.txt"}`
- `{"dev-null typo", "ls 2>/dev/nul"}`
- `{"redirect to variable target", "cat a > $F"}`
- `{"here-doc", "cat <<EOF\nhi\nEOF"}`
- `{"input redirect from real file", "cat < config.yaml"}`

(The existing rows — `echo x > /etc/passwd`, `grep foo bar >> ~/.zshrc`,
`ls > out.txt`, `ls 2>/dev/null > /etc/x` — already cover the real-file and
mixed cases and must keep passing.) Run the full `TestSplitSegments_FailClosed`;
the five new rows fail red only if the implementation is wrong — with the
current blanket guard they all (correctly) pass, so this task's new rows are the
regression net that Task 3 must not break.

### Task 3 — Green: per-redirect benign check in shellparse.go

Add the `redirIsBenign(r *syntax.Redirect) bool` helper (rule above) and a small
`allRedirsBenign([]*syntax.Redirect) bool`. Replace lines 111-113:

```go
if len(st.Redirs) > 0 {
    return ErrUnparseable
}
```

with:

```go
if !allRedirsBenign(st.Redirs) {
    return ErrUnparseable
}
```

Update the doc comment on `collectStmt` (and the package-level `splitSegments`
doc that says "any redirection … fails closed") to state the benign exception
precisely. Reuse `literalWord`. Add a small `isAllDigits(string) bool` helper
(or inline) for the dup-target numeric check.

Run `go test ./internal/permissions/...` — all green (Task 1 now passes, Task 2
rows stay closed, all pre-existing tests unchanged).

### Task 4 — Full verification

- `go build ./...`
- `go test ./...`
- `go vet ./internal/permissions/...`
- `gofmt -l internal/permissions/` (expect empty)

## Out of scope

Here-docs, input redirects from real files, real in-workspace output files —
all still fail closed (per the change body). No changes outside
`shellparse.go` / `shellparse_test.go`.

## Files touched

- `internal/permissions/shellparse.go`
- `internal/permissions/shellparse_test.go`
