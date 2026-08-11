---
id: 49
slug: auth-multi-tenancy
title: Auth / multi-tenancy — loop_id ownership and per-tenant isolation for the deployed service
status: in-progress
priority: medium
type: feat
created: 2026-08-10
updated: 2026-08-11
depends_on: [48]
related: [45, 46, 47, 48, 50, 51, 53]
discovered_from: [45, 47]
adrs: []
spec: docs/superpowers/specs/2026-08-11-auth-multi-tenancy-design.md
plan: docs/superpowers/plans/2026-08-11-auth-multi-tenancy-plan.md
results:
trivial: false
auto_groomable:
branch: feat/auth-multi-tenancy
claimed_at: 2026-08-11T07:16:47Z
pr:
blocked_by:
reconciled: true
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-08-11-auth-multi-tenancy-design.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/2026-08-11-auth-multi-tenancy-design.md) |
| Plan | [2026-08-11-auth-multi-tenancy-plan.md](https://github.com/ethanhinson/fuse/blob/feat/auth-multi-tenancy/docs/superpowers/plans/2026-08-11-auth-multi-tenancy-plan.md) |
<!-- docket:artifacts:end -->

## Why

The networked binding (change 48) is single-tenant — every caller shares one trust domain, an
explicit non-goal carried forward from 0045. A **deployed** service that many users' apps target
needs authentication and multi-tenancy: each caller may only start, drive, observe, and replay the
loops it owns, and loops are isolated per tenant. Needs brainstorming: the identity/auth model,
loop_id ownership, and per-tenant isolation boundaries over the networked seam.

## What changes

An auth + multi-tenancy layer over the networked binding — now the **Connect/protobuf
`fuse.loop.v1`** transport (change 55, ADR-0033; the original change-48 WebSocket binding was
removed). Full design in the linked spec; at proposal altitude:

- **Bearer-token identity behind a pluggable `Verifier` seam.** A `Authorization: Bearer` token on
  each Connect request resolves to a `Principal{tenant, subject}` via a **Connect interceptor**
  (covering unary `StartLoop`/`Send` and the streaming `Observe`). A static/configured token verifier
  ships as the default; the seam is shaped so OIDC/JWT/mTLS verifiers slot in later without a re-cut
  (the same two-implementation discipline as the store seam). The verifier lives at the **binding
  edge** — `internal/runtime` gains no auth import and keeps the policy-free seam (ADR-0030).
- **Token is authoritative for tenant.** Tenant is derived solely from the verified principal; the
  wire `tenant` field (already present on every request, threaded but *unenforced*) is never trusted
  (validated must-match-or-rejected). `StartLoop` derives `(tenant, owner)` from the token, making
  the already-converged `(tenant, loop)` key authoritative rather than caller-asserted (finding 1).
- **Ownership binding + per-request authorization.** `StartLoop` records `Owner = subject` (a new
  authorization field, distinct from the existing `OwnerNodeID` liveness/instance id) in the durable
  registry; every `Send`/`Observe`/`Attach` authorizes against it. Cross-tenant → `CodeNotFound`;
  cross-owner (same tenant) → a distinguishable **`CodePermissionDenied`** (finding 2). Ownership is
  read from the durable registry (source of truth), not the in-memory cache.
- **Owner liveness lease (heartbeat + TTL).** `LoopRecord` gains a lease; the owning instance
  heartbeats to renew it; a cold instance treats `Live && lease-expired` as abandoned and may
  reap/re-own — the same lease shape docket uses for claim reclaim (finding 3). Added to both the
  fsstore and Postgres backends behind the existing conformance suite.
- **Reconnect is first-class and re-authorized.** The client-driven stateless reconnect (track last
  `Seq`, re-observe `from_seq=<lastSeq>`, ride subscribe-before-replay + dedup-at-watermark; already
  in the Connect `Observe` stream) is preserved and re-authorized on every request — a reconnect may
  land on a different instance (no sticky sessions, ADR-0033). **Re-own on reconnect:** when the
  reconnecting caller is the recorded owner and the prior owner's lease has expired (redeploy/crash),
  the new instance re-owns, renews the lease, and resumes serving — so "attach to your running loop
  from your phone after the server redeployed" works end-to-end.

### Findings folded in from the 0047 dogfood (discovered_from: 47)

The design closes three gaps a real `fuse loop-server` dogfood surfaced, all re-verified present on
`origin/main` @ e21cd22 at reconcile: (1) tenant is wire-trusted, not identity-derived — the field
is threaded but spoofable, with no `Principal`; (2) loop ownership (`OwnerNodeID`) is recorded but
unenforced on the wire, and there is no calling *subject* distinct from the node id; (3) a
hard-killed server leaves `Live: true` forever with no lease to distinguish live from abandoned. See
the spec for how each is closed.

## Out of scope

- The Connect/protobuf transport itself — changes 48/55 (done); the change-48 WS binding was removed.
- Client SDK ergonomics / versioned external wire envelope — change 50.
- Observability emission (OTEL, `/metrics`) — change 51; this change only carries the already-threaded
  context and `(tenant, loop, node)` triple.
- Rich verifiers (OIDC/JWT signature checking, mTLS, token issuance/rotation/revocation) — the seam
  is shaped for them; only the static bearer impl ships here.
- TLS termination / deployment topology / cross-instance load-balancing — operational concerns.
- A nested per-org→per-user isolation hierarchy — the boundary here is `(tenant, owner)`.
- Resuming a *completed* one-shot loop's execution on re-own (re-own serves replay for a finished
  loop; resuming a parked persistent loop rides change 53's persistence primitive).

## Reconcile log

### 2026-08-11 — reconciled against `origin/main` @ e21cd22

**Scope-adjustable reconcile (not a re-brainstorm).** The design intent — bearer-token identity,
token-authoritative tenant, per-owner authorization, an owner liveness lease with reap/re-own,
first-class re-authorized reconnect — is fully intact. The spec + body were refreshed to current
reality; no escalation warranted.

Material changes folded in:

- **Transport swap: WS/JSON-RPC → Connect/protobuf.** The spec was written against change 48's
  JSON-over-WebSocket + HTTP-replay binding (`coder/websocket`). That binding was **removed** by
  change 55 (ADR-0033, supersedes ADR-0032) and replaced with a Connect/protobuf `fuse.loop.v1`
  transport over h2c (`internal/loopconnect`, `internal/loopwire/v1`, `proto/fuse/loop/v1/loop.proto`,
  `cmd/fuse/loop_serve_net.go`). Re-mapped: WS handshake `Authorization` header → a `connect.Interceptor`
  (unary + `WrapStreamingHandler`); JSON-RPC error codes → Connect codes (`CodeUnauthenticated`,
  `CodePermissionDenied`, `CodeNotFound`); the browser `Sec-WebSocket-Protocol` token-carriage open
  question is closed (a plain HTTP header is browser-native via connect-es). The `Observe`
  server-stream already subsumes the old HTTP replay endpoint via `from_seq`.

- **Scope reductions — several anticipated sub-tasks are already done on `main`.** (a) The `Runtime`
  seam already carries tenant on `Send`/`Observe`/`Attach` + `LoopConfig.Tenant` (#47/#55) — no seam
  signature change needed. (b) The `r.loops` cache already re-asserts tenant on hit
  (`inProcRuntime.loopCache`) — the `cache-over-tenant-scoped-source-reassert-key-on-hit` learning is
  already applied. (c) All three proto requests already carry a `tenant` field, threaded but
  unenforced — no proto field additions needed for tenant.

- **Design sharpening: `Owner` (subject) vs `OwnerNodeID` (node).** The original spec conflated the
  authorization owner with the liveness node id. On `main`, `LoopRecord.OwnerNodeID` is the owning
  *instance* id (for liveness/re-own). Authorization needs a distinct **`Owner string`** = the
  principal's subject. The reconciled design adds `Owner` alongside `OwnerNodeID` on `LoopRecord` in
  both backends behind the conformance suite.

- **Finding restatement.** Finding 1 refined: the three verbs already agree on the tenant key (closed
  by #55), but the key is caller-asserted/spoofable, not identity-derived — the real gap this closes.
  Findings 2 and 3 verified present unchanged.

Dependencies: #47, #48, #53, #55 all `done` and merged to `main`; #53's `event.KindLoopParked`
completion event exists for re-own of a parked persistent loop. No obsolescence; no auto-capture
(disabled this repo).
