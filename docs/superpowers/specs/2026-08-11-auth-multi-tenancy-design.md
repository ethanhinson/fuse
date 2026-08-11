<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0049 — Auth / multi-tenancy — loop_id ownership and per-tenant isolation for the deployed service](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0049-auth-multi-tenancy.md)**
<!-- docket:backlink:end -->

# Auth / multi-tenancy — loop_id ownership and per-tenant isolation for the deployed service

**Change:** #49 · **Status:** design (build-ready) · **Depends on:** #48 (done, superseded by #55), #47 (done), #55 (done, transport), #53 (done, persistent loop)
**Date:** 2026-08-11 · **Reconciled:** 2026-08-11 against `origin/main` @ e21cd22

## Context

> **Reconcile note (2026-08-11).** This spec was originally written against change #48's
> JSON-over-WebSocket + HTTP-replay binding (`coder/websocket`, JSON-RPC error codes, a WS
> `Authorization` handshake header). That binding was **removed** by change #55 (ADR-0033,
> supersedes ADR-0032) and replaced with a **Connect/protobuf `fuse.loop.v1`** transport over h2c.
> The design intent below is unchanged; the transport-specific mechanics are re-mapped onto Connect.
> Several sub-tasks the original spec anticipated are **already done on `main`** and are struck from
> scope (see D2/D3/D7). The three findings this change closes are all still present and verified on
> `origin/main` @ e21cd22.

The networked loop-control binding exposes the loop `Runtime` over the network as a Connect service
`fuse.loop.v1.LoopService` (unary `StartLoop`/`Send`, server-streaming `Observe` which subsumes
replay via `from_seq`), served with connect-go over h2c (ADR-0033). #47 made a loop's existence,
history, ownership, and liveness durable and cross-instance-reachable behind a backend-agnostic
durable store + loop registry, with `tenant_id` first-class and **app-enforced** in every key and
query (ADR-0031). #55 already threaded `tenant` through the seam and the wire as a typed field —
**present but unenforced**, deferred to this change.

But the networked binding today has **no caller identity**: `tenant` arrives on the proto request
and is threaded to the `Runtime` seam, but nothing verifies who the caller is or that they are
entitled to the loop they drive. A caller can name any `tenant` string and any `loop_id`. A
**deployed** service that many users' apps target needs authentication and multi-tenancy — each
caller may only start, drive, observe, and replay the loops it owns, and loops are isolated per
tenant.

Three concrete gaps, all verified present on `origin/main` @ e21cd22:

1. **Tenant is wire-trusted, not identity-derived.** `StartLoopRequest`/`SendRequest`/`ObserveRequest`
   each carry a `tenant` string (`proto/fuse/loop/v1/loop.proto`) that `internal/loopconnect`
   threads to the seam verbatim (`handler.go` `StartLoop`/`Send`, `observe.go` `Observe`). The field
   is honest for a cooperating client but **spoofable** — any caller can set any tenant. There is no
   `Principal`, no token, nothing that makes tenant authoritative. (The three verbs already agree on
   the key — that part of the original finding #1 is closed by #55 — but the key is caller-asserted,
   not verified.)
2. **Loop ownership is recorded but unenforced.** `LoopRecord.OwnerNodeID` (`internal/event/registry.go`)
   is set at `Register` and never checked against a caller. There is no notion of a calling *subject*
   distinct from `OwnerNodeID` (which is the owning *node/instance* id, for liveness — not a user).
   Nothing authorizes `Send`/`Observe`/`Attach` against the recorded owner.
3. **Stale liveness on hard process death.** `LoopRecord` has `OwnerNodeID` + `Live` + `CreatedAt` +
   `UpdatedAt` but **no lease/TTL/expiry**. `SetLive(false)` fires on clean completion
   (`internal/runtime/inproc.go`); a hard-killed instance skips it, leaving the record `Live: true`
   forever with a stale `UpdatedAt`, and nothing reaps it. A cold instance cannot distinguish
   genuinely-live from abandoned.

This change is the auth + multi-tenancy layer over the Connect seam that closes all three gaps,
**without regressing the policy-free seam** (ADR-0030: `internal/runtime` imports no `cmd/fuse` and
no auth package) or the **app-enforced** tenancy model (ADR-0031).

### What is already done on `main` (reduces scope vs the original spec)

- **The `Runtime` seam already carries tenant.** `Send(ctx, tenant, loopID, input)`,
  `Observe(ctx, tenant, loopID)`, `Attach(ctx, tenant, loopID, from)`, and `LoopConfig.Tenant`
  (`internal/runtime/runtime.go`). No seam signature change is needed for tenant — the original
  spec's "thread tenant to the seam" work is **complete**.
- **The `r.loops` cache already re-asserts tenant on hit.** `inProcRuntime.loopCache(tenant, loopID)`
  returns nil on a tenant mismatch and falls through to the tenant-scoped durable registry
  (`internal/runtime/inproc.go`), i.e. the `cache-over-tenant-scoped-source-reassert-key-on-hit`
  learning is **already applied**. No cache re-keying is needed.
- **The wire already carries `tenant` on all three requests** (`proto/fuse/loop/v1/loop.proto`), so
  no proto field additions are needed for tenant; the only possible proto change this brings is an
  OPTIONAL authz-error detail (below), and the leaning is not to add one.

## Decisions

### D1 — Bearer-token identity behind a pluggable `Verifier` seam, at the Connect edge

Caller identity is established by a **bearer token** on the `Authorization: Bearer <token>` request
header. A pluggable verifier turns the token into a principal:

```go
type Principal struct {
    Tenant  event.TenantID   // the authoritative tenant
    Subject string           // the owning identity (owner)
}

type Verifier interface {
    Verify(ctx context.Context, token string) (Principal, error)
}
```

- **Two-implementation discipline** (mirroring the store seam's two backends): the default shipped
  impl is a **static/configured bearer-token verifier** (token → principal from configuration), and
  the seam is shaped so an OIDC / JWT / mTLS verifier can slot in **without a seam re-cut**. Only the
  static impl ships in #49.
- **Where it lives — a Connect interceptor at the binding edge.** The verifier and the
  request→principal extraction live in the **binding layer** (`internal/loopconnect` +
  `cmd/fuse/loop_serve_net.go`), NOT in `internal/runtime`. A `connect.Interceptor` (covering
  `WrapUnary` for `StartLoop`/`Send` AND `WrapStreamingHandler` for the `Observe` stream) reads
  `Authorization` off the request header, calls `Verify`, and stashes the resulting `Principal` on
  the request `context.Context`. Header access: `req.Header().Get("Authorization")` for unary;
  `conn.RequestHeader().Get("Authorization")` in `WrapStreamingHandler`. This preserves ADR-0030's
  policy-free seam — `internal/runtime` gains no auth import; the seam continues to take
  `(ctx, tenant, loop, …)` values, with the interceptor having resolved identity at the edge.
- **Token carriage is trivial under Connect** — `Authorization: Bearer` is a plain HTTP header,
  browser-native via connect-es (no WS `Sec-WebSocket-Protocol` subprotocol trick needed). The
  original spec's browser-handshake open question is **closed** by the transport swap.
- **Missing/invalid token → `connect.CodeUnauthenticated`.**

### D2 — Token is authoritative for tenant; the wire `tenant` field is validated-or-ignored

Tenant is derived **solely from the verified principal** — the wire `tenant` field is never trusted:

- **`StartLoop`** takes tenant and owner from the principal. The proto `tenant` field is
  ignored-or-validated (if a client sends one, it **must equal** `principal.Tenant`, else
  `CodePermissionDenied`). The loop is registered under
  `StreamKey{Tenant: principal.Tenant, Loop: <id>}` with a new `Owner` = `principal.Subject`.
- **`Send` / `Observe`** likewise validate any wire `tenant` field must-equal `principal.Tenant`,
  else reject. The binding-edge shim passes `principal.Tenant` (not the wire field) to the seam call.
- The three verbs already converge on one tenant key (done by #55); this decision makes that key
  **authoritative** rather than caller-asserted.

### D3 — Ownership binding + per-request authorization; `Owner` (subject) distinct from `OwnerNodeID` (node)

`LoopRecord` gains an **`Owner string`** (the authorization subject) distinct from the existing
`OwnerNodeID` (the liveness/instance id — the two were conflated in the original spec). At
`StartLoop`, the record is written with `Owner = principal.Subject`. Every subsequent
`Send`/`Observe`/`Attach` resolves the loop through the durable registry (source of truth, ADR-0031)
and **authorizes** against the principal:

- **Cross-tenant**: the resolve is tenant-scoped (`StreamKey`), so a loop under another tenant does
  not resolve → treated as non-existent (D4).
- **Cross-owner (same tenant)**: the loop resolves, but `record.Owner != principal.Subject` →
  **`forbidden`** (D4).
- The authorization check reads ownership from the **durable registry** (`Resolve`), not the
  in-memory `r.loops` cache, for the same source-of-truth-on-correctness reason the tenant assertion
  already follows. Authorization is enforced at the **binding edge** (the interceptor / handler shim)
  so the policy-free seam is untouched — the seam never learns about `Owner`; only the durable
  `LoopRecord` and the edge do. (The registry `Resolve` returns the record, which now carries
  `Owner`, so the edge can compare without the seam importing auth.)

### D4 — Authz error taxonomy on Connect codes: distinguishable `forbidden`, accepted cross-tenant oracle

- **Missing/invalid token** → `connect.CodeUnauthenticated`.
- **Not yours (same tenant, other owner)** → `connect.CodePermissionDenied` (the distinguishable
  `forbidden`), distinct from not-found.
- **Doesn't exist (or under another tenant)** → `connect.CodeNotFound` (matching the existing
  `ErrLoopNotFound` mapping in `handler.go`).

The deliberate choice keeps authz errors debuggable at the cost of a **bounded intra-tenant existence
oracle**: a caller in tenant A naming a loop owned by tenant B learns nothing (collapses to
not-found via the tenant-scoped resolve), but *within* a tenant a caller can distinguish "exists but
not mine" from "doesn't exist." Acceptable for the deployment model. These map onto Connect's code
space directly — no custom JSON-RPC codes (that space belongs to the untouched stdio binding #2 in
`internal/loopserver/server.go`).

### D5 — Owner liveness lease (heartbeat + TTL) with reap/re-own

`LoopRecord` gains a **lease** so a cold instance can distinguish live from abandoned:

- Add a `LeaseExpiry time.Time` (or equivalent) to `LoopRecord` and to both backends — the fsstore
  `loop.json` sidecar AND the pgstore `loops` table (a new column) — extended symmetrically behind
  the existing `RunDurableStoreConformance` / registry conformance suite
  (`internal/event/eventtest/conformance.go`), so neither backend silently diverges (ADR-0031 parity
  contract). A registry method to renew the lease (e.g. `Heartbeat(ctx, key, ownerNode, expiry)` or
  folding it into `SetLive`) is added — chosen at build to fit both backends.
- **The owning instance heartbeats** to renew the lease while it drives the loop (renewal cadence <
  TTL, ~⅓ TTL). The `Observe` keepalive ticker already runs at 20s (`observe.go`) — the lease
  heartbeat is a separate server-side renewer on the owning-instance loop lifecycle in
  `internal/runtime/inproc.go` (where `SetLive(true/false)` already fire), NOT tied to whether any
  client is observing.
- **Reap / re-own semantics.** A cold instance that resolves a record with `Live == true &&
  now > LeaseExpiry` treats the loop as **abandoned** and may reap / re-own (D6). A quiet-but-alive
  loop is kept alive by the active renewer, so false-abandon is closed by the renewer, not a
  lazy-at-resolve check.
- TTL configurable with a sane default (tens-of-seconds range; renewal ~⅓ TTL) so redeploy
  detection is prompt without flapping.

### D6 — Reconnect is first-class and re-authorized every request; re-own on expired lease

Reconnect under Connect: a dropped `Observe` server-stream is normal (ADR-0033); the client tracks
its last `event.Seq` and re-observes with `from_seq=<lastSeq>`, riding the inherited
subscribe-before-replay + dedup-at-watermark (`ev.Seq <= last`) + gap-marker path (already in
`observe.go`). Auth preserves this exactly:

- **Every reconnect is a fresh Connect request that re-presents the bearer token and is
  re-authorized** against the same `(tenant, owner)` binding recorded at `StartLoop`. No server-side
  session state is needed — the token + durable registry suffice. A reconnect may land on a
  **different instance** (no sticky sessions, ADR-0033) and must still authorize + replay correctly
  over the durable cross-instance store.
- **Re-own on reconnect.** When the reconnecting caller **is the recorded owner** (token verifies to
  the same `(tenant, subject)`) and the loop's lease has **expired** (its prior owning instance died
  — redeploy/crash), the new instance **re-owns**: renews the lease, records itself as `OwnerNodeID`,
  and resumes serving `Observe`/replay from `from_seq` (and `Send`, subject to the loop being
  resumable — a persistent conversational loop per #53 parks and can resume, keyed off the explicit
  `event.KindLoopParked` completion event; a completed one-shot serves replay only).
- A reconnect by a **non-owner** (different subject, same tenant) → `CodePermissionDenied`; a
  **different tenant** → `CodeNotFound` (D4).

### D7 — Seam & invariant preservation

- `internal/runtime` imports no `cmd/fuse` and no auth package (ADR-0030). Identity + authorization
  are resolved at the Connect binding edge (interceptor + handler shim); the Runtime continues to
  receive `(ctx, tenant, loop, …)` values. The seam's tenant-threading is already done (#47/#55);
  this change adds only edge-side identity/authz and the registry's `Owner`/lease fields.
- Tenancy stays **app-enforced** in every key and query (ADR-0031), portable across fsstore and
  pgstore; the `Owner` + lease fields are added to **both** backends behind the conformance suite.
- Events stay born-plaintext (ADR-0024); the store remains sole Seq allocator with non-blocking
  `Append` (ADR-0025). Auth adds no event kinds and no Seq-model change.

## What changes (scope, one PR)

- A **`Verifier` seam** + a static/configured bearer-token impl in the binding layer.
- A **Connect interceptor** (`internal/loopconnect`) covering unary + streaming that extracts the
  bearer token, verifies it to a `Principal`, and stashes it on the request context; wired into the
  handler in `cmd/fuse/loop_serve_net.go`.
- **`StartLoop`** derives `(tenant, owner)` from the principal and records `Owner`; the wire `tenant`
  field is validated-must-equal-or-ignored.
- **Per-request authorization** at the edge on `Send`/`Observe`/`Attach`: cross-tenant → not found,
  cross-owner → permission-denied; ownership read from the durable registry.
- **`Owner string`** added to `LoopRecord` (distinct from `OwnerNodeID`) + a **lease field**
  (`LeaseExpiry`) in `registry.go`, the fsstore sidecar, and the pgstore `loops` table + schema, all
  behind the conformance suite; a lease-renew registry method.
- An **owner heartbeat renewer** in the owning-instance loop lifecycle (`internal/runtime/inproc.go`)
  + **reap/re-own** logic on resolve/reconnect. (This is the one place runtime-side code grows — it
  renews a lease and reaps an abandoned record; it does NOT import auth, it operates on the durable
  registry the seam already depends on.)
- Connect error mapping: `CodeUnauthenticated` (no/bad token), `CodePermissionDenied` (forbidden),
  `CodeNotFound` (cross-tenant / unknown).
- Config surface: verifier config (static token→principal map / secret source) and lease TTL.
- Tests: auth pass/deny, tenant-spoof rejection (wire tenant ≠ principal), cross-tenant not-found vs
  cross-owner permission-denied, lease expiry → reap, **reconnect happy path** (re-present token,
  `from_seq=lastSeq`, no loss/no dup — MUST drive live + replay concurrently per
  `replay-live-handoff-dedup-at-watermark`), and **re-own-on-reconnect after simulated owner death**.
  The registry `Owner`/lease additions run under the existing two-backend conformance suite. Live
  verification uses a cheap scripted `LLM_GATEWAY_URL` double — never Claude/Anthropic — and drives
  the real backend for at least one acceptance (per `smoke-over-fake-backend-proves-wire-not-system`).

## Out of scope

- **The transport itself** (Connect/protobuf binding) — changes #48/#55 (done).
- **Client SDK ergonomics / versioned external wire envelope** — change #50.
- **Observability emission** (OTEL spans, `/metrics`) — change #51; #49 only carries the existing
  threaded `context.Context` and the `(tenant, loop, node)` triple.
- **Rich verifiers** (OIDC/JWT signature verification, mTLS, token issuance/rotation/revocation) —
  the seam is shaped for them; only the static bearer impl ships here.
- **TLS termination / deployment topology / cross-instance load-balancing** — operational concerns
  (the transport is already reconnect-to-different-instance safe per ADR-0033).
- **Per-org vs per-user isolation hierarchy** — #49's boundary is `(tenant, owner)`.
- **Resuming a *completed* one-shot loop's execution** on re-own — re-own serves replay for a
  finished loop; resuming *driving* a parked persistent loop rides #53's persistence primitive (done).
- **The stdio binding #2** (`internal/loopserver`, JSON-RPC over stdio) — untouched; it keeps its own
  local trust model and its JSON-RPC error codes.

## Open questions (resolve at build)

- Exact lease representation symmetric across the fsstore `loop.json` sidecar and the pgstore `loops`
  table (new `lease_expiry timestamptz` column vs a TTL-interpreted `updated_at`), and the renew
  method shape (`Heartbeat` vs folding into `SetLive`). Leaning: an explicit `LeaseExpiry` field +
  a `Heartbeat` (or `RenewLease`) registry method, so `Live` and lease stay orthogonal.
- Exact interceptor vs per-handler-shim split for authorization (identity extraction is clearly the
  interceptor; the per-loop ownership check needs the resolved `LoopRecord`, so it likely sits in a
  thin handler shim after the interceptor has set the principal).
- Whether `SetLive(false)` on clean completion also clears/ignores the lease, and how reap interacts
  with a genuinely-completed (non-live) loop's durable history (replay must always remain available).
- Static verifier config shape (inline token→principal map vs secret file / env), and the
  local-dev/no-token path: leaning auth-required for the networked binding, with the stdio + local
  CLI bindings unaffected (they never hit the Connect edge). A configured "dev" token mapping to
  `_default` tenant keeps local `loop-serve-net` usable.
- Whether an authz failure needs any proto surface (a typed error detail) or Connect's code + message
  suffices for #50's SDK. Leaning: code + message; no proto change.

## Prior art / references

- **ADR-0033** — Connect/protobuf `fuse.loop.v1` transport (supersedes ADR-0032's WS binding); tenant
  present-but-unenforced awaiting this change; reconnect-to-different-instance resilience.
- **ADR-0031** — durable store + registry; app-enforced tenancy in every key/WHERE; registry is the
  source of truth for existence/liveness/ownership; `LoopRecord{OwnerNodeID, Live, CreatedAt, UpdatedAt}`.
- **ADR-0030** — policy-free seam; value-threading; `internal/runtime` imports no `cmd/fuse`.
- **ADR-0024 / ADR-0025** — plaintext events; sole Seq allocator; non-blocking `Append`.
- **Learning `cache-over-tenant-scoped-source-reassert-key-on-hit`** — already applied to
  `loopCache`; authorization reads ownership from the durable source of truth, same discipline.
- **Learning `replay-live-handoff-dedup-at-watermark`** — the reconnect test must force an append
  into the subscribe→replay gap; a sequential test can't see double-delivery.
- **Learning `smoke-over-fake-backend-proves-wire-not-system`** — drive the real backend for at least
  one acceptance; keep the hard no-loss/no-dup property on the authoritative side; make skips loud.
- **Learning `persistent-loop-needs-explicit-completion-event`** (#53) — re-own of a parked
  persistent loop keys off the explicit `event.KindLoopParked` event, not stream shape.
