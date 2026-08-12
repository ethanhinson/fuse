<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0056 — SDK viability hardening — dogfood @fuse/sdk by building Wander, fix what blocks a real web app](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0056-sdk-viability-hardening-wander.md)**
<!-- docket:backlink:end -->

# Plan — SDK viability hardening via Wander (change 0056)

Spec: `docs/superpowers/specs/2026-08-11-sdk-viability-hardening-wander-design.md` (on `docket`).
Reconciled against `origin/main` @ `b7cebf4` (2026-08-11).

> **Plan-skill note:** `superpowers:writing-plans` is not invocable in this harness, so the plan
> role degraded to `auto` (agent-authored). Expected per project memory ("superpowers:* skills
> unavailable"), not a fault. Same degrade will apply to build (§5) and review (§6).

## Goal

Dogfood `@fuse/sdk` by building **Wander** (a plain HTML/CSS/JS vacation-rental concierge over
`@connectrpc/connect-web`), fix the SDK gaps building it surfaces, and convert the *deferred manual*
real-browser reconnect proof (recorded as a checkbox in `sdk/ts/README.md` "Verify (human)") into a
**permanent, loud headless-browser CI lane**. The deliverable is the SDK hardening + the CI lane +
the example app; Wander is the forcing function.

## Current-state anchors (verified against origin/main)

- **TS SDK** `sdk/ts/src/index.ts` — `createClient({baseUrl, credentials:{token,tenant}, transport?})`
  → `startLoop / send / observe / isCompletion / KIND_LOOP_PARKED`. `observe(loopId, fromSeq?)` is an
  async iterable that reconnects internally (exp backoff cap 30s), dedups at watermark, surfaces `gap`.
  **Gaps (spec must-haves, all confirmed real):** (1) reconnect state is never surfaced; (2) the
  reconnect loop's `catch {}` swallows *every* error — a **terminal** condition (auth rejected, loop
  finished) hot-loops forever instead of stopping; (3) no explicit/idempotent teardown beyond breaking
  the `for await` (no `close()` / AbortSignal for page-unload).
- **Wire** `proto/fuse/loop/v1/loop.proto` — `StartLoop`/`Send` unary, `Observe(from_seq)` server-stream,
  completion = explicit `loop.parked` event kind. No wire work in scope.
- **Test harnesses:** `sdk/ts/test/server/main.go` (scripted `fakeRuntime`, prints `URL <url>`,
  forces a subscribe→replay overlap at seq 3) for the pure-wire/dedup slice; `sdk/fuse/acceptance_test.go`
  for the real engine (build `cmd/fuse`, exec `fuse loop-serve-net`, scripted `LLM_GATEWAY_URL`, dev
  token `fuse-dev-token` → tenant `_default`). Root npm workspace `package.json` = `[proto, sdk/ts]`.
- **CI** `.github/workflows/integration.yml` — `unit-race` (Go) + `mcp-integration` (Docker +
  **playwright-go chromium already installed**). `make sdk-ts-test` runs the node lane, loud on node
  absence. **No browser lane over `@fuse/sdk` yet.**
- **`examples/concierge-demo/`** is a *prior*, WS-era (binding #3, raw `loop.*` JSON-RPC) concierge that
  does NOT use `@fuse/sdk` — Wander (`examples/wander/`) is genuinely additive; leave the old demo alone.

## Design decisions (settled here, from the spec's open questions)

- **D1 — Connection-state surface.** Add an observed connection state enum
  `connecting | live | reconnecting | closed` exposed *alongside* the existing `observe` async-iterable
  (do NOT break the iterable surface #50 shipped and the node test asserts). Shape: `observe` gains an
  options object `observe(loopId, { fromSeq?, signal?, onState? })` where `onState?: (s: ConnState) => void`
  is a lifecycle callback; `ConnState` is exported. Backward-compatible: `observe(loopId, fromSeq?)`
  (bigint positional) still works via an overload/union so the existing node test is untouched.
- **D2 — Transient vs terminal error classification.** Port
  `websocket-read-errors-are-not-closeerror` to the Connect-stream shape: an abnormal mid-stream drop
  (network error, stream end) = **transient** → reconnect (current behavior). A **terminal** Connect
  code — `unauthenticated` / `permission_denied` (auth), and a loop-finished/`not_found` signal — must
  **stop the reconnect loop** and surface a typed terminal error (throw a `FuseTerminalError` out of the
  async iterator, and fire `onState('closed')`), instead of the current infinite hot-loop. Distinguish
  via `ConnectError.code` from `@connectrpc/connect`.
- **D3 — Idempotent teardown.** `observe` accepts an `AbortSignal` (D1's `signal`); aborting it stops
  the loop, tears the underlying stream down, fires `onState('closed')`, and is idempotent (double-abort
  / abort-after-close is a no-op). This is the page-unload / component-teardown primitive Wander needs.
- **D4 — Browser lane harness.** Reuse `playwright-go` (already a CI dep) driving **headless chromium**
  against a static-served Wander page that imports the *built* `@fuse/sdk` over `@connectrpc/connect-web`,
  pointed at a **real** `fuse loop-serve-net` backend with a scripted `LLM_GATEWAY_URL` double (never
  Claude). The lane kills the network mid-stream (Playwright route abort / offline) and asserts the
  concierge reply still completes with no-loss/no-dup. **Loud on toolchain absence** (no green skip) per
  `smoke-over-fake-backend-proves-wire-not-system`. Implemented as a Go test (`//go:build browser` tag)
  so it rides the existing Go CI toolchain, mirroring `sdk/fuse/acceptance_test.go`'s subprocess pattern.

## Tasks (TDD; each = failing test → implement → green → self-review → one commit)

### Task 1 — SDK: connection-state surface + lifecycle callback (D1)
- **Test first** (`sdk/ts/test/state.test.ts`, node:test, real Go `test/server`): drive
  `observe(loopId, { onState })`; assert the callback observes `connecting → live` on first frame, and
  `reconnecting → live` across the forced stream re-open. Assert the **positional** `observe(loopId, 0n)`
  form still yields events (back-compat).
- **Implement:** export `type ConnState = "connecting" | "live" | "reconnecting" | "closed"`; add the
  options-object overload; fire `onState` at each transition inside the reconnect loop (set `live` on
  first delivered frame, `reconnecting` before a re-open sleep, `connecting` at start).
- Commit: `feat(sdk/ts): #56 surface connection state + lifecycle callback on observe`.

### Task 2 — SDK: transient-vs-terminal error classification (D2)
- **Test first:** extend `test/server/main.go` (or a sibling scripted server) to return a **terminal**
  Connect error (`unauthenticated`) on Observe, and (separately) a normal stream end → transient. Test:
  a terminal code makes `observe` **throw a typed `FuseTerminalError`** and fire `onState('closed')`
  (loop stops — assert it does NOT spin); a transient drop reconnects (existing behavior preserved).
  Guard the reconnect-does-not-hot-spin property with a bounded attempt count in the test.
- **Implement:** classify the caught error by `ConnectError.code`; terminal set = `Unauthenticated`,
  `PermissionDenied`, plus the loop-finished signal (`FailedPrecondition`/`NotFound` per handler — verify
  against `internal/loopconnect` mapping). Terminal → throw + `closed`; else → reconnect as today.
  Export `FuseTerminalError` (carries the code). Reference learning
  `websocket-read-errors-are-not-closeerror` in a code comment (ported to Connect shape).
- Commit: `fix(sdk/ts): #56 stop reconnect + surface typed terminal error on terminal Connect codes`.

### Task 3 — SDK: idempotent AbortSignal teardown (D3)
- **Test first:** start `observe(loopId, { signal })`, consume one event, `abort()`; assert the async
  iterator returns (loop exits), `onState('closed')` fired exactly once, and a second `abort()` is a
  no-op. Assert no dangling stream (best-effort: the server-side `func()` cleanup path is hit).
- **Implement:** thread `signal` into the reconnect loop + the underlying `wire.observe` call; on abort,
  break the loop, run the stream `finally` teardown, fire `closed` once (guard with a `closed` flag).
- Commit: `feat(sdk/ts): #56 idempotent AbortSignal teardown for observe`.

### Task 4 — Wander example app (`examples/wander/`)
- **Build:** `index.html` + `styles.css` + `app.js` (plain, no framework) importing the **built**
  `@fuse/sdk` (bundled/ESM from the workspace) over `@connectrpc/connect-web`; a small static server
  (mirror `examples/concierge-demo/server.js`'s dependency-free static-serve, minus the WS relay — the
  SDK speaks Connect directly, no proxy per the settled #55 browser path) + a `run.sh` that starts
  `fuse loop-serve-net` and serves the page. Concierge chat: `startLoop(interactive:true)` on first
  message, `send` per turn, `observe` for streamed replies, completion via `isCompletion`
  (`persistent-loop-needs-explicit-completion-event`), a visible connection-state indicator wired to
  Task-1's `onState`, and page-unload teardown via Task-3's abort. Stateless across page loads
  (holds the #54 boundary). `README.md` documenting run steps + which SDK features it exercises.
- **Acceptance for this task is the Task-5 browser lane** (an example app has no unit test of its own);
  keep `app.js` thin and driven entirely through `@fuse/sdk`'s public API (no generated-stub access).
- Commit: `feat(examples): #56 Wander — SDK-driven concierge demo over connect-web`.

### Task 5 — Headless-browser reconnect CI lane (D4)
- **Test first** (`sdk/ts/browser/browser_test.go` or `examples/wander/browser_test.go`, `//go:build browser`):
  a `playwright-go` test that (a) builds `@fuse/sdk` + serves Wander, (b) starts `fuse loop-serve-net`
  with a scripted `LLM_GATEWAY_URL` double (PLAIN JSON tool args per
  `scripted-gateway-double-double-escapes-tool-args`; never Claude), (c) drives a concierge turn in
  headless chromium, (d) **kills the network mid-stream** (Playwright `context.SetOffline`/route abort),
  (e) asserts the reply completes after transparent resume with **no-loss/no-dup** (assert on the
  rendered transcript / a page-exposed seq log). **Loud** on missing chromium/node
  (`smoke-over-fake-backend-proves-wire-not-system`): a `command -v` / `playwright install` guard that
  **fails**, never `t.Skip`s green. Use `t.Cleanup` (not `defer srv.Close()`) for all teardown ordering
  per `httptest-defer-close-before-tcleanup-deadlock`.
- **Wire into CI:** add a `browser-acceptance` job (or extend `mcp-integration`, which already installs
  playwright-go chromium) running `go test -tags browser ./...` for the lane; add a `make browser-test`
  target. Update `sdk/ts/README.md` "Verify (human)" — the deferred manual checkbox is now the enforced
  CI lane (cross-reference it).
- Commit: `test(ci): #56 permanent headless-browser reconnect lane for @fuse/sdk (no-loss/no-dup)`.

### Task 6 — Results file: record every SDK bug/rough-edge Wander surfaced
- Author `docs/results/2026-08-11-sdk-viability-hardening-wander-results.md` (feature-branch build
  artifact) listing each SDK fix with the Wander interaction that surfaced it (spec acceptance-4), the
  browser-lane manual/human verification checklist (if any beyond CI), and any plan deviations /
  follow-ups discovered. (This satisfies Step-6.5 conditions (b)+(c).)
- Commit: `docs(results): #56 SDK hardening findings + Wander-surfaced fixes`.

## Test strategy / gates

- Go: `go test -race ./...` stays green (SDK Go side untouched except possibly error-code mapping refs).
- TS: `make sdk-ts-test` green (existing node lane + Tasks 1–3 new node tests).
- Browser: `go test -tags browser ./...` green; loud on toolchain absence.
- **No Claude/Anthropic traffic anywhere** — every live-loop test uses the scripted `LLM_GATEWAY_URL`
  double + dev token (project policy).

## Risks / notes

- **Back-compat of `observe`:** the node test (`sdk/ts/test/remote.test.ts`) calls
  `observe(loopId, fromSeq)` positionally — Task 1's options-object form MUST be additive (union/overload),
  never a breaking signature change. This is a hard gate: run `make sdk-ts-test` after Task 1.
- **Terminal-code set (D2):** verify the exact Connect codes `internal/loopconnect` emits for
  auth-reject and loop-finished before finalizing the terminal set (read the handler, don't guess).
- **Browser lane weight:** if a full `playwright-go` + network-kill proves flaky in CI, the fallback is
  a deterministic in-page seq-log assertion over an offline/online toggle rather than a real packet drop
  — still a real browser, still loud, still asserting no-loss/no-dup (record any such descope in results).
- **Bundling `@fuse/sdk` for the browser:** the SDK imports the proto stubs by relative path; Wander may
  need a tiny build step (esbuild/tsc) to produce a browser bundle. Prefer the lightest path that keeps
  Wander driving the *real published surface*, not relative imports (per #50's monorepo rationale).
