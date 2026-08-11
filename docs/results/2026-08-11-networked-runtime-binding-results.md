<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0048 — Networked binding over the Runtime seam — WS live observe + HTTP start/send/replay](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0048-networked-runtime-binding.md)**
<!-- docket:backlink:end -->

# Networked binding over the Runtime seam (binding #3) — results

Change: #48 · Branch: feat/networked-runtime-binding · PR: <set at PR open> · Plan: docs/superpowers/plans/2026-08-11-networked-runtime-binding.md · ADRs: 0032

## Verify (human)

Automated `go test ./... -race` (all 25 packages green, WS tests stressed 30x, no data races) is the
primary receipt. Optional manual sanity check at the merge gate — NOT required for correctness:

- [ ] Start the server: `go run ./cmd/fuse loop-serve-net --addr 127.0.0.1:8787` (auto-approve binding; needs a model gateway only if you actually start a loop).
- [ ] If starting a real loop, point `LLM_GATEWAY_URL` at a cheap NON-Anthropic gateway model (never Claude/Anthropic/Fable/Opus/Sonnet/Haiku — project policy).
- [ ] Attach a WS client, drive `loop.start` + `loop.observe`, confirm a live `loop.event` tail.
- [ ] `GET http://127.0.0.1:8787/loops/{id}/events?from=0` returns the durable history JSON array.

## Findings

- **Same seam, three transports — proven.** Binding #3 drives the identical `runtime.Runtime` over a
  WS transport with **zero change** to the interface (`internal/runtime`, `internal/event`, and
  `internal/mcp` are byte-untouched vs `origin/main`). The `internal/loopserver` core was extracted
  into a transport-agnostic `conn` abstraction (`transport.go`); stdio (`stdio.go`) and WS (`net.go`)
  are two transports over one `dispatch`/`serveObserve` — the subscribe-before-replay +
  dedup-at-watermark + gap-marker discipline is inherited verbatim, not reimplemented. Recorded as
  **ADR-0032**.
- **Review Major (fixed).** The initial WS transport's `readRequest` under-matched the error classes
  `coder/websocket` returns on abnormal peer close (TCP RST / no close frame → raw `io.ErrUnexpectedEOF`
  or `*net.OpError`, never a `websocket.CloseError`) and on a self-`Close` race (`net.ErrClosed`), so a
  routine client drop surfaced as a server error. Fixed: every post-handshake read error now maps to a
  clean `io.EOF` (a WS conn cannot resume after a read error), with `TestWSAbnormalCloseIsCleanShutdown`
  (drives `conn.CloseNow()`) and `TestWSMidSessionCancelIsCleanShutdown`. No goroutine/subscription
  leak either way — the observe pump's `defer cancel()` fires on any return.
- **Concurrent-reattach dedup verified over the real WS transport.** `TestWS…` forces an append into
  the subscribe→replay gap concurrently (a gated fake Runtime), disconnects, re-observes from the
  watermark, and asserts no dup / no loss / exactly one `gap:true` frame — mutation-proven (breaking
  `ev.Seq <= last` turns it red). Per the `replay-live-handoff-dedup-at-watermark` learning, a
  sequential test cannot see the double-delivery.

## Follow-ups

Deferred to their own changes (auto-capture is disabled, so these are reported, not minted):

- **HTTP replay error-detail leak (review Minor).** `GET /loops/{id}/events` returns raw `Attach`
  error strings and uses `coder/websocket`'s default same-origin `CheckOrigin`. Acceptable at this
  pre-auth stage; harden (generic error bodies, origin policy) alongside **#0049** (auth / identity),
  which is the natural home for endpoint hardening.
- **Cross-origin browser clients** would get a 403 under the default same-origin check — fine for the
  current CLI/programmatic client scope; if a browser UI on a different host is wanted, wire
  `OriginPatterns` (also #0049/#0050 territory).

## Notes / deviations

- **Structuring (spec open q1):** the extracted core stayed **inside** `internal/loopserver`
  (`transport.go` + `stdio.go` + `net.go`), `NewServer` signature preserved — least churn, binding #2
  behaviorally identical. No sibling package was needed.
- **Merge-gate note:** the primary working tree at `/Users/ethanhinson/dev/fuse` sits on `main` and was
  behind `origin/main` (which has #47's tenant-threaded seam merged) during this build. The feature
  branch was correctly cut from `origin/main` (tip `d6d3e77`), so the code is against the current seam;
  no action needed, just don't be misled by a stale primary checkout.
- **Skill degradations (per-machine, expected):** `superpowers:*` role skills are unavailable in this
  harness, so plan/build/review/finish degraded to auto per the missing-skill rule — plan authored
  inline; build ran via docket's own profile agents (`docket-build-standard`/`-premium`) under the
  `docket-build-task` contract (TDD + per-task self-review); review ran as a whole-branch dispatched
  review; finish via the auto fallback. This is expected here, not a fault.
