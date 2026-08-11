---
slug: smoke-over-fake-backend-proves-wire-not-system
hook: "A cross-language smoke test (real generated client stub → real server handler) run against a FAKE/scripted backend proves the WIRE serializes and round-trips, NOT that the system works end-to-end — and if it t.Skips silently when its toolchain is absent, a green suite hides that the path never ran. Keep the rigorous property test (no-loss/no-dup, real lifecycle) on the authoritative side, run the smoke against the real backend for at least one acceptance, and make the skip loud."
topics: [testing, integration, streaming, cross-language, ci]
changes: [55, 50]
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
