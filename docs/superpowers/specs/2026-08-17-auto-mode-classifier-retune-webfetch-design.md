<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0069 — Auto-mode classifier retune + web_fetch loosening — allow-bias for routine dev ops, seed becomes real auto-approve](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/archive/2026-08-19-0069-auto-mode-classifier-retune-webfetch.md)**
<!-- docket:backlink:end -->

# Auto-mode classifier retune + web_fetch loosening — design

**Change:** #0069 · Stage C of the auto-mode overhaul (arc: #0067 → #0068 → #0069 → #0070)

## Problem

Both LLM classifier prompts are block-biased ("when in doubt, prefer ask or deny"), and the web_fetch known-good seed is only a hint. Real data: 32 web_fetch classifier denies of ordinary hosts — lmstudio.ai, web.archive.org, hn.algolia.com, html.duckduckgo.com, bing.com, google.com, substack — plus bash classifier denies of routine `/tmp`/package operations. Product bar: Claude Code / Cursor parity — web reads and routine dev ops are not deny-shaped.

## Design

### D1. Bash classifier retune (`internal/permissions/classifier.go`)

- Rewrite `classifierSystemPrompt` (classifier.go:54-59): permission gate for a coding agent working in a workspace; routine dev operations are expected and should be **allowed** — network reads (curl/wget/API calls), package installs (npm/pip/cargo/go), managing the agent's own dev-server processes (kill/pkill of dev processes), temp/scratch directory use. **Deny only named dangerous shapes**: secret/workspace-data exfiltration, piping remote content into a shell, privilege escalation, destruction outside the workspace, credential harvesting. Ask for genuinely ambiguous calls.
- Workspace context: `pendingCallPrompt` (classifier.go:279-281) gains workspace root + scratch dir ("workspace: %s, scratch: %s") via new Classifier fields set from `autoModeOptions` (fields copied in `cloneForChild`, classifier.go:142-151; no `ToolExecutor` interface churn).
- Input hygiene (classifier.go:261-275) unchanged — bias changes, hygiene contract does not.

### D2. web_fetch seed promotion + allow-biased GET prompt

- Split the seed (fetchhost.go:26-60): exact/suffix host entries (github.com, pypi.org, wikipedia.org, readthedocs.io, rfc-editor.org, …) become a **strong** set ⇒ real auto-approve. Broad TLD wildcards (`*.gov`, `*.edu`, `*.dev`, fetchhost.go:57-59) stay nudge-only — auto-approving every `.dev` domain is unacceptable widening.
- `classifyFetchHost` (fetchhost.go:130-159): floor order unchanged (SSRF → config `fetch_deny` → config `fetch_ask` → blocklist — config always beats the seed); then `strongSeedMatch(host) || reputation.KnownGood(host)` ⇒ `Verdict: VerdictAllow, DecidedBy: "known-good"`. `reputation.KnownGood` (reputation.go:162-170, top-sites set) covers the observed bing/google/duckduckgo/archive.org denials.
- `gate.go:449-459`: handle `DecidedBy == "known-good"` allow before routing fallthrough to `classifyWebFetch` — known-good hosts never invoke the classifier.
- Remaining hosts: rewrite `webFetchPendingPrompt` (classifier.go:244-253) allow-biased — "web_fetch performs a read-only GET returning page text; fetching public web pages is routine — default allow. Deny shapes: URLs carrying credentials/tokens/secrets, webhook endpoints (hooks.slack.com, discord.com/api/webhooks, …), paste/upload services usable for exfiltration, raw-IP URLs, URLs encoding workspace data." (`web_fetch` is verified GET-only: tools/web_fetch.go:52-60.)

### D3. Valve total retune

- Keep `valveConsecutiveLimit = 3` (the deny→identical-retry amplification breaker). Raise `valveTotalLimit` 20 → 50 (gate.go:106-109): with rules-layer denies gone (#0068) and the classifier allow-biased, honest long sessions must not trip; 50 preserves a hard headless abort budget against a persistently probing model. Drive tests off the constant, not literals.

## Tests

- fetchhost_test.go: known-good fallthrough test becomes an auto-approve assertion; keep spoof/subdomain regression (`hostMatchesSuffix` is dot-boundary-safe — keep the compromised-subdomain caveat test); "config fetch_deny beats known-good"; "TLD wildcard is nudge-only".
- gate_test.go: extend `TestResolveAuto_WebFetchStaticFloor` (line 302) — known-good host allows with **zero** classifier invocations (stub asserts).
- classifier_test.go (lines 88, 144, 203): prompt-content assertions — system prompt names allow-shapes and deny-shapes; pending prompt carries workspace root. Behavioral verdict tests unchanged (stubbed completer).
- valve_test.go: total-limit test driven off the constant.

## Risks / notes

- Prompt-only changes are cheap to iterate; the real guard is that terminal denies (#0068's catastrophic floor) and the SSRF floor no longer depend on classifier bias.
- Seed poisoning-by-subdomain already mitigated by dot-boundary matching — regression-pin it.
- Sequence D1 before/with D2 (shared prompt wording), D3 last.
