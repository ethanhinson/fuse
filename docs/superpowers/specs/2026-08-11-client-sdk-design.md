<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0050 — Client SDK — thin-client library, same API local-or-remote](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0050-client-sdk.md)**
<!-- docket:backlink:end -->

# Client SDK — Runtime-parity Go + TS/JS libraries over one versioned wire

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

**Motivating consumer.** The **Wander** demo app is a browser (TS/JS) client — it is the concrete
reason #50 ships a JS SDK, not only a Go one, and the reason the cross-language wire contract is in
scope *now* rather than deferred to a later SDK.

**Decided in grooming (2026-08-11):**

1. **Two SDKs in this change: Go and TS/JS.**
   - **Go SDK** — the in-process `Runtime` API surface plus a remote backend over #48's WS/HTTP
     wire, one constructor switch; **both a local and a remote backend ship** (the literal "same API
     local-or-remote" thesis).
   - **TS/JS SDK** — a **remote-only** client (a browser has no in-process Go `Runtime`) presenting
     the **same Runtime-parity surface** over WS + HTTP, so the Wander browser app can start, drive,
     observe, and replay loops.
2. **Runtime-parity, not "transport-transparent".** Each SDK's API is **identical to the `Runtime`
   seam** — `StartLoop` / `Send` / `Observe` / `Attach`, keyed on `loopID`. The term is
   *Runtime-parity*; the word "transparent" is deliberately avoided in the spec and in code doc
   comments (the network is not invisible — see decision 4).
3. **The versioned wire contract is owned by change 55, and #50 depends on it.** A TS client and a Go
   server do not share types the way two Go peers do, so the `loop.*` envelope + `event.Event` shape
   must be a **versioned, schema-first contract** both SDKs target. Rather than generate TS types from
   Go structs over #48's JSON wire, that contract is now a **separate change (#55)** — a gRPC/protobuf
   transport, successor to #48, **superseding ADR-0032**. #50 `depends_on: [55]` and both SDKs
   generate their clients from #55's IDL. (This replaces the Go-only sketch's "codegen from Go structs
   over #48's JSON wire"; the schema IDL is now the single source of truth.)
4. **One API, mechanics surfaced.** The method surface is identical across backends *and* languages,
   **but** the event stream, the replay cursor (last `event.Seq`), gap/reconnect, and the **explicit
   park/completion event** are *first-class* — because a client **cannot** infer "this exchange is
   done" or "safe to reconnect from here" from the *shape* of the event stream once the loop persists
   across turns (learning `persistent-loop-needs-explicit-completion-event`), and a naive replay
   double-delivers events landing between subscribe and replay
   (`replay-live-handoff-dedup-at-watermark`). Both SDKs are *honest* about the persistent-loop
   lifecycle rather than papering over it.
5. **Both SDKs drive change 55's gRPC transport**, generating their stubs from its IDL — the Go remote
   backend and the TS client alike. The reconnect discipline (subscribe-before-replay +
   dedup-at-watermark + gap-driven re-observe) is preserved over #55's streaming model. The generated
   contract, not shared code, is what keeps the Go and TS clients honest (TS cannot import Go).
   (#48's JSON WS/HTTP client is superseded by #55, not extracted into the SDK.)
6. **Credential seam, pass-through now.** Each SDK's constructor takes an identity/tenant seam that
   today forwards `tenant_id` on the wire **present-but-unenforced** (matching #48), and becomes the
   real auth carrier when #49 lands. The surface is designed once, in both languages; #49 adds no
   breaking change to it. (A browser SDK's credential carrier — how Wander supplies a token — is
   sketched here but its enforcement is #49.)

## Decision

### A Go SDK: Runtime-parity client over a local or remote backend

Add a Go SDK package (e.g. `sdk/` at the module root, or `pkg/fusesdk` — final home decided at plan
time; it MUST NOT live under `internal/`, since the whole point is that other modules import it). The
package exposes a **client type whose method set is the `Runtime` seam**, constructed against **one
of two backends** picked by the constructor:

- **Local backend** — a thin adapter holding an in-process `runtime.Runtime` (built through the same
  composition root `cmd/fuse` uses), forwarding each SDK call directly. No network; the SDK call *is*
  the seam call.
- **Remote backend** — drives **change 55's gRPC transport** (decision 5), generating its stub from
  55's IDL: the full session (`StartLoop` / `Send` / `Observe` + the event tail) + replay `Attach(from)`
  over 55's streaming.

Both satisfy one internal backend interface; the public client is backend-agnostic. The constructor
switch is the *only* place local-vs-remote is named.

### A TS/JS SDK: Runtime-parity, remote-only, for the browser (Wander)

Add a TS/JS package (home/tooling — npm workspace layout, bundler — decided at plan time) presenting
the **same Runtime-parity surface** — `startLoop` / `send` / `observe` / `attach` — **remote-only**
over change 55's transport. It consumes 55's generated TS stubs, tracks the last `Seq`, and
re-implements the **subscribe-before-replay + dedup-at-watermark + gap-driven re-observe** reconnect
discipline in TS. It surfaces the same first-class mechanics (event stream, replay cursor, explicit
completion) as the Go SDK. Its connection layer must treat **every** post-handshake read/close as a
clean shutdown and reconnect from `lastSeq` (the browser-side analogue of learning
`websocket-read-errors-are-not-closeerror`). **Browser reach depends on 55:** gRPC has no native
browser support, so whether the browser path is grpc-web + a proxy or a WS/JSON bridge is a #55 open
question the TS SDK inherits. No local backend — a browser has no in-process `Runtime`.

### The versioned wire contract (decision 3) — owned by change 55

The `loop.*` request/response envelope and the `event.Event` shape are pinned as a **versioned,
schema-first contract** — but that contract is **not built in #50**. It is change **#55** (a
gRPC/protobuf transport, successor to #48, superseding ADR-0032): #55 defines the `loop.*` protocol +
`event.Event` in an IDL, stands up the transport, and emits the generated Go and TS stubs. #50
`depends_on: [55]` and consumes those generated stubs — the Go SDK's remote backend and the TS SDK
both target #55's wire. This is a deliberate reversal of the earlier "codegen from Go structs over
#48's JSON wire" sketch: the human chose to reopen the transport decision and let a schema IDL be the
single source of truth, accepting that #50 (and Wander) now waits on #55.

### Surfacing the persistent-loop mechanics (decision 4)

Both SDKs make these first-class and identical, never hidden:

- **Event stream** — `Observe(loopID)` yields typed events plus unsubscribe. Go: `event.Event`
  channel; TS: an async iterator / event target over the same generated shape. Remote channels are
  fed by the WS `loop.event` push; the Go local backend by `EventStore.Subscribe`.
- **Replay cursor** — the client tracks the last delivered `Seq`; `Attach(loopID, from)` returns
  durable history with `Seq > from`. Reconnect is **client-driven**: on a dropped connection the SDK
  re-observes `from=<lastSeq>`; subscribe-before-replay + dedup-at-watermark guarantees no loss / no
  dup; gap markers drive re-observe.
- **Explicit completion** — both SDKs surface the loop's explicit park/completion event (e.g.
  `loop.parked` carrying the final answer) as a first-class signal so a caller knows an exchange is
  done and it is safe to `Send` the next turn — never inferred from stream shape
  (`persistent-loop-needs-explicit-completion-event`).

### Credential seam (decision 6)

Each constructor accepts an identity/credential provider plus a tenant. Today the remote backends
forward `tenant_id` on the wire present-but-unenforced (the server ignores it for authz, exactly as
#48 ships); the Go local backend carries it through the seam. When **#49** lands its auth mechanism
(tokens / mTLS / OIDC — TBD there), it plugs into this same seam as the real credential carrier in
both languages, so #50's public surface does not break. The SDKs MUST NOT hard-code an auth mechanism
#49 has not yet chosen — they define the *seam*, not the scheme.

## Consequences

**Enables.** "Any AI app can target fuse" becomes concrete across the boundary that matters for the
demo: the **Wander browser app imports the TS SDK** and drives a hosted loop with the same surface a
Go app uses locally. The Go SDK proves the seam is portable across the local/remote boundary; the TS
SDK proves it is portable across *languages*; and the versioned wire envelope gives every future SDK
(Python, mobile) a stable, one-source-of-truth contract to target. #49 gets a ready credential seam
to fill in both languages.

**Costs / gives up.** This is materially larger than a Go-only #50: two client implementations of the
reconnect/dedup discipline (Go + TS), each generated from #55's IDL, and a JS package toolchain
(bundling, publishing) the repo does not have today. The two clients are kept honest by #55's
generated *contract*, since they share no code. **The dominant cost is now the #55 dependency:** the
SDKs cannot be built until a brand-new, undesigned gRPC transport lands — so #50 and the Wander demo
are gated on #55 (plus #49). Had the SDKs targeted #48's existing JSON wire (codegen from Go structs),
#50 could have proceeded once #49 landed; the IDL-first choice trades that near-term path for a
single schema source of truth and no later wire migration. The credential seam is a shape, not
enforcement: until #49 lands, both remote backends are single-trust-domain (a non-goal carried from
#48/#45).

**Dependencies.** `depends_on: [48, 49, 55]`. #48 is **done** (the transport #55 supersedes). #49 is
**proposed / needs-brainstorm** and gates *build* — the reconcile pass re-validates this spec against
#49's actual identity decision at build time. **#55** (the gRPC/protobuf transport) is **newly minted
and undesigned**, and now gates the whole SDK: both SDKs generate against its IDL, so #50 cannot build
until #55's wire exists. This is the cost of the human's decision to reopen the transport via an IDL
rather than ship the SDKs on #48's JSON wire — Wander is now downstream of #55 as well as #49.

## Out of scope

- **A Python (or mobile-native) SDK** — a later change; #50 ships Go + TS/JS. #55's IDL is exactly
  what makes those cheap later.
- **The gRPC/protobuf transport + IDL itself** — change #55. #50 *consumes* #55's generated stubs; it
  does not define the wire or the transport.
- **The auth *mechanism*** (tokens / mTLS / OIDC) and enforcement — change #49. #50 defines only the
  credential *seam* the mechanism plugs into, in both languages.
- **The Wander app itself** — #50 ships the SDK Wander consumes, not Wander. Wander is the motivating
  consumer and the acceptance target, not a deliverable of this change.
- **Any change to the `Runtime` seam** — the SDKs are clients *over* the seam.
- **Observability emission** (OTEL / `/metrics`) — change #51.
- **TLS / deployment topology / load-balancing** — operational concerns beneath the transport (#48 → #55).

## Open questions (for plan-time reconcile)

- Go package home/name (`sdk/` vs `pkg/fusesdk`) and TS package layout (npm workspace, bundler,
  publish target) — plan-time; the Go package must be importable from outside the module and never
  under `internal/`.
- How the SDKs consume #55's generated stubs (package boundaries, versioning), and the browser path
  #55 settles on (grpc-web + proxy vs. a WS/JSON bridge) that the TS SDK inherits — re-validated once
  #55 is designed.
- The precise completion-signal API in each language (a typed event on the stream, a handle await, or
  both) — the *requirement* (completion observable without stream-shape inference) is fixed; the
  surface is a plan-time call.
- Whether the Go local backend takes a pre-built `runtime.Runtime` or the config to build one.
- How the credential seam reconciles with #49's chosen mechanism, and how a browser SDK supplies its
  credential (Wander's token flow) — re-validated at the build reconcile pass.
