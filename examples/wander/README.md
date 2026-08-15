# Wander — the `@fuse/sdk` vacation-rental concierge demo

Wander is a plain HTML/CSS/JS single-page **vacation-rental concierge** built to *dogfood*
[`@fuse/sdk`](../../sdk/ts) — the remote TypeScript/JS client for the `fuse.loop.v1` Connect
wire. It was the forcing function for change 0056: building it surfaced (and drove the fix
for) the SDK's connection-state, terminal-error, and teardown gaps. Wander drives the loop
**entirely through the SDK's public API** — it never touches the generated Connect stubs.

![screenshot](./screenshot.png)

> **This is the one concierge demo.** There used to be two: `examples/concierge-demo` (a
> nicer UI over a hand-rolled `loop.*` JSON-RPC **WebSocket relay** plus a
> `GET /loops/{id}/events` replay proxy — the pre-#55 wire, with no CI lane) and this one
> (a plainer UI over the real SDK, carrying the only browser acceptance lane). Change 0060
> consolidated them **onto this base**: the concierge UI moved here and
> `examples/concierge-demo` was deleted, WS relay and all. The direction is deliberate and
> one-way — **the look may move, the transport never moves backwards.** Anything that wants
> to talk to a fuse loop from a browser uses `@fuse/sdk` over Connect.

## What it exercises (the SDK surface Wander dogfoods)

| Wander behavior | `@fuse/sdk` API |
| --- | --- |
| Start a persistent concierge on the first message | `createClient(...).startLoop({ interactive: true })` |
| Inject each subsequent user turn | `client.send(loopId, text)` |
| Stream the reply incrementally, and every step into the activity rail | `client.observe(loopId, { fromSeq })` |
| Know when the concierge finished a turn (never inferred from stream shape) | `isCompletion(ev)` |
| Render a live connection indicator (live / reconnecting / …) | `observe(loopId, { onState })` — **0056, D1** |
| Show the right affordance on auth-rejected / gone loop instead of hot-looping | `catch (e) { if (e instanceof FuseTerminalError) … }` — **0056, D2** |
| Tear the stream down on page unload without leaking it | `observe(loopId, { signal })` + `AbortController` — **0056, D3** |

Transparent reconnect with **no-loss / no-dup** across a mid-stream network drop is the
SDK's job — it re-observes from the last-seen seq and dedups at the watermark. Wander just
renders; the reconnect property is enforced by the headless-browser CI lane (see below).

## The UI

- **Activity rail** — every observed event kind that isn't the answer itself streams into
  the left rail as a labelled line: searches (`search_rentals` / `web_search`), listing
  reads, saved listings, dispatched scouts, context compaction, loop errors. The stat strip
  underneath counts turns / searches / agents / events for the session.
- **Thread** — the user's messages and the concierge's replies. A reply bubble opens with a
  spinner + phase cue the moment a turn starts, fills in from `model.delta` as tokens
  stream, and is re-rendered at `loop.parked` as lightly-formatted markdown, with rental
  cards pulled out when the answer uses the `**Name** — price — why — url` shape. The
  markdown is HTML-escaped **before** any inline pattern is applied, so model output can
  never inject markup.
- **Link grounding** — every URL that actually arrives in a `tool.result` (or a scout's
  `spawn.done`) is recorded, and each card link is cross-checked against that set. A link
  the model invented renders as `⚠︎ unverified link`, not as a clickable listing. A
  concierge that presents hallucinated booking links as real ones demos the opposite of the
  point, so this check is load-bearing, not decoration.
- **Composer** — suggestion chips plus the input. The whole composer is disabled only while
  a turn is in flight and re-enabled at each park; it is never disabled in the markup.
- **＋ New** reloads the page. Wander is stateless across page loads by design (see
  *Scope*), so a reload is the honest reset — it tears the live observe stream down through
  the same abort path as any navigation instead of hand-rolling a second teardown route.

The page loads **no remote assets** (no webfont CDN, no framework): the browser acceptance
lane navigates with `networkidle` against a hermetic backend, and a third-party fetch would
make the repo's only browser lane depend on public network egress. `styles.css` names
Fraunces/Inter first and falls back to the system serif/sans stack.

## Architecture — the SDK speaks Connect directly (no WS relay)

```
browser  ──(connect-web, same origin)──►  server.js  ──(HTTP reverse-proxy)──►  fuse loop-serve-net
  app.js → @fuse/sdk (vendor/fuse-sdk.js)      static + /fuse.loop.v1.* proxy         Connect/protobuf
```

`server.js` runs **no WebSocket relay and re-implements no protocol**. It only:

1. serves the static page + the esbuild-bundled SDK (`vendor/fuse-sdk.js`), and
2. **transparently reverse-proxies** the Connect service path `/fuse.loop.v1.*` to
   `fuse loop-serve-net`.

The proxy exists purely to keep the browser **same-origin** (so there is no CORS): the SDK
still drives the real Connect wire end-to-end. (`fuse loop-serve-net` serves no CORS headers
today — the same-origin reverse proxy is the friction-free browser path. This was one of the
rough edges building Wander surfaced; see the change results file.)

It also exposes one test-only control, `/__cut`, which forcibly destroys every in-flight
proxied Connect socket. That is how the browser lane stages a deterministic mid-stream
network kill; nothing calls it in normal use.

## Run it

```sh
# from the repo root, once:
npm install                 # installs esbuild (used by build.sh)

# then:
./examples/wander/run.sh    # builds the SDK bundle, starts loop-serve-net + the page
# open http://localhost:5173
```

`run.sh` points the loop server at whatever model gateway your `~/.fuse/config.yml`
configures. To run **fully offline / deterministic** (no provider — the project policy for
tests is to NEVER call Claude/Anthropic), set `LLM_GATEWAY_URL` to a scripted double before
launching; the headless-browser CI lane does exactly this.

Environment knobs: `PORT` (static server, default `5173`), `FUSE_NET_ADDR` (backend,
default `127.0.0.1:8787`).

`fuse.demo.yml` in this directory is the checked-in demo config for the rentals MCP server
and its demo user directory (change 0060). Its tokens are obviously fake and **demo-only**.

## Build the SDK bundle only

```sh
./examples/wander/build.sh   # → examples/wander/vendor/fuse-sdk.js (git-ignored artifact)
```

`build.sh` runs esbuild over `sdk/ts/src/index.ts`, inlining `@connectrpc/connect-web` and
`@bufbuild/protobuf`, so `index.html` imports the **real published SDK surface**, not
relative proto stubs.

## Scope

Wander is **stateless across page loads** — a refresh starts a fresh session. That is
deliberate: it demonstrates a *live, reconnecting* session without needing durable/resumable
sessions (change #54). It is an example app, not a production deployment.

## Acceptance / test

An example app has no unit test of its own; its acceptance is the permanent
**headless-browser reconnect CI lane** — `browser_test.go` (`//go:build browser`), run as
`make browser-test` / `go test -tags browser -timeout 300s ./examples/wander/...`, and
enforced by the `browser-acceptance` job in `.github/workflows/integration.yml`. It serves
this page, drives a concierge turn in headless chromium against a real `loop-serve-net` with
a scripted gateway double, kills the network mid-stream via `/__cut`, and asserts the reply
still completes with **no loss / no dup** and that the SDK went `reconnecting` → `live`.

This is the repo's **only** browser acceptance lane. Changing the markup here can silently
break it: `browser_test.go` drives `#input` and `#send` and reads `.msg.concierge` /
`.msg.error` plus the `window.__wander*` instrumentation `app.js` publishes. Keep those, and
run the lane before and after any UI change.
