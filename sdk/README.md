# fuse Go SDK (`github.com/ethanhinson/fuse/sdk/fuse`)

A small Go library that exposes fuse's **Runtime-parity** loop-control surface — `StartLoop` /
`Send` / `Observe`, keyed on `loopID` — over **either** a local in-process runtime **or** a remote
hosted runtime, chosen by the constructor. The same method surface works local-or-remote (change
0050). The TypeScript sibling lives in [`../sdk/ts`](../sdk/ts).

The network is **surfaced, not hidden**: the event stream, the replay cursor (`fromSeq`), gap
markers, and the **explicit completion event** (`loop.parked`) are all first-class — a client never
infers "this exchange is done, safe to send the next turn" from the *shape* of the stream.

## Install

```go
import "github.com/ethanhinson/fuse/sdk/fuse"
```

## Public surface

```go
type Client struct { /* ... */ }

func (c *Client) StartLoop(ctx, StartLoopConfig) (LoopID, error)
func (c *Client) Send(ctx, id LoopID, input string) error
func (c *Client) Observe(ctx, id LoopID, fromSeq Seq) (<-chan Event, func(), error)

func IsCompletion(e Event) bool   // true on the loop.parked completion event

type StartLoopConfig struct { Task, Model string; Interactive bool }
type Credentials     struct { Token, Tenant string }

// Event / Seq / Kind / LoopID are aliases of internal/event types so callers
// don't import internal/ directly. Payload stays raw JSON.
type Event = event.Event
type Seq   = event.Seq
type Kind  = event.Kind
type LoopID = string
```

Sentinel errors: `ErrUnauthenticated`, `ErrPermissionDenied`, `ErrLoopNotFound`, `ErrLoopFinished`.

## Remote backend (hosted fuse over the Connect wire)

Drives change 0055's `fuse.loop.v1` Connect/protobuf transport (successor to the WebSocket wire,
ADR-0033). Point it at a `fuse loop-serve-net` server:

```go
c := fuse.NewRemote("http://localhost:8080", fuse.Credentials{
    Token:  "fuse-dev-token", // the server's built-in dev token when loop_server.auth is unset
    Tenant: "acme",
})

id, err := c.StartLoop(ctx, fuse.StartLoopConfig{Task: "summarize x", Model: "cloud/x", Interactive: true})
_ = c.Send(ctx, id, "and now translate it")

ch, cancel, err := c.Observe(ctx, id, 0) // fromSeq=0 ⇒ full history then live
defer cancel()
for ev := range ch {
    if fuse.IsCompletion(ev) {
        // this exchange is done; safe to Send the next turn
    }
}
```

**Reconnect** is `Observe(ctx, id, lastSeq)` — re-open from the last `Seq` you saw; the SDK preserves
the subscribe-before-replay + dedup-at-watermark discipline so a reconnect loses and duplicates
nothing across the subscribe→replay gap.

## Local backend (in-process runtime)

For an app embedding fuse in-process. It takes a **pre-built** `runtime.Runtime` (building one pulls
the full composition root, which is `package main` and not importable — so the caller supplies the
runtime it built):

```go
rt := runtime.New(deps)                // your own composition
c := fuse.NewLocal(rt, fuse.Credentials{Tenant: "acme"})
// same StartLoop / Send / Observe surface; the SDK call IS the seam call.
```

## Credential seam (auth is change 0049)

Each constructor takes `Credentials{Token, Tenant}`. The remote backend sets
`Authorization: Bearer <token>` on every request and threads `tenant` on every request message; a
wrong token → `ErrUnauthenticated`, a tenant that mismatches the authenticated principal →
`ErrPermissionDenied`. The SDK defines only the *seam* — the auth **mechanism and enforcement** are
owned by change 0049 at the server edge; the local backend carries the tenant through the runtime
seam (no token).
