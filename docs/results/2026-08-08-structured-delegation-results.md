<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0024 — Structured delegation — expected result schemas for spawn_agent](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0024-structured-delegation.md)**
<!-- docket:backlink:end -->

# Structured delegation — results

Change: #0024 · Branch: feat/structured-delegation · PR: <set at PR open> · Plan: docs/superpowers/plans/0024-structured-delegation.md · ADRs: ADR-0012

## Verify (human)

Automated tests cover the seam types, the pure validator (fidelity cases + lenient
extraction), producer prompt injection, the match/mismatch result path, the tool
param round-trip, and a real-adapter gateway-seam e2e for match and mismatch. No
manual check is strictly required at the merge gate. Optional live look:

- [ ] (optional) Real-model smoke: run the interactive shell, ask the model to
      `spawn_agent` with an `expects` schema, and confirm the returned tool result
      carries the `(matched expected schema)` / `(result did NOT match expected
      schema: …)` note and that the agent tree shows a `schema_mismatch` event on a
      deliberate miss. The seam is covered by
      `cmd/fuse/structured_delegation_e2e_test.go` against a scripted gateway, but a
      live run is the ultimate check (learning `verify-tool-loop-at-gateway-seam`).

## Findings

- **ADR-0012 — vendored a JSON-Schema library.** The design's D2 (full-fidelity
  validation, not a shallow key check) required a real engine;
  `github.com/santhosh-tekuri/jsonschema/v6` (v6.0.3, + transitive
  `golang.org/x/text v0.31.0`) was vendored, pinned, and isolated to
  `internal/agent/schemavalidate.go`. Recorded as ADR-0012. Note: the library's
  `ErrorKind.LocalizedString` panics on a nil `message.Printer`, so a fixed English
  printer is passed at the one call site.

- **`AgentHandle` stays copy-safe.** `Wait()`/`Result()` memoize the single
  buffered-channel receive via a **pointer**-held `doneMemo{sync.Once}`, not an
  inline `sync.Once` — because `AgentHandle` is returned and passed by value across
  the wiring and an embedded lock would trip `go vet`'s copylock check (and could
  double-receive). Verified drain-safe under `-race`.

- **The note composes with existing suffixes.** The schema match/mismatch note is
  appended to the result **inside the child goroutine** in `spawn.go`, before the
  tool's `Execute` appends `budgetLine()` + `quotaWarning()` — so the three compose
  in order with no double-append.

## Review nits (optional, non-blocking — deferred, not fixed)

The pre-PR whole-branch review returned no blockers and no should-fix items. Two
optional nits were raised and deliberately left for a follow-up rather than expanded
in this change:

1. **Guard asymmetry (latent, unreachable today).** The producer-side prompt
   directive fires on `opts.Expects != nil`, while result-side validation runs only
   when `opts.Expects` type-asserts to `map[string]any`. A caller passing a non-map
   schema (e.g. a boolean JSON Schema) would have the child *told* to conform but the
   parent silently never validate. Unreachable via the current tool seam (which
   always decodes `expects` to `map[string]any`); worth tightening if a code-driven
   non-map `Expects` ever becomes possible.
2. **Per-return schema recompile.** `validateAgainstSchema` compiles the schema on
   every child return. Fine at the current one-spawn cadence; a compile-once/cache-by-
   schema would help if change 0026 fans this out.

## Plan deviations

- **Concurrent build on a shared worktree.** This change was built by two overlapping
  sessions (a `docket-implement-next` run plus a dispatched build agent) operating in
  the same feature worktree. The edits converged (same design, tests green), redundant
  duplicate test files were de-duplicated before the PR, and the final API adopted an
  `ErrNoStructuredResult` sentinel from `AgentHandle.Result()` (cleaner than returning
  `(nil, nil)` on no-match). No functional divergence; noted for provenance.
