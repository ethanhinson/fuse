---
slug: smoke-over-fake-backend-proves-wire-not-system
hook: "A cross-language smoke test (real generated client stub → real server handler) run against a FAKE/scripted backend proves the WIRE serializes and round-trips, NOT that the system works end-to-end — and if it t.Skips silently when its toolchain is absent, a green suite hides that the path never ran. Keep the rigorous property test (no-loss/no-dup, real lifecycle) on the authoritative side, run the smoke against the real backend for at least one acceptance, and make the skip loud."
topics: [testing, integration, streaming, cross-language, ci]
changes: [55, 50, 49, 56]
created: 2026-08-11
updated: 2026-08-11
promotion_state: candidate
promoted_to:
---

## Apply

When a change adds a cross-language client (e.g. a `connect-es` TS stub over a `connect-go` server),
the natural first test is a "smoke" that drives the **real generated stub** against the **real server
handler**. That is genuinely valuable — it proves the wire compiles, serializes, and round-trips
across the language boundary. But it is easy to overclaim what it covers, in three specific ways that
all read as "covered" when they are not:

1. **The backend is fake.** If the server sits on a scripted/`fakeRuntime` that emits canned events,
   the smoke proves *transport plumbing*, not the *system*: no real engine, no real lifecycle, no real
   completion. "The client does something" is not "a real request flowed through the whole stack."
   Keep the fake for a fast wire check, but at least one acceptance must drive the **real backend**
   end-to-end.

2. **The hard property is asserted only on one side.** The rigorous invariant (here: no-loss/no-dup
   across the subscribe→replay gap — see [[replay-live-handoff-dedup-at-watermark]]) often lives in the
   authoritative-language tests (Go), while the cross-language smoke does a *thin* version (read one
   frame, check `seq > resumeFrom`). That does not prove the property from the client's side. If the
   client is the thing users run, prove the hard property *from the client*, not just server-side.

3. **A silent skip hides an unexercised path.** A smoke gated on an optional toolchain
   (`t.Skip` when `node_modules` / `node` is absent) lets `go test ./...` pass **green without ever
   running the cross-language path**. "N packages, 0 fail" then means nothing about that path. Make the
   skip **loud** — a tracked/required CI lane, or a hard fail in the environment that is supposed to
   have the toolchain — so green cannot mask a skipped test.

**Scope boundary that travels with this:** proving the transport (server + authoritative-side
resilience tests) can legitimately be a *different change* from proving the end-to-end system through
the client SDK. That is fine — but record the deferred end-to-end proof as an explicit **acceptance
requirement on the SDK change**, or it silently evaporates behind an optimistic build summary. (Here:
#55 shipped the transport + Go resilience tests; #50 owns the real-loop, real-browser, loud-CI proof.)

### Provenance

Surfaced from human review of change #55's `docket-implement-next` build (PR #52): the agent's run
report described a `connect-es` smoke as end-to-end proof, but it ran against a `fakeRuntime`, checked
reconnect with a single frame, and `t.Skip`ped without the node toolchain. The transport itself and
its Go-side resilience tests were sound; the overclaim was the lesson. Recorded as a #50 acceptance
requirement rather than a #55 defect.

## War story — 2026-08-11 (#49, PR #53)

The auth/multi-tenancy build ran a **live** Connect-wire acceptance (auth / spoof-tenant /
cross-owner-reject / reconnect-no-dup, real transport against a scripted gateway). The one path it
did **not** cover end-to-end was the two-instance **re-own** story ("redeploy, then re-`Observe`
from your phone with the same token"): a true second-instance-death simulation over the live wire is
heavy, so it was covered at the **runtime unit layer** (`internal/runtime/lease_test.go:
TestResolveReOwnsExpiredLease` and siblings), not as a `cmd/fuse` acceptance test.

The build did the right thing per this finding: rather than let an optimistic "all green" summary
imply the re-own story was proven end-to-end, it **recorded the deferred proof as an explicit
acceptance item** — a `## Verify (human)` checkbox in the results file with the exact manual
reproduction (start a loop, kill the owner, start a fresh instance, re-`Observe(from_seq)` with the
same bearer, confirm resume). That is this finding's "record the deferred end-to-end proof as an
explicit acceptance requirement, or it evaporates behind an optimistic build summary" rule applied
**within one change** (a `## Verify` checkbox) rather than as a hand-off to a downstream change (the
#55→#50 case above). Both are valid discharges; the invariant is that the gap is written down where
a human will act on it, never left implicit in a green suite.

## War story — 2026-08-12 (#56, PR #57)

The SDK dogfood change made the client-side proof permanent: Wander's browser lane drives the real
`@fuse/sdk` against a real `connect-go` loop with a scripted gateway, cuts the live Observe stream,
and asserts reconnect with strictly increasing sequence numbers and no duplicate events. Missing
node, esbuild, Go, or Playwright tooling is a hard failure rather than a green skip, so the lane
cannot quietly prove only a fake wire path.
