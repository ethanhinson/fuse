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
func (c *Client) Interrupt(ctx, id LoopID, input string) error            // Send that cancels the loop's current step so the message lands now
func (c *Client) SendWith(ctx, id LoopID, input string, SendOptions) error // Interrupt, and/or Source: SourceHuman | SourceOrchestrator
func (c *Client) Observe(ctx, id LoopID, fromSeq Seq) (<-chan Event, func(), error)
func (c *Client) Capabilities(ctx) (Capabilities, error)              // what this tenant may use in a spec

func IsCompletion(e Event) bool   // true on the loop.parked completion event

type StartLoopConfig struct {
    Task, Model string
    Interactive bool
    Orchestrate LoopID                 // watch and steer that loop
    Harness     *harnessv1.HarnessSpec // run this harness (layered over the server's config)
}
type Credentials     struct { Token, Tenant string }

// Event / Seq / Kind / LoopID are aliases of internal/event types so callers
// don't import internal/ directly. Payload stays raw JSON.
type Event = event.Event
type Seq   = event.Seq
type Kind  = event.Kind
type LoopID = string
```

Sentinel errors: `ErrUnauthenticated`, `ErrPermissionDenied`, `ErrLoopNotFound`, `ErrLoopFinished`,
`ErrHarnessUnsupported`, `ErrCapabilitiesUnsupported`.

## Start a loop from a harness spec

`StartLoopConfig.Harness` carries a `fuse.harness.v1` `HarnessSpec`
(`github.com/ethanhinson/fuse/harness/v1`) so the loop runs that harness instead of the
server's config. The server's trusted config stays the floor: fields marked `tighten_only`
may only tighten the server's values, and operator-only settings (gateway, sandbox,
observability, auth) always come from the server. Skill names resolve against the skills
installed on the server; `secret_ref` names resolve through the server operator's
`loop_server.secrets` allowlist for your tenant.

```go
turns := uint32(5)
id, err := c.StartLoop(ctx, fuse.StartLoopConfig{Task: "triage the backlog", Harness: &harnessv1.HarnessSpec{
    Models:   &harnessv1.Models{DefaultAlias: "glm"},
    MaxTurns: &turns,
    Skills:   []*harnessv1.SkillRef{{Name: "research"}},
}})
var bad *harness.ValidationError
if errors.As(err, &bad) {
    for _, v := range bad.Violations {
        fmt.Println(v.FieldPath, v.Message, v.RuleID) // e.g. skills[0].name ... fuse.skill_ref
    }
}
```

The spec and its digest (`harness.Digest`) are the first event of the loop's stream
(`loop.harness`), so a resume on any server replica rebuilds the same harness.

A client can also send loop-scoped secret values for the spec's `secret_ref` names
(the token needs the `secrets:inline` scope). Each value resolves only for the listed
hosts, lives in server memory only, is never recorded, and is gone after a resume. An
operator secret of the same name wins.

```go
id, err := c.StartLoop(ctx, fuse.StartLoopConfig{Task: "triage the backlog", Harness: spec}.
    WithSecrets(fuse.LoopSecret{Name: "my-token", Value: tok, Hosts: []string{"mcp.acme.test"}}))
```

Right after it, the loop records one `mcp.server` event per configured MCP server
(name, transport, auth type, whether it connected, the error if not, and the tool
names it exposes). A server that cannot connect is reported and skipped; the loop
continues. No event carries a credential. The event is emitted at each loop start,
including a resume, so consumers should take the latest event per server name.

### Pre-flighting a spec

Call `Capabilities(ctx)` before `StartLoop` to check a spec against what the
server will accept:

- Build a model picker from `Models` (`Alias`, `Label`, `Description`, `Tags`,
  `IsDefault`, `ContextWindow`).
- Reference only the listed `Skills` and `SecretRefs`.
- Keep each tighten-only field strictly below its `Ceiling`'s `Value`
  (`FieldPath`, `Value`, `Direction`; a value equal to it is refused), and
  leave it unset when `OperatorSet` is false.

A local client or an older server returns `ErrCapabilitiesUnsupported`.

```go
caps, err := client.Capabilities(ctx)
if errors.Is(err, fuse.ErrCapabilitiesUnsupported) {
    // no pre-flight available; StartLoop still enforces every limit
}
```

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

### Short-lived tokens (`WithTokenProvider`)

Pass `fuse.WithTokenProvider` to `NewRemote` to fetch the bearer token per request (each unary
call and each stream open); `Token` is then ignored:

```go
c := fuse.NewRemote(url, fuse.Credentials{Tenant: "acme"},
	fuse.WithTokenProvider(func(ctx context.Context) (string, error) { return mint(ctx) }),
)
```

A provider error fails that request before anything is sent. There is no caching
or refresh loop; cache inside the provider if needed. A provider returning `""`
sends no `Authorization` header, like an empty `Token`. The provider is an option, not a `Credentials` field, so
`Credentials` stays comparable. The local backend ignores it.
