# Wander — an `@fuse/sdk` vacation-rental concierge (dogfood demo, change 0056)

Wander is a plain HTML/CSS/JS single-page **vacation-rental concierge** built to *dogfood*
[`@fuse/sdk`](../../sdk/ts) — the remote TypeScript/JS client for the `fuse.loop.v1` Connect
wire. It is the forcing function for change 0056: building it surfaced (and drove the fix
for) the SDK's connection-state, terminal-error, and teardown gaps. Wander drives the loop
**entirely through the SDK's public API** — it never touches the generated Connect stubs.

## What it exercises (the SDK surface Wander dogfoods)

| Wander behavior | `@fuse/sdk` API |
| --- | --- |
| Start a persistent concierge on the first message | `createClient(...).startLoop({ interactive: true })` |
| Inject each subsequent user turn | `client.send(loopId, text)` |
| Stream the reply incrementally (assistant text, tool "looking things up…") | `client.observe(loopId, { fromSeq })` |
| Know when the concierge finished a turn (never inferred from stream shape) | `isCompletion(ev)` |
| Render a live connection indicator (live / reconnecting / …) | `observe(loopId, { onState })` — **0056, D1** |
| Show the right affordance on auth-rejected / gone loop instead of hot-looping | `catch (e) { if (e instanceof FuseTerminalError) … }` — **0056, D2** |
| Tear the stream down on page unload without leaking it | `observe(loopId, { signal })` + `AbortController` — **0056, D3** |

Transparent reconnect with **no-loss / no-dup** across a mid-stream network drop is the
SDK's job — it re-observes from the last-seen seq and dedups at the watermark. Wander just
renders; the reconnect property is enforced by the headless-browser CI lane (see below).

## Architecture — the SDK speaks Connect directly (no WS relay)

```
browser  ──(connect-web, same origin)──►  server.js  ──(HTTP reverse-proxy)──►  fuse loop-serve-net
  app.js → @fuse/sdk (vendor/fuse-sdk.js)      static + /fuse.loop.v1.* proxy         Connect/protobuf
```

Unlike the older `examples/concierge-demo` (a WebSocket relay over the #48 wire), Wander's
`server.js` runs **no WebSocket relay and re-implements no protocol**. It only:

1. serves the static page + the esbuild-bundled SDK (`vendor/fuse-sdk.js`), and
2. **transparently reverse-proxies** the Connect service path `/fuse.loop.v1.*` to
   `fuse loop-serve-net`.

The proxy exists purely to keep the browser **same-origin** (so there is no CORS): the SDK
still drives the real Connect wire end-to-end. (`fuse loop-serve-net` serves no CORS headers
today — the same-origin reverse proxy is the friction-free browser path. This was one of the
rough edges building Wander surfaced; see the change results file.)

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
**headless-browser reconnect CI lane** (`go test -tags browser ./...`, `make browser-test`)
that serves this page, drives a concierge turn in headless chromium, kills the network
mid-stream, and asserts the reply still completes with no loss / no dup.
