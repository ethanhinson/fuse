<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0037 — Redirect fail-closed guard — allow /dev/null targets and fd-dups, keep failing closed on real files](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0037-redirect-guard-lenience.md)**
<!-- docket:backlink:end -->

# Redirect fail-closed guard — /dev/null + fd-dup lenience — results
Change: #37 · Branch: feat/redirect-guard-lenience · PR: <pending> · Plan: docs/superpowers/plans/0037-redirect-guard-lenience.md · ADRs: 5

## Verify (human)

Automated tests cover the corpus fully; the checks below are optional
confidence spot-checks in a real auto-mode run at the merge gate.

- [ ] In an auto-mode session, run a read-only stderr-silencing pipeline
      (e.g. `wc -l internal/**/*.go 2>/dev/null | tail -5`) and confirm it no
      longer stalls on a permission prompt for the redirect.
- [ ] Confirm an out-of-workspace write still prompts:
      `echo x > /tmp/should-prompt.txt` must NOT be silently allowed.

## Findings

- The security boundary is preserved by construction: the change only widens
  what *parses* into segments, never what the classifier *allows*. `/dev/null`
  and fd-dup targets carry no writable file the read-only classifier could
  miss, so no write primitive escapes the per-segment allow evaluation
  (ADR-0005).
- The `/dev/null` match reuses the existing `literalWord` helper, which already
  returns `ok=false` for any variable / glob / command-or-process substitution.
  That is what makes `$F`, `/dev/nul$X`, `$(…)`, and globbed targets fail closed
  for free — no new expansion logic was needed.
- Review edge, locked in by empirical parse probe against `mvdan.cc/sh/v3
  v3.13.1`: `>&` (op `DplOut`) with a **path** word — `ls >&file` — is bash's
  "redirect both streams to a real file", a genuine write. The benign fd-dup
  branch gates on `isAllDigits(word)`, so `>&file` (non-numeric), `2>&-`
  (fd-close), and `<>` (read-write, op `RdrInOut`) all correctly fall through to
  fail-closed. Regression rows were added for all three so the numeric-only gate
  and the `<>` default can't silently regress.

## Follow-ups

- None. Here-docs, input redirects from real files, and real in-workspace
  output files remain fail-closed by design (out of scope per the change body);
  no new change was minted (auto-capture is disabled in this repo).
