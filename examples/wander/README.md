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
- **＋ New** is the explicit fresh-start gesture: it tears the live observe stream down and
  also clears the stored session, through the same reset path a user switch uses — one
  teardown route, not a second one hand-rolled per gesture. A page reload no longer resets
  anything; it now *restores* the conversation (see *Scope*).

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

**`server.js` binds `127.0.0.1` only, and that is not configurable.** It publishes demo
bearer tokens at `/demo-users.json` and forwards `Authorization` verbatim through the proxy
into your local `loop-serve-net`, so a LAN-reachable listener would let any peer fetch a
credential and then drive your backend with it. View a remote demo through a tunnel
(`ssh -L 5173:127.0.0.1:5173 box`), never by widening the bind.

The token endpoint also **fails closed** on an override: point `FUSE_DEMO_CONFIG` at
anything other than the checked-in `examples/wander/fuse.demo.yml` and `/demo-users.json`
answers `403` (the picker degrades to the built-in dev credential) unless you *also* set
`FUSE_DEMO_PUBLISH_TOKENS=1`. That second, explicit opt-in is what stops a stale
`FUSE_DEMO_CONFIG=~/.fuse/config.yml` from handing out every real `loop_server.auth` token.

## Run it

```sh
# from the repo root, once:
npm install                                          # installs esbuild (used by build.sh)
cp examples/wander/fuse.demo.yml ~/.fuse/config.yml  # or merge it into yours, by hand

# then:
./examples/wander/run.sh    # SDK bundle + rentals MCP server + loop-serve-net + the page
# open http://127.0.0.1:5173   (loopback only — server.js does not bind the LAN)
```

`run.sh` is the whole demo in one command: alongside the bundle and `loop-serve-net` it
builds and starts **`cmd/rentals-mcp`** — the live rentals backend — on `127.0.0.1:8091`
with env matching `fuse.demo.yml`, and its `trap` tears all three processes down on Ctrl-C.
Without that server every rentals tool call fails to connect, so it is not optional.

The one-time config copy is not optional either: `loop_server.auth` and `tool_identity` are
credential surfaces honored **only** from the trusted `~/.fuse/config.yml` (ADR-0006), so a
repo-relative file cannot supply them. Without it fuse has no rentals server declared, no
signing key to mint delegation tokens with, and no demo user directory.

`run.sh` points the loop server at whatever model gateway your `~/.fuse/config.yml`
configures. To run **fully offline / deterministic** (no provider — the project policy for
tests is to NEVER call Claude/Anthropic), set `LLM_GATEWAY_URL` to a scripted double before
launching; the headless-browser CI lane does exactly this.

The rentals server needs **no search credential**: it defaults to `RENTALS_DATA=auto`, which
uses live web-search listings only if a credential (e.g. `TAVILY_API_KEY`) is already in the
environment and otherwise serves canned listings. Set `RENTALS_DATA=canned` to force
hermetic data.

Environment knobs: `PORT` (static server, default `5173`), `FUSE_NET_ADDR` (backend, default
`127.0.0.1:8787`), and the rentals overrides `RENTALS_ADDR`, `RENTALS_AUDIENCE`,
`RENTALS_SIGNING_KEY`, `RENTALS_TENANTS`, `RENTALS_FAVORITES_DIR`, `RENTALS_DATA`. The
rentals defaults are the values in `fuse.demo.yml`; overriding the first three means editing
`fuse.demo.yml` to match, or fuse dials the wrong port or mints tokens the server rejects.
`internal/config`'s `TestWanderRunScriptStartsRentalsServer` asserts the two sides agree.

`fuse.demo.yml` in this directory is the checked-in demo config for the rentals MCP server
and its demo user directory (change 0060). Its tokens are obviously fake and **demo-only**.

## Switching users (change 0060)

The rail's **Signed in as** picker chooses which bearer credential the SDK client presents.
The first option is always the built-in `loop-serve-net` dev token (`fuse-dev-token` →
`_default`), so a zero-config backend behaves exactly as it did before the picker existed;
the demo principals are appended from `GET /demo-users.json`, which `server.js` reads out of
`fuse.demo.yml` (override with `FUSE_DEMO_CONFIG`). You can also paste an arbitrary token.

> ⚠ `/demo-users.json` **publishes every bearer token in that file** to any browser that
> asks. That is safe for the checked-in demo tokens and for nothing else — never point
> `FUSE_DEMO_CONFIG` at `~/.fuse/config.yml`.

The client never asserts who it is beyond presenting the token: `loop_server.auth` resolves
`{tenant, subject}` server-side, and the identity tier mints the audience-bound delegation
token the rentals MCP server adjudicates. The **Saved stays** panel is filled *only* from a
`list_favorites` tool result, so it shows exactly what that principal's delegated token was
allowed to read — which is why switching users shows a different list.

Switching users performs a **full client-side reset** before the new session starts: the
live observe stream is aborted through the SDK's idempotent teardown *first*, then the
transcript, activity rail, stats and saved panel are cleared and a new session generation
begins (so any frame still draining on the old stream is inert). The `＋ New` button runs
the same reset for the same principal. Both gestures also **forget the stored session**, so
either one is an explicit fresh start — only a *reload* restores (see *Scope*).

A stored session belongs to the principal that minted it, and reloading re-selects that
principal in the picker before restoring. What is persisted is the principal's **name**
(`{tenant, subject}`) — never its token; the token is re-resolved from the demo directory
the page already fetches. So a session minted under a **pasted** token is deliberately *not*
restorable: the app will not put a bearer credential in `localStorage`, and the
`{tenant, subject}` it shows for a pasted token is its own guess rather than the server's
resolution of that token. Reloading after using a pasted token starts a fresh session, as
does reloading when the stored principal is no longer in the directory.

Note for anyone editing `fuse.demo.yml`: `mcp_servers[rentals].url` must be the **base**
URL with no `/sse` suffix — the MCP HTTP transport appends `/sse` and `/messages` itself.

## Build the SDK bundle only

```sh
./examples/wander/build.sh   # → examples/wander/vendor/fuse-sdk.js (git-ignored artifact)
```

`build.sh` runs esbuild over `sdk/ts/src/index.ts`, inlining `@connectrpc/connect-web` and
`@bufbuild/protobuf`, so `index.html` imports the **real published SDK surface**, not
relative proto stubs.

## Scope

Wander persists its `loopId` — together with the `{tenant, subject}` of the principal that
minted it, never that principal's token — and, on reload, re-selects that principal and
restores the conversation by replaying the loop's durable event stream from `seq` 0: the
browser-visible face of change #54's server-side durable resume. The restore is refused
unless the stored principal matches the credential in hand, so it can never issue a
cross-owner `Observe`. Sessions started under a **pasted** token are not persisted at all
(see *Switching users*). It is an example app, not a production deployment.

The conversation is truly lost only when the durable stream itself is gone (a wiped or
rebuilt store): that surfaces as a terminal `not_found` on the restore attempt, and Wander
falls back to starting a clean fresh session rather than pretending the old one is still
there.

After roughly 30 minutes idle, the server reaps the live run behind the session — and you
will not notice. The reap cancels the run, which ends the observe stream *cleanly*; a clean
end is transient by the SDK's own classification, so the SDK simply re-opens the stream from
its watermark. The loop's registry record and its durable events are untouched, so the
re-observe succeeds, and the next message you send revives the loop transparently (the
server resumes a reaped-but-resumable loop and retries the send — see
`internal/loopconnect/handler.go`). There is deliberately **no** "session paused" state in
the UI, because from the browser's point of view nothing happened: the transcript stays on
screen, the composer stays usable, and a reload restores the same conversation the same way
any other reload does. "Reap ≠ loss" holds by the reap being invisible, not by a visible
paused affordance.

## Acceptance / test

An example app has no unit test of its own; its acceptance is two permanent headless-browser
CI lanes, both `//go:build browser`, both run by `make browser-test` /
`go test -tags browser -timeout 300s ./examples/wander/...` and enforced by the
`browser-acceptance` job in `.github/workflows/integration.yml`.

**`browser_test.go` — the reconnect lane.** It serves this page, drives a concierge turn in
headless chromium against a real `loop-serve-net` with a scripted gateway double, kills the
network mid-stream via `/__cut`, and asserts the reply still completes with **no loss / no
dup** and that the SDK went `reconnecting` → `live`.

**`browser_identity_test.go` — the per-principal isolation lane.** It stands up the real
`cmd/rentals-mcp` server plus a `loop-serve-net` configured from **this directory's
`fuse.demo.yml`** (only the rentals URL is rewritten to an ephemeral port), then drives the
page as two different demo principals: user A favorites a listing, and after switching to
user B the saved panel must NOT contain it (and switching back must show it again). It also
asserts the switch left **exactly one** live observe stream — the previous principal's must
have been torn down — and that the transcript was cleared. Because the checked-in demo
config is used verbatim, config rot fails this lane instead of quietly demoing nothing.

**Restore is asserted in both files**, because it is a property of the *credential* as much
as of the transport. `browser_test.go` closes the tab and reopens one in the **same**
browser context (contexts are storage-isolated, so a fresh one would silently test the
fresh-session path) and asserts the same loop comes back with both its question and its
answer — and, separately, that a stored session naming a principal who is not in the demo
directory is *forgotten* and replaced by a working fresh session rather than retried under
the wrong credential. `browser_identity_test.go` does the same reopen while a **non-dev**
demo principal is selected, and asserts the page comes back as that principal, on that
principal's loop, with no terminal error — which is what proves the `Observe` was issued
under their token and not the one the page boots with.

Changing the markup here can silently break both lanes: they drive `#input`, `#send`,
`#user`, `#whoami` and `#saved .saved-item`, and read `.msg.concierge` / `.msg.error` plus
the `window.__wander*` instrumentation `app.js` publishes. Keep those, and run the lanes
before and after any UI change.
