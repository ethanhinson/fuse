---
id: 48
slug: web-fetch-host-floor-as-authorization-boundary
title: The web_fetch host floor is an authorization boundary, and the bounds that make it safe
status: Accepted
date: 2026-08-17
supersedes: []
reverses: []
relates_to: []
change: 69
---

## Context

Auto-mode's `web_fetch` path previously ran **every** fetch through an LLM classifier. The
"known-good" host seed and the bundled reputation popularity list (change #0045) were only *bias
hints* fed into that classifier prompt — nothing in the system could approve a fetch without a model
call.

That made the probabilistic layer the bottleneck on routine developer work. Real sessions produced
32 classifier denials of entirely ordinary hosts: `web.archive.org`, `hn.algolia.com`, duckduckgo,
bing, google. Every one of those was a correct-by-policy read that a competing harness (Claude Code,
Cursor) performs without a prompt or a token spend.

Change #0068 (auto-mode overhaul stage B) merged a **catastrophic-deny floor**, so safety no longer
depends on the classifier being pessimistic — the deterministic layers below it already refuse the
things that must never happen. That removed the reason to route benign hosts through a judge.

Change #0069 therefore promoted the strong seed and `reputation.KnownGood` to a **real auto-approve**:
`DecidedBy "known-good"`, zero classifier calls, no human prompt.

The forcing question this ADR answers: promoting a data-backed host set from a classifier *hint* to
an *authorization source* changes what that data file **is**. A hint that is slightly wrong biases a
judge; an authorization source that is slightly wrong grants access. The seed and the popularity CSV
were curated as the former and are now consumed as the latter.

## Decision

The known-good promotion is an authorization boundary, and it carries **explicit bounds in code**
rather than relying on the underlying data being well-curated. Six rules, as built:

1. **Only the STRONG seed auto-approves.** Broad TLD wildcards (`*.gov`, `*.edu`, `*.dev`) and
   open-registration user-content namespaces (`*.github.io`, `*.readthedocs.io`) remain nudge-only
   hints. The admission criterion is **"the named operator controls the hostname NAMESPACE"**, not
   merely "the operator is reputable" — anyone can register `attacker.github.io`.

2. **Config always beats the seed.** The layer order is: SSRF → config `fetch_deny` → config
   `fetch_ask` → reputation blocklist → credentialed-URL → known-good promotion → fallthrough to the
   classifier. The promotion sits **strictly last** among the deciding layers: it can only *withhold*
   an allow, never grant one above a deny.

3. **The host is canonicalized ONCE, before every layer** — lowercase plus trailing-dot strip, via
   the shared `reputation.CanonicalHost`. Previously a trailing-dot host defeated `fetch_deny` while
   still matching `reputation.KnownGood`: a one-character mutation that converted a configured deny
   into a zero-classifier auto-approve.

4. **A hardcoded exfil-shape denylist is subtracted from the promotion** — paste/upload services,
   link shorteners, webhook and request-capture endpoints. A future data refresh therefore cannot
   promote an exfiltration endpoint even if it legitimately lands in the popularity CSV.

5. **The exact promoted host set is PINNED by a test.** Any refresh that widens authorization fails
   the suite, forcing a human to review the new hosts *as authorization grants*.

6. **Deterministic, machine-checkable URL properties are decided at the FLOOR, never delegated to
   the classifier.** The classifier prompt only ever sees the host, so a credential-bearing URL
   (`url.Userinfo` present) resolves to Ask with `DecidedBy "credentialed-url"` rather than being
   handed to a judge that cannot see the credential.

## Consequences

**Enables.** Claude Code / Cursor parity for routine public web reads: no model call, no prompt. The
observed class of 32 spurious denials disappears.

**Costs.** The popularity CSV and the seed lists are now **security-relevant data**. A routine data
refresh is an authorization change and must be reviewed as one; the pinned test (rule 5) is the
mechanism that enforces this rather than trusting the reviewer to notice.

**Given up.** The classifier no longer sees known-good traffic *at all*, so it can no longer catch a
compromised-but-reputable host. This is accepted deliberately: the SSRF floor, the reputation
blocklist, the exfil-shape denylist, and config `fetch_deny` all remain above the promotion.

**Known residual limitation.** The floor authorizes the **requested** host only. The HTTP client in
`internal/research` sets no `CheckRedirect`, so a known-good host that redirects elsewhere is fetched
without the floor re-deciding the new host. Per-hop re-checking is follow-up work outside #0069's
scope; the limitation is documented in `classifyFetchHost`'s doc comment.
