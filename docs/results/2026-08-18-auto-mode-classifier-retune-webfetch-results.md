<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0069 — Auto-mode classifier retune + web_fetch loosening — allow-bias for routine dev ops, seed becomes real auto-approve](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0069-auto-mode-classifier-retune-webfetch.md)**
<!-- docket:backlink:end -->

# Auto-mode classifier retune + web_fetch loosening — results

Change: #0069 · Branch: feat/auto-mode-classifier-retune-webfetch · PR: (see change `pr:`) · Plan: docs/superpowers/plans/2026-08-18-auto-mode-classifier-retune-webfetch-plan.md · ADRs: 48

## Verify (human)

The suite proves the *wording* of both prompts and every deterministic floor layer. It cannot prove
how a real model behaves under the retuned prompts — that is the whole point of the change, and it
is what these checks are for. **Use a cheap gateway model for all of them; never an Anthropic/Claude
model.**

- [x] **Routine dev ops now allow.** In auto mode, run a handful of the shapes that used to deny:
      a `curl` of a public API, an `npm install` / `go get`, a write into the session scratch dir,
      and `pkill -f vite` against a dev server you actually started. Expect allow, not ask/deny.
      *Verified 2026-08-18 (headless one-shot, glm driver + deepseek-flash classifier): all four
      allow. Caveat: the scratch write allows via `write_file`/`edit_file`; a bash `>` redirect into
      the scratch dir still asks, because `splitSegments` fails closed on every real-target redirect
      (change 0037) — that is the #0070 redirect-capture scope, not a regression here.*
- [x] **The kill bound actually bites.** Still in auto mode, try `pkill -9 -f .` and
      `killall -9 Finder`. Expect **ask**, not allow — this is the clause fix `a96fc0a` added, and it
      is the one place the prompt deliberately narrows against the surrounding allow bias.
      *Verified 2026-08-18 execution-free (real gate + live classifier over a stub bash tool):
      `pkill -9 -f .` → ask ("broad -f pattern"); `killall -9 Finder` → deny rather than ask —
      stricter than spec'd, fail-safe direction; noted as a prompt-adherence quirk, not a gap.*
- [ ] **Out-of-workspace writes ask.** Try `cp .env ~/Library/x` and `echo x >> ~/.zshrc`.
      Expect **ask**. (Note the caveat under Findings — whether a `>>` redirect target is scoped by
      the deterministic layer is unverified; if the redirect one does not ask, that is the known gap,
      not a regression in this branch.)
- [x] **Known-good fetches are silent and free.** Fetch `https://github.com/...`,
      `https://google.com`, `https://web.archive.org/...`. Expect no prompt **and no classifier call
      at all** — confirm zero `auto-classifier` entries appear in the trace for these fetches. That
      "zero calls" property is the actual deliverable of D2; a silent allow that still burned a
      model call would mean the floor arm is not being hit.
      *Verified 2026-08-18 with one correction: `github.com` and `google.com` are silent floor
      allows with zero `auto-classifier` trace entries. `web.archive.org` is NOT in the known-good
      set (matching is exact-host post-review; only `archive.org` is in the CSV), so it goes to the
      fallthrough classifier by design — this check's expectation for that URL was stale. That
      fallthrough then exposed a real bug, fixed in-branch: `classifierMaxTokens = 128`
      deterministically truncated deepseek-flash mid-reasoning (empty content → fail-closed ask for
      every fallthrough host, silently defeating the allow-bias). Raised to 512 (observed real
      usage 101–297 completion tokens); with the fix the same fetch classifies a clean allow.*
- [ ] **The valve does not trip on an honest long session.** Run a genuinely long auto-mode session
      and confirm it does not pause. The old budget was 20 total blocks; it is now 50.

## Findings

- **13 review findings** (1 blocker, 6 important, 6 minor) from the deep whole-branch review, all
  repaired in-branch. The PR body carries the full disposition table with commit SHAs.
- **The blocker was a real, live auto-approve bypass, introduced by this change's own premise.**
  `url.Hostname()` preserves a trailing root dot, but `reputation.normalize` strips one. So
  `https://google.com./?q=…` missed `fetch_deny` and missed `strongSeedMatch` (both exact matchers)
  while `reputation.KnownGood` matched — yielding `VerdictAllow` / `known-good` with zero classifier
  calls, against an explicitly configured deny. The same gap put `http://localhost./admin` past the
  SSRF check. Pre-#0069 this URL still reached the classifier, so promoting the seed to a real
  auto-approve is what made it terminal. Fixed by canonicalizing once before every layer through a
  shared `reputation.CanonicalHost`, so the two spellings of the rule cannot drift apart again.
  The lesson generalizes: **when two layers match the same operand with different normalizations,
  promoting either one to an authorization decision turns the mismatch into a bypass.**
- **ADR-0048** records the resulting architecture: the web_fetch host floor as a real authorization
  boundary, and the six bounds that make a data-backed host set safe to authorize from
  (strong-seed-only, config-beats-seed ordering, single canonicalization, exfil-shape subtraction,
  a pinned promoted-set test, and deciding machine-checkable URL properties at the floor rather than
  delegating them to a classifier that only sees the host).
- **The popularity CSV changed category.** It was a bias hint; it is now an authorization source.
  Nothing in code had bounded it, while `generate.go` documented a refresh procedure that would
  happily import paste sites and link shorteners. The branch now subtracts a hardcoded exfil-shape
  denylist and pins the exact promoted set in a test, so a refresh that widens authorization fails
  the suite instead of shipping silently.
- **Open-registration namespaces were wrongly in the strong seed.** `*.github.io` and
  `*.readthedocs.io` were auto-approving; anyone can register `attacker.github.io`. Demoted to
  nudge-only. `*.stackexchange.com` and `*.wikipedia.org` were checked against the same criterion
  and kept — those subdomains are operator-minted. The criterion is now stated explicitly in the
  source: *the operator controls the hostname namespace*, not merely *the operator is known*.
- **Unverified caveat worth a look at merge time:** whether `splitSegments` surfaces a `>>` redirect
  target as a path arg was not established. If it does not, `echo … >> ~/.zshrc` may not reach the
  path scoping at all. The new prompt clause covers it *once the classifier sees it*; the
  deterministic layer's redirect handling is a separate question and was out of scope here.

## Addendum — decision audit completeness (2026-08-19, in-branch)

Live verification surfaced that `permission.decision` events were not audit-complete: the
classifier's rationale was discarded at parse time, the classifier call itself had no record
(the 128-token truncation shipped precisely because a truncation-ask was indistinguishable from a
considered ask), human-layer events could not tell a real approval from loop-serve's
`AlwaysApprove` stand-in, and nothing in metrics or logs could filter by verdict/layer. Fixed
in-branch:

- **Events** (full-fidelity tier): decisions now retain the classifier's bounded reason on every
  verdict, carry a `classifier` block (`model`, `latency_ms`, token counts, `truncated`,
  `parse_ok`, `cached`), and stamp `decided_by: human|policy` on human-layer outcomes. Previews
  are scrubbed of URL userinfo (results follow-up #2). All additions are append-only payload
  fields.
- **Metrics**: `fuse_permission_decisions_total{tenant_id, verdict, layer}` and
  `fuse_permission_classifier_replies_total{tenant_id, outcome}` (outcome: ok | truncated |
  parse_error | cached), derived from the event projection — bounded enums only, inside the
  0051 payload-free/cardinality contract.
- **Logs**: projection log lines for decisions carry `verdict`, `decision_layer`, and
  `classifier_outcome` beside the existing `trace_id`/`loop_id`/`sequence`, so incident triage
  pivots metric spike → filtered log line → `(loop_id, sequence)` in the event store →
  `trace_id` in Tempo.

Verified live against the hosted binding (`loop-serve-net`, glm driver + deepseek-flash
classifier, dev observability stack): one loop produced the full verdict×layer spread
(classifier allow ×2 with reply-health `ok`, fetch_floor allow, parse ask, human/policy allow,
rules deny), visible end-to-end in `docs/results/screenshots/grafana-permission-decisions-table.png`,
`…-series.png`, and `grafana-tempo-decision-trace.png` (the trace a decision log line's
`trace_id` resolves to). Note the shell/one-shot bindings serve only Observer-path metrics —
event projection (and thus these counters and log fields) runs where the durable store is wired,
i.e. the hosted runtime; the event-store payloads themselves are identical in every binding.

## Follow-ups

Auto-capture is disabled for this repo, so none of these were minted as stubs — they are recorded
here for a human to file.

1. **Redirects defeat the host floor** (`internal/research/http.go`). No `CheckRedirect` is set
   anywhere in the tree, so Go follows up to 10 hops. The floor authorizes the *requested* host; a
   known-good host that 302s to `169.254.169.254` is fetched with no further gate. This predates the
   branch but the branch changes its weight — the host went from a hint to the terminal
   authorization. The fix belongs in `internal/research` (re-run the SSRF check, ideally the whole
   floor, per hop). Documented in `classifyFetchHost`'s doc comment by commit `499c23a`.
2. **Credentials leak into the decision event stream.** `Decision.Command` carries the raw web_fetch
   args via `commandPreview`, so a `https://<token>@host/…` URL puts the token in the event stream
   regardless of the new `credentialed-url` Ask. Redacting userinfo from decision previews is
   independent of this change.
3. **`raw.githubusercontent.com` is the same exfil shape one level down.** It passes the
   namespace-control criterion (GitHub controls that single hostname) but serves arbitrary user repo
   content, so the exposure is at the path level rather than the hostname level. Worth revisiting if
   the floor ever gains path awareness.
4. **The web_fetch prompt sees only the host.** Two of the spec's named deny shapes — credential-
   bearing URLs and URLs encoding workspace data — are path/query properties. The credential half is
   now decided deterministically at the floor. The **query-string-encodes-workspace-data** half
   remains unenforced and unenforceable as built; closing it means either widening the prompt's input
   surface to the full URL or adding a deterministic query heuristic. That is a real design decision,
   not a defect in this branch.
