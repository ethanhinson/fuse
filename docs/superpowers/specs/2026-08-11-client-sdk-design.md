<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0050 — Client SDK — thin-client library, same API local-or-remote](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0050-client-sdk.md)**
<!-- docket:backlink:end -->

# Client SDK — a Runtime-parity Go library, one API local-or-remote

## Context

The north-star end-state is that the user's *other* apps are thin network clients over a hosted
fuse service. The transport for that already exists: change **#48** (done, ADR-0032) exposes the
policy-free `Runtime` seam over the wire — a WebSocket full-session channel (`loop.start` /
`loop.send` / `loop.observe` + server-push `loop.event`) and a stateless HTTP replay endpoint
(`Attach(loop_id, from)`), reusing binding #2's `loop.*` JSON-RPC protocol. `tenant_id` already
flows over that wire **present-but-unenforced** (identity enforcement is change #49).

What #48 deliberately left to **#50** (its own *Out of scope*): "REST-native `loop.start`/`loop.send`
over HTTP and a versioned external-SDK wire envelope — client-SDK ergonomics." That is this change.

The `Runtime` interface (`internal/runtime/runtime.go`) is a small, policy-free seam:

```go
type Runtime interface {
    StartLoop(ctx, cfg LoopConfig) (LoopHandle, error)
    Send(ctx, loopID string, input string) error
    Spawn(ctx, loopID string, opts SpawnOpts) (SpawnHandle, error)
    Observe(loopID string) (<-chan event.Event, func(), error)
    Attach(loopID string, from event.Seq) ([]event.Event, error)
}
```

Today it is reachable only as in-process Go calls from the composition root (`cmd/fuse`), or over
the wire via a low-level JSON-RPC client. Neither is a library another app imports to "get a loop,
local or hosted, identically."

**Decided in grooming (2026-08-11):**

1. **First SDK is Go** — the in-process `Runtime` API surface, plus a remote backend over #48's
   WS/HTTP wire, one constructor switch. Cross-language (TS/Python) clients — and the versioned
   external wire envelope they'd force — are a **later change**, not #50.
2. **Runtime-parity, not "transport-transparent".** The SDK's API is **identical to the `Runtime`
   seam** whether it drives a local in-process `Runtime` or a remote one over the wire. The term is
   *Runtime-parity*; the word "transparent" is deliberately avoided in the spec and in code doc
   comments (the network is not invisible — see decision 3).
3. **One API, mechanics surfaced.** The method surface is identical across backends, **but** the
   event stream, the replay cursor (last `event.Seq`), gap/reconnect, and the **explicit
   park/completion event** are *first-class* in the API — because a client **cannot** infer "this
   exchange is done" or "safe to reconnect from here" from the *shape* of the event stream once the
   loop persists across turns (learning `persistent-loop-needs-explicit-completion-event`), and a
   naive replay double-delivers events landing between subscribe and replay
   (`replay-live-handoff-dedup-at-watermark`). The SDK is *honest* about the persistent-loop
   lifecycle rather than papering over it.
4. **Extract #48's WS/HTTP client into the SDK** as the single remote-backend implementation — one
   home for the dedup-at-watermark + gap-driven re-observe discipline, matching the "one
   implementation, two transports" pattern #48 already used server-side. The client #48 built to
   drive its own in-process test is promoted (and hardened) into the library, not forked.
5. **Credential seam, pass-through now.** The constructor takes an identity/tenant seam that today
   forwards `tenant_id` on the wire **present-but-unenforced** (matching #48), and becomes the real
   auth carrier when #49 lands. The surface is designed once; #49 adds no breaking change to it.
6. **Both backends ship in #50.** A **local** backend wraps the in-process `Runtime` directly (no
   network); a **remote** backend goes over the wire; one constructor picks. This is the literal
   "same API local-or-remote" thesis, and the local backend is a thin adapter over an existing seam
   — so proving the switch for real costs little.

## Decision

### A Go SDK package presenting a Runtime-parity client over a local or remote backend

Add a new SDK package (e.g. `sdk/` at the module root, or `pkg/fusesdk` — final home decided at
plan time; it MUST NOT live under `internal/`, since the whole point is that other modules import
it). The package exposes a **client type whose method set is the `Runtime` seam** — `StartLoop`,
`Send`, `Spawn`, `Observe`, `Attach`, keyed on `loopID` — so an app written against the SDK reads
identically to one written against the in-process `Runtime`.

The client is constructed against **one of two backends**, chosen by the constructor:

- **Local backend** — a thin adapter that holds an in-process `runtime.Runtime` (built through the
  same composition root `cmd/fuse` uses) and forwards each SDK call to it directly. No network, no
  serialization; the SDK call *is* the seam call.
- **Remote backend** — drives #48's wire: WebSocket for the full session (`StartLoop` / `Send` /
  `Observe` + the `loop.event` tail) and the stateless HTTP endpoint for `Attach(from)` replay. This
  is the **extracted #48 client** (decision 4), promoted from test-facing to library-grade.

Both backends satisfy one internal backend interface; the public client is backend-agnostic. The
constructor switch is the *only* place local-vs-remote is named — e.g. `Dial(ctx, opts)` where opts
carry either an in-process `Runtime` (or the config to build one) **or** a server URL + credentials.

### Surfacing the persistent-loop mechanics (decision 3)

The SDK does **not** hide these; it makes them first-class and identical across backends:

- **Event stream** — `Observe(loopID)` returns a typed `event.Event` channel plus unsubscribe,
  exactly as the seam does. The remote backend's channel is fed by the WS `loop.event` push; the
  local backend's by `EventStore.Subscribe`. Same type (`event.Event`, inherited wire format), same
  shape.
- **Replay cursor** — the client tracks the last delivered `event.Seq`; `Attach(loopID, from)`
  returns durable history with `Seq > from`. Reconnect is **client-driven**: on a dropped remote
  connection the SDK re-observes `from=<lastSeq>` and the inherited **subscribe-before-replay +
  dedup-at-watermark** path guarantees no loss / no dup. Gap markers drive re-observe. This logic
  lives once, in the extracted client (decision 4).
- **Explicit completion** — the SDK surfaces the loop's explicit park/completion event (e.g.
  `loop.parked` carrying the final answer) as a first-class signal so a caller knows an exchange is
  done and it is safe to `Send` the next turn — rather than guessing from stream shape
  (`persistent-loop-needs-explicit-completion-event`). Exact API shape (a typed event a caller
  selects on, and/or a `LoopHandle`-style await) decided at plan time; the *requirement* is that
  completion is observable without inferring it.

### Credential seam (decision 5)

The constructor accepts an **identity/credential provider** (an interface — e.g. a
`CredentialSource` that yields whatever the wire carries) plus a tenant. Today the remote backend
forwards `tenant_id` on the wire present-but-unenforced (the server ignores it for authz, exactly as
#48 ships); the local backend carries it through the seam. When **#49** lands its auth mechanism
(tokens / mTLS / OIDC — TBD there), it plugs into this same seam as the real credential carrier, so
#50's public surface does not break. The SDK MUST NOT hard-code an auth mechanism #49 has not yet
chosen — it defines the *seam*, not the scheme.

### Wire envelope scope

#48 named a "versioned external-SDK wire envelope" as #50's concern. For a **Go-only** first SDK the
wire is **inherited as-is** from #48 (`event.Event`'s existing JSON encoding, `loop_id` =
`tree.RootID()` as the handle, the `loop.*` methods) — a Go client and a Go server share the same
types, so no new cross-language envelope is minted here. A *versioned, language-neutral* envelope is
deferred to the first **non-Go** SDK (a later change), where wire-contract stability across
independently-versioned client and server actually bites. #50 MAY record the current wire shape as
the de-facto v0 contract, but ships no new serialization.

## Consequences

**Enables.** "Any AI app can target fuse" becomes concrete *in-language first*: an app imports the
SDK, calls `StartLoop`, and drives a loop — local for embedded use, remote for the hosted service —
through one identical API. It proves the Runtime seam is genuinely portable not just across
transports (which #48 proved) but across the local/remote boundary from the *caller's* side. It
gives #49 a ready-made credential seam to fill, and gives a future TS/Python SDK a proven reference
surface + wire shape to target.

**Costs / gives up.** Extracting #48's test-grade client into a library-grade one is real hardening
work (error taxonomy, reconnect, resource cleanup) — the WS client must treat **every** post-handshake
read error as a clean shutdown (learning `websocket-read-errors-are-not-closeerror`), not only
`websocket.CloseError`. Shipping both backends means #50 owns a local adapter *and* a remote client.
No cross-language reach yet — the north-star web/Python apps still wait on a later SDK. The credential
seam is a shape, not enforcement: until #49 lands, the remote backend is single-trust-domain (a
non-goal carried from #48/#45), and the spec must not imply otherwise.

**Dependencies.** `depends_on: [48, 49]`. #48 is **done** (the transport + the client to extract).
#49 is **proposed / needs-brainstorm** and gates *build* — the implementer's reconcile pass
re-validates this spec against #49's actual identity decision at build time; if #49's credential
model differs from the pass-through seam sketched here, reconcile adjusts the seam, not the rest of
the design.

## Out of scope

- **A non-Go (TS / Python) SDK** and the **versioned language-neutral wire envelope** it forces —
  a later change; #50 is Go-only and inherits #48's wire as-is.
- **The auth *mechanism*** (tokens / mTLS / OIDC) and enforcement — change #49. #50 defines only the
  credential *seam* the mechanism plugs into.
- **Any change to the `Runtime` seam itself** — the SDK is a *client over* the seam, not a
  modification of it; the seam stays policy-free.
- **Observability emission** (OTEL / `/metrics`) — change #51.
- **TLS / deployment topology / load-balancing** — operational concerns beneath the transport (#48).

## Open questions (for plan-time reconcile)

- Final package home and name (`sdk/` vs `pkg/fusesdk`), and the exact public type/constructor names
  — must be import-safe from outside the module and MUST NOT sit under `internal/`.
- The precise completion-signal API shape (a typed event on the `Observe` channel, a `LoopHandle`
  await, or both) — the *requirement* (completion observable without stream-shape inference) is
  fixed; the surface is a plan-time call.
- Whether the local backend takes a pre-built `runtime.Runtime` or the config to build one (or both),
  given the composition-root wiring in `cmd/fuse`.
- How the credential seam reconciles with #49's chosen mechanism — re-validated at build reconcile.
