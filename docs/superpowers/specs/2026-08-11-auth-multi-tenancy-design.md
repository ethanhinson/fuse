<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0049 — Auth / multi-tenancy — loop_id ownership and per-tenant isolation for the deployed service](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0049-auth-multi-tenancy.md)**
<!-- docket:backlink:end -->

# Auth / multi-tenancy — loop_id ownership and per-tenant isolation for the deployed service

**Change:** #49 · **Status:** design (build-ready) · **Depends on:** #48 (done), #47 (done)
**Date:** 2026-08-11

## Context

Change #48 exposed the loop `Runtime` over the network (WebSocket carries the full
`loop.start`/`loop.send`/`loop.observe` session + `loop.event` push; HTTP is a thin stateless
`Attach(loop_id, from)` replay endpoint). #47 made a loop's existence, history, ownership, and
liveness durable and cross-instance-reachable behind a backend-agnostic durable store + loop
registry, with `tenant_id` first-class and **app-enforced** in every key and query.

But the networked binding is **single-tenant**: `tenant_id` flows through *present but unenforced*,
there is no caller identity, and nothing on the wire checks that a caller is entitled to the loop it
drives. A **deployed** service that many users' apps target needs authentication and multi-tenancy —
each caller may only start, drive, observe, and replay the loops it owns, and loops are isolated per
tenant.

Building and dogfooding #47 (a real `fuse loop-server` loop persisted to Postgres, live-tailed
cross-process) surfaced three concrete gaps that live squarely in this change's boundary and are
verified present in the merged code on `origin/main`:

1. **`loop.start` carries no tenant → the binding is single-tenant.** `startParams` is
   `{task, model}` only, so every loop lands under `event.DefaultTenant` (`_default`), while
   `loop.send`/`loop.observe` already accept a `tenant` field. A `loop.send` under tenant X for a
   loop started via `loop.start` (which is `_default`) returns `runtime: loop not found`. The three
   verbs disagree on the key.
2. **Loop ownership is not enforced on the wire.** The durable registry records `OwnerNodeID` in
   `LoopRecord`, but nothing checks the caller sending/observing a loop is entitled to it.
3. **Stale liveness on hard process death.** `LoopRecord` has `OwnerNodeID` + `Live` + `CreatedAt`
   but **no lease/TTL/expiry field**. A loop-server killed mid-run leaves its record `Live: true`
   (it never reaches the clean-completion `SetLive(false)`), so a cold instance sees a "live" loop
   no process owns and cannot distinguish genuinely-live from abandoned.

This change is the auth + multi-tenancy layer over the networked seam that closes all three gaps,
**without regressing the policy-free seam** (ADR-0030: `internal/runtime` imports no `cmd/fuse`) or
the **app-enforced** tenancy model (ADR-0031).

## Decisions

### D1 — Bearer-token identity behind a pluggable `Verifier` seam

Caller identity is established by a **bearer token** presented on the WebSocket handshake and on the
HTTP replay request. A pluggable verifier turns the token into a principal:

```
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
  static impl ships in #49; richer verifiers are follow-ups.
- **Token carriage.** WS: `Authorization: Bearer <token>` on the upgrade request (fallback
  `Sec-WebSocket-Protocol` carrier documented for browser clients that cannot set arbitrary
  handshake headers — resolve the exact browser mechanism at build reconcile against the chosen WS
  library `github.com/coder/websocket`). HTTP replay: `Authorization: Bearer <token>`.
- **Where it lives.** The verifier and the request→principal extraction live in the **binding layer**
  (`cmd/fuse` / the networked-binding package), *not* in `internal/runtime`. The Runtime seam
  continues to take `(ctx, tenant, loop, …)` as values; identity is resolved to `(tenant, owner)` at
  the transport edge and threaded in. This preserves ADR-0030's policy-free seam — `internal/runtime`
  gains no auth import and no `cmd/fuse` import.

### D2 — Token is authoritative for tenant; `loop.start` binds owner

Tenant is derived **solely from the verified principal** — the wire `tenant` field is never trusted:

- **`loop.start`** takes tenant and owner from the principal. `startParams` no longer needs a client
  tenant field; the loop is registered under `StreamKey{Tenant: principal.Tenant, Loop: <id>}` with
  `OwnerNodeID`/owner set from `principal.Subject`. This is the fix for finding #1 — the three verbs
  now converge on the token-derived `(tenant, loop)` key.
- **`loop.send` / `loop.observe` / HTTP replay** ignore-or-validate the wire `tenant` field: if a
  client still sends one, it **must equal** `principal.Tenant`, else the request is rejected
  (`forbidden`). A caller cannot claim another tenant by setting the field — the spoofing hole is
  closed. (Retaining the field as validated-must-match keeps #48's wire shape byte-compatible for
  correct clients; a client that omits it is fine.)

### D3 — Ownership binding + per-request authorization

At `loop.start`, the durable registry records the loop under `(tenant, owner)` (owner =
`principal.Subject`). Every subsequent `loop.send` / `loop.observe` / HTTP-replay resolves the loop
through the durable registry (the source of truth, ADR-0031) and **authorizes** against the
principal:

- **Cross-tenant**: the resolve is already tenant-scoped (`StreamKey`), so a loop under another
  tenant does not resolve → treated as non-existent (see D4).
- **Cross-owner (same tenant)**: the loop resolves, but `record.owner != principal.Subject` →
  **`forbidden`** (see D4).
- The authorization check reads ownership from the **durable registry**, not from the in-memory
  `r.loops` cache, when correctness depends on it — the cache is an accelerator, and per the
  `cache-over-tenant-scoped-source-reassert-key-on-hit` learning (already applied in #47) every hit
  re-asserts the full `(tenant, loop)` key; ownership authorization follows the same
  source-of-truth-on-correctness rule.

### D4 — Authz error taxonomy: distinguishable `forbidden`, accepted cross-tenant oracle

- **Not yours (same tenant, other owner)** → a distinguishable **`forbidden` / not-entitled** error,
  distinct from not-found, for honest diagnostics.
- **Doesn't exist (or under another tenant)** → `loop not found`.

This is the deliberate choice to keep authz errors debuggable at the cost of a **cross-tenant
existence oracle** — a caller in tenant A who names a loop_id owned by tenant B learns nothing
(it collapses to not-found because the tenant-scoped resolve fails), but *within a tenant* a caller
can distinguish "exists but not mine" from "doesn't exist." The oracle is bounded to intra-tenant
loop_id existence, which is acceptable for the deployment model. Error codes map onto #48's existing
JSON-RPC error space (a new distinguishable code alongside the existing `codeInvalidParams`/
loop-not-found mapping — exact code assigned at build reconcile).

### D5 — Owner liveness lease (heartbeat + TTL) with reap/re-own

`LoopRecord` gains a **lease** so a cold instance can distinguish genuinely-live from abandoned:

- Add a lease-expiry concept to the registry record (a `LeaseExpiry`/`lease_until` timestamp, or
  renew the existing `updated_at` interpreted against a configured TTL — exact field shape chosen at
  build reconcile to fit both fsstore sidecar and Postgres backends symmetrically, honoring the
  ADR-0031 conformance-suite parity contract).
- **The owning instance heartbeats** to renew the lease while it drives the loop (renewal cadence <
  TTL). This is the same lease shape docket already uses for claim reclaim.
- **Reap / re-own semantics.** A cold instance that resolves a record with `Live == true &&
  now > lease_expiry` treats the loop as **abandoned**: it may reap it (and re-own on reconnect, D6).
  A quiet-but-alive loop is kept alive by the heartbeat, so the false-abandon risk is closed by the
  active renewer (not a lazy-at-resolve check).
- TTL is configurable with a sane default (build-reconcile: pick a default in the tens-of-seconds
  range so redeploy detection is prompt without flapping; renewal at ~⅓ TTL).

### D6 — Reconnect is first-class and re-authorized every handshake; re-own on expired lease

Reconnect is a **required, first-class** behavior of this change (not deferred). #48's reconnect is
client-driven and the server stateless: the client tracks its last `event.Seq` and re-observes
`from=<lastSeq>`, riding the inherited subscribe-before-replay + dedup-at-watermark path with gap
markers. Auth must preserve this exactly:

- **Every reconnect is a fresh WS handshake that re-presents the bearer token and is
  re-authorized** against the same `(tenant, owner)` binding recorded at `loop.start`. No server-side
  session state is required to make reconnect work — the token + the durable registry are sufficient.
- **Re-own on reconnect.** When the reconnecting caller **is the recorded owner** (token verifies to
  the same `(tenant, subject)`) and the loop's lease has **expired** (its prior owning instance
  died — the redeploy/crash case), the new instance **re-owns** the loop: it renews the lease,
  records itself as `OwnerNodeID`, and **resumes serving** `observe`/`replay` from `from=<lastSeq>`
  and `send` (subject to the loop being resumable — a persistent conversational loop per #53 parks
  and can resume; a one-shot that already completed serves replay only). This is what makes "attach
  to your running loop from your phone after the server redeployed" work end-to-end.
- A reconnect by a **non-owner** (different subject, same tenant) is `forbidden`; by a **different
  tenant** is `loop not found` (D4).

### D7 — Seam & invariant preservation

- `internal/runtime` imports no `cmd/fuse` and no auth package (ADR-0030 policy-free seam). Identity
  is resolved at the binding edge; the Runtime continues to receive `(ctx, tenant, loop, …)` values.
- Tenancy stays **app-enforced** in every key and query (ADR-0031), portable across fsstore and
  Postgres; the lease field is added to **both** backends behind the existing conformance suite so
  neither silently diverges.
- Events stay born-plaintext (ADR-0024); the store remains sole Seq allocator with non-blocking
  `Append` (ADR-0025). Auth adds no event kinds and no Seq-model change.

## What changes (scope, one PR)

- A **`Verifier` seam** + a static/configured bearer-token impl in the binding layer.
- **Token extraction** on the WS handshake and HTTP replay request → `Principal`.
- **`loop.start`** derives `(tenant, owner)` from the principal and records ownership; the client
  tenant field is dropped/derived.
- **Per-request authorization** on `loop.send` / `loop.observe` / HTTP replay: cross-tenant → not
  found, cross-owner → forbidden.
- **`loop.send`/`observe`/replay** validate any wire `tenant` field must-equal the principal's.
- A new **distinguishable authz error** on the JSON-RPC/HTTP surface.
- A **lease field** on `LoopRecord` in both backends + an **owner heartbeat renewer** in the
  owning-instance loop lifecycle; **reap/re-own** logic on resolve/reconnect.
- **Re-own-on-reconnect** wiring so an expired-owner loop resumes under the reconnecting owner.
- Config surface: verifier config (the static token map / secret source) and lease TTL.
- Tests: auth pass/deny, tenant-spoof rejection, cross-tenant not-found vs cross-owner forbidden,
  lease expiry → reap, **reconnect happy path** (re-present token, `from=lastSeq`, no loss/no dup —
  MUST drive live + replay concurrently per `replay-live-handoff-dedup-at-watermark`), and
  **re-own-on-reconnect after simulated owner death**. Live verification uses a cheap scripted
  `LLM_GATEWAY_URL` double — never Claude/Anthropic.

## Out of scope

- **The transport itself** (WS/HTTP binding) — change #48 (done).
- **Client SDK ergonomics / versioned external wire envelope** — change #50.
- **Observability emission** (OTEL spans, `/metrics`) — change #51; #49 only carries the existing
  threaded `context.Context` and the `(tenant, loop, node)` triple already exposed.
- **Rich verifiers** (OIDC/JWT signature verification, mTLS, token issuance/rotation/revocation
  infrastructure) — the seam is shaped for them; only the static bearer impl ships here.
- **TLS termination / deployment topology / cross-instance load-balancing** — operational concerns.
- **Per-org vs per-user isolation hierarchy** — #49's boundary is `(tenant, owner)`; a nested
  org→user hierarchy, if wanted, is a later refinement.
- **Resuming a *completed* one-shot loop's execution** on re-own — re-own serves replay for a
  finished loop; resuming *driving* a parked persistent loop rides #53's persistence primitive.

## Open questions (resolve at build reconcile)

- Exact JSON-RPC error code for `forbidden` alongside #48's existing mapping, and the HTTP status
  (403 vs 404 mirroring D4's collapse).
- Browser WS token carriage: header vs `Sec-WebSocket-Protocol` subprotocol trick under
  `coder/websocket` — verify what the client SDK (#50) will actually be able to set.
- Lease field representation that is symmetric across fsstore sidecar JSON and the Postgres
  `loops` table, and the exact TTL default + heartbeat cadence.
- Whether `SetLive(false)` on clean completion should also clear the lease, and how reap interacts
  with a genuinely-completed (non-live) loop's durable history (replay must always remain available).
- Static verifier config shape (inline token→principal map vs a secret file / env), and the empty/
  default-tenant compatibility path for local single-tenant dev (no token → `_default`, or auth
  strictly required — leaning: auth required for the networked binding, local CLI bindings unaffected).

## Prior art / references

- **ADR-0031** — durable store + registry; app-enforced tenancy in every key/WHERE; registry is the
  source of truth for existence/liveness/ownership; `LoopRecord{OwnerNodeID, Live, CreatedAt}`.
- **ADR-0030** — policy-free seam; value-threading; `internal/runtime` imports no `cmd/fuse`.
- **ADR-0024 / ADR-0025** — plaintext events; sole Seq allocator; non-blocking `Append`.
- **Learning `cache-over-tenant-scoped-source-reassert-key-on-hit`** — the `r.loops` cache re-asserts
  `(tenant, loop)` on every hit; authorization reads ownership from the durable source of truth.
- **Learning `replay-live-handoff-dedup-at-watermark`** — the reconnect test must force an append
  into the subscribe→replay gap; a sequential test can't see double-delivery.
- **Learning `persistent-loop-needs-explicit-completion-event`** (#53) — re-own of a parked
  persistent loop keys off the explicit completion/park event, not stream-shape.
