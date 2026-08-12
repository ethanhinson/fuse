# @fuse/sdk — TypeScript/JS client for the fuse loop wire

A **Runtime-parity, remote-only** client for `fuse.loop.v1`. It presents the same
`startLoop` / `send` / `observe` surface the Go SDK does, over Connect: browser reach via
[`@connectrpc/connect-web`](https://connectrpc.com/) by default, with a
`@connectrpc/connect-node` transport injectable for tests. Change 0050.

## Install

This package lives in the repo's npm workspace (root `package.json`). From the repo root:

```sh
npm install
```

`@fuse/sdk` depends on the generated stubs in the sibling `proto/` workspace, imported by
relative path (`../../proto/gen/ts/...`) since `proto` exposes no `exports` map.

## createClient (remote-only)

```ts
import { createClient, isCompletion } from "@fuse/sdk";

const client = createClient({
  baseUrl: "https://your-fuse-host",
  credentials: { token: "fuse-dev-token", tenant: "_default" },
  // transport?  — optional. Defaults to a browser connect-web transport at baseUrl.
});
```

The `credentials` seam is the concrete #49 mechanism: the `token` is sent as
`Authorization: Bearer <token>` on **every** request (a Connect interceptor) and the
`tenant` is threaded into every request message. This SDK only *presents* the seam — auth
**enforcement** (verifier, tenant-spoof rejection) is #49's server-side business.

## Drive a loop

```ts
const { loopId } = await client.startLoop({
  task: "summarize the repo",
  model: "cloud/x",
  interactive: true, // persistent conversational loop; parks between turns
});

await client.send(loopId, "hello");

for await (const ev of client.observe(loopId)) {
  console.log(ev.seq, ev.kind);
  if (isCompletion(ev)) break; // the loop parked (kind === "loop.parked")
}
```

`observe(loopId, fromSeq?)` is an **async iterable over domain events**. It:

- **skips keepalive frames** (`frame.keepalive || !frame.event`);
- tracks `seq` (a `bigint`) and surfaces the `gap` re-observe hint on each event;
- **reconnects transparently**: when the server stream ends or errors while you are still
  iterating, it re-opens `observe(loopId, fromSeq = lastSeq)` — the browser analogue of
  treating every post-handshake read/close as a clean shutdown and resuming from the
  last-seen seq. A defensive watermark drops any overlap frame, so the stream is
  **no-loss / no-dup** across the reconnect.

## Reconnect / completion

Reconnect is not a separate call — it is just `observe(loopId, fromSeq)` with the last seq
you saw, matching the wire contract (`Observe(from_seq)` subsumes replay + live tail). The
SDK does this for you internally on stream end; you can also drive it explicitly by
breaking the loop and re-calling `observe(loopId, lastSeq)`.

Completion is a first-class predicate `isCompletion(event)` keyed on the real event kind
(`internal/event.KindLoopParked = "loop.parked"`), never inferred from stream shape. After
a completion event on an interactive loop you may `send` the next turn.

## Tests

`npm test` (or `make sdk-ts-test` from the repo root) runs `test/remote.test.ts`
(node's built-in `node:test`). It spawns a **real connect-go server** — a tiny Go helper
under `test/server/main.go` that mounts the loop handler over a scripted `fakeRuntime` —
reads the URL it prints, and drives this SDK over the wire. It asserts, **from TS**:

- `startLoop → send → observe` delivers the scripted events;
- **no-loss/no-dup** across the server's subscribe→replay gap (the helper forces an
  overlap event into the window so a naive replay would double-deliver it — the client
  sees every seq exactly once);
- a **reconnect** `observe(fromSeq = lastSeq)` resumes with no loss and no dup.

This proves the **wire from TS** over a fake backend. The **real-engine** end-to-end for
the identical, language-agnostic wire is covered by the Go `sdk/fuse/acceptance_test.go`
real-loop test (scripted `LLM_GATEWAY_URL`, never Claude — project policy).

The CI lane `make sdk-ts-test` **fails loud** when node is absent in an environment that
should have it (a `command -v node` guard, never a green skip).

## Connection state, terminal errors, teardown (change 0056)

`observe` has an additive options form alongside the positional `observe(loopId, fromSeq?)`:

```ts
import { createClient, FuseTerminalError, type ConnState } from "@fuse/sdk";

const ac = new AbortController();
try {
  for await (const ev of client.observe(loopId, {
    fromSeq: 0n,
    signal: ac.signal, // idempotent teardown: ac.abort() stops observing + releases the stream
    onState: (s: ConnState) => setIndicator(s), // "connecting" | "live" | "reconnecting" | "closed"
  })) {
    render(ev);
  }
} catch (err) {
  if (err instanceof FuseTerminalError) {
    // A TERMINAL Connect code (unauthenticated / permission_denied / not_found /
    // failed_precondition) stops the reconnect loop instead of hot-looping. err.code carries
    // the Connect code so the app can show the right affordance.
    showTerminal(err.code);
  }
}
```

- **`onState`** surfaces the reconnect lifecycle so an app can render a connection indicator
  without reaching into the transport.
- **Terminal-error classification**: an abnormal mid-stream drop (network error, stream end)
  is *transient* → the SDK reconnects from the watermark (no loss/no dup). A *terminal*
  Connect code throws a typed `FuseTerminalError` out of the iterator and fires
  `onState("closed")` — it does **not** hot-loop.
- **`signal`** (an `AbortSignal`) is the page-unload / component-teardown primitive: aborting
  stops the loop, tears the stream down, fires `closed` once, and is idempotent.

These are dogfooded by the [`examples/wander`](../../examples/wander) concierge demo.

## Verify (human) → now an enforced CI lane

The real-browser reconnect proof deferred at #50/#55 is now a **permanent, loud headless-
browser CI lane** — no longer a manual checkbox.

- **What it does.** `go test -tags browser ./...` (target `make browser-test`; CI job
  `browser-acceptance` in `.github/workflows/integration.yml`) drives the real
  [`examples/wander`](../../examples/wander) app — which imports the real `@fuse/sdk` over
  `@connectrpc/connect-web` — in headless chromium against a real `fuse loop-serve-net`
  backend with a scripted `LLM_GATEWAY_URL` double (**never** Claude/Anthropic), **kills the
  network mid-stream**, and asserts the concierge reply completes after a transparent
  reconnect with **no loss / no dup** (a strictly-increasing seq log across the drop).
- **Loud on toolchain absence.** Missing node / esbuild / go / a playwright-installable
  chromium is a hard `t.Fatal`, never a green `t.Skip` — a passing suite can never hide an
  unexercised browser path (`smoke-over-fake-backend-proves-wire-not-system`).
```
