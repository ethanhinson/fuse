<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0056 — SDK viability hardening — dogfood @fuse/sdk by building Wander, fix what blocks a real web app](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0056-sdk-viability-hardening-wander.md)**
<!-- docket:backlink:end -->

# Results — SDK viability hardening via Wander (change 0056)

Spec: `docs/superpowers/specs/2026-08-11-sdk-viability-hardening-wander-design.md`
Plan: `docs/superpowers/plans/2026-08-11-sdk-viability-hardening-wander-plan.md`

Dogfooding `@fuse/sdk` by building **Wander** (a plain HTML/CSS/JS vacation-rental concierge
over `@connectrpc/connect-web`) surfaced three SDK gaps that a real browser app cannot work
around, plus one browser-integration rough edge. Each was fixed with a test, each is consumed
by Wander, and the deferred manual real-browser reconnect proof is now a permanent, loud
headless-browser CI lane.

## SDK fixes, and the Wander interaction that surfaced each

### 1. Connection state was never surfaced (D1) — `feat(sdk/ts): surface connection state + lifecycle callback on observe`

- **Surfacing interaction.** Wander's header has a live connection indicator (live /
  reconnecting / …). Wiring it, there was **no way to know the SDK was reconnecting** — the
  `observe` async-iterable exposed events but not lifecycle. An app would have to reach into
  the transport (defeating the SDK) to render a connection dot.
- **Fix.** Added an exported `type ConnState = "connecting" | "live" | "reconnecting" |
  "closed"` and an additive options form `observe(loopId, { fromSeq?, onState?, signal? })`.
  `onState` fires at each transition inside the reconnect loop. The positional
  `observe(loopId, fromSeq?)` form is preserved (union/overload) so the #50 node test is
  untouched — back-compat verified by `make sdk-ts-test` after the change.
- **Test.** `sdk/ts/test/state.test.ts` drives a forced stream re-open (`statesrv` MODE=reopen)
  and asserts `connecting → live → reconnecting → live`, plus a positional-form back-compat
  test.
- **Consumed by Wander.** `app.js` passes `onState: (s) => setConn(s)`; the header dot is
  purely SDK-driven.

### 2. Every stream error was swallowed → a terminal condition hot-looped forever (D2) — `fix(sdk/ts): stop reconnect + surface typed terminal error on terminal Connect codes`

- **Surfacing interaction.** Testing Wander against a misconfigured token (auth rejected), the
  reconnect loop's `catch {}` swallowed the `unauthenticated` Connect error and **re-opened
  Observe forever** — an invisible hot-loop hammering the server, with no way for the app to
  show "session closed, refresh". A finished/unknown loop behaved the same.
- **Fix.** Ported `websocket-read-errors-are-not-closeerror` to the Connect-stream shape:
  classify the caught error by `ConnectError.code`. The **terminal set** —
  `Unauthenticated`, `PermissionDenied`, `NotFound`, `FailedPrecondition` (verified against
  `internal/loopconnect/handler.go` + `observe.go`: the exact codes the loop handler emits for
  auth-reject / tenant-spoof / cross-owner / unknown-loop / finished-loop) — **stops** the
  reconnect loop and throws an exported `FuseTerminalError` (carrying the `code`), firing
  `onState("closed")`. Everything else stays transient → reconnect (the #50 behavior).
- **Test.** `sdk/ts/test/error.test.ts` asserts, for each of the four terminal codes, that
  `observe` throws `FuseTerminalError`, fires `closed`, and does **not** spin (no
  `reconnecting` in the state log); a separate case proves a transient stream end still
  reconnects to completion.
- **Consumed by Wander.** `app.js`'s `catch (err) { if (err instanceof FuseTerminalError) … }`
  shows a "Connection closed (code). Refresh…" affordance instead of hot-looping.

### 3. No explicit, idempotent teardown for page-unload (D3) — `feat(sdk/ts): idempotent AbortSignal teardown for observe`

- **Surfacing interaction.** A browser app must release the observe stream on navigation /
  component teardown or it **leaks a stream per page load**. The only stop mechanism was
  breaking the `for await`, which a `pagehide` handler cannot cleanly reach from outside the
  loop.
- **Fix.** `observe`'s options accept an `AbortSignal` (`signal`), threaded into the reconnect
  loop and the underlying `wire.observe` call. Aborting stops the loop, tears the stream down,
  and fires `onState("closed")` exactly once; a pre-aborted signal closes immediately with no
  events; double-abort / abort-after-close is a no-op.
- **Test.** `sdk/ts/test/abort.test.ts` consumes one event, aborts, and asserts the iterator
  returns, `closed` fires exactly once, a second abort is a no-op; plus a pre-aborted-signal
  case.
- **Consumed by Wander.** `window.addEventListener("pagehide", () => pageAbort.abort())` with
  one `AbortController` per page lifetime.

### 4. Browser CORS — an integration rough edge (recorded, not an SDK code change)

- **Surfacing interaction.** Pointing Wander's connect-web transport straight at
  `fuse loop-serve-net` from a page served on a different port triggers a **CORS preflight**:
  `loop-serve-net` serves **no CORS headers** (verified — `cmd/fuse/loop_serve_net.go` /
  `internal/loopconnect` set none). The SDK is fine; the browser blocks the cross-origin call.
- **Resolution (no SDK change; the SDK still speaks Connect directly).** Wander's dependency-
  free `server.js` **reverse-proxies** the Connect service path `/fuse.loop.v1.*` to the
  backend so the browser stays **same-origin** (no CORS). This is a *transparent HTTP forward*,
  **not** a WebSocket relay and **not** a protocol re-implementation (unlike the older
  `examples/concierge-demo`) — the SDK drives the real Connect wire end-to-end. Server-
  streaming (Observe) responses are piped un-buffered so the live tail streams frame-by-frame.
- **Follow-up (reported, not minted — auto-capture disabled):** consider adding an **opt-in
  CORS allow-origin config** to `loop-serve-net` so a browser app can hit it directly without a
  same-origin proxy. Out of scope for this change; recorded here for the backlog.

## The permanent headless-browser reconnect CI lane (D4)

- **What it is.** `examples/wander/browser_test.go` (`//go:build browser`), run by
  `make browser-test` and the `browser-acceptance` CI job. It builds the `@fuse/sdk` browser
  bundle (esbuild), serves the real Wander page, starts a real `fuse loop-serve-net` with a
  **scripted `LLM_GATEWAY_URL` double (never Claude/Anthropic)**, drives a concierge turn in
  headless chromium (playwright-go), **kills the network mid-session**, and asserts the reply
  completes after a transparent reconnect with **no-loss/no-dup** (a strictly-increasing seq
  log across the drop) and that the SDK saw `reconnecting → live` (never a terminal error).
- **Loud on toolchain absence.** Missing node / esbuild / go / a playwright-installable
  chromium is a hard `t.Fatal`, never a green `t.Skip`
  (`smoke-over-fake-backend-proves-wire-not-system`). Teardown uses `t.Cleanup` ordered
  client-before-server (`httptest-defer-close-before-tcleanup-deadlock`).
- **Local run result.** PASS in ~3s against a local chromium (playwright-go cache present):
  the cut severed the open Observe stream, the SDK went `reconnecting → connecting`, re-observed
  from the watermark, and turn 2 completed — seq log `[1,2,3,4,5,…]` strictly increasing.

## Plan deviations / descopes

1. **Network-kill mechanism (documented fallback taken).** The plan's primary was a
   Playwright `context.SetOffline` / route-abort mid-stream drop; the Risks/notes documented a
   deterministic fallback if a real packet-drop proved flaky. In practice `SetOffline` did
   **not** reliably sever an already-established, idle (parked) streaming socket — the SDK's
   `for await` did not observe the drop within the test window. **Descope taken:** a small,
   deterministic `/__cut` control on Wander's own reverse proxy (`server.js`) forcibly destroys
   every in-flight proxied Connect socket — a **real** mid-stream network kill of the open
   Observe stream, still a real headless browser, still loud, still asserting no-loss/no-dup.
   This is strictly stronger than an offline toggle (it guarantees the open stream drops) and
   is fully deterministic. Recorded per the plan's Risks/notes allowance.

2. **Wander observe is one long-lived stream, not one-per-turn.** Building against the real
   interactive loop, the natural and correct shape is a **single** persistent `observe` for the
   whole session (one loop_id, one stream — matching `internal/runtime/interactive_test.go`'s
   "second turn.end on the SAME stream"), with `send` injecting input at the parked boundary.
   An initial per-turn re-observe design left no open stream to kill between turns; the single-
   stream design is both more correct and what makes the mid-stream cut testable. No plan text
   contradicted this; noted for clarity.

3. **SDK bundle is a git-ignored build artifact.** `examples/wander/vendor/fuse-sdk.js`
   (esbuild output) is generated by `build.sh` / `run.sh` / the CI lane and is **not**
   committed (`examples/wander/.gitignore`) — it is derived from `sdk/ts/src`. This keeps the
   example driving the real published SDK surface, not a stale checked-in bundle.

## Follow-ups noticed (reported only — auto-capture disabled, nothing minted)

- **Opt-in CORS for `loop-serve-net`** (see rough edge #4) so a browser app can target the
  Connect server directly without a same-origin reverse proxy.
- **Streaming assistant text in the demo.** The scripted gateway returns a non-streamed reply,
  so Wander renders the final `loop.parked` content; the `model.delta` incremental-render path
  in `app.js` is wired but only exercised against a streaming gateway. A streaming-gateway demo
  turn (or a scripted streaming double) would exercise it end-to-end.

## Gates

- `go build ./...` — green.
- `go test -race ./...` — green.
- `make sdk-ts-test` — green (10 node tests: the #50 no-loss/no-dup wire test + Tasks 1–3).
- `go test -tags browser ./...` — green (the headless-browser reconnect lane).
- **No Claude/Anthropic traffic anywhere** — every live-loop test uses the scripted
  `LLM_GATEWAY_URL` double + the built-in dev token (project policy).
