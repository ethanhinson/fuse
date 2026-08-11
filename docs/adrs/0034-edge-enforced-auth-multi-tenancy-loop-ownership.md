---
id: 34
slug: edge-enforced-auth-multi-tenancy-loop-ownership
title: Token-authoritative tenancy + edge-enforced loop ownership over the policy-free runtime seam
status: Accepted
date: 2026-08-11
supersedes: []
reverses: []
relates_to: [30, 31, 33]
change: 49
---

## Context

The networked loop binding (Connect/protobuf `fuse.loop.v1`, ADR-0033) threaded `tenant` as a present-but-UNENFORCED wire field and recorded `LoopRecord.OwnerNodeID` but never checked it — any caller could name any tenant and address any loop. Two standing invariants constrained how identity and isolation could be added. ADR-0030 mandates a policy-free `internal/runtime` seam: it takes `(ctx, tenant, loop, …)` values and imports no `cmd/fuse` and no auth, so the runtime carries no policy. ADR-0031 makes tenancy app-enforced in every key across two store backends (fsstore + pgstore) behind a single conformance suite. Change #49 (auth-multi-tenancy) needed to add authentication plus per-tenant and per-owner isolation WITHOUT regressing either invariant — the seam must stay policy-free, and the app-enforced tenancy model must not fork between backends.

## Decision

Authenticate and authorize at the Connect binding edge; keep the runtime seam policy-free; make the verified token authoritative for tenant; and add a durable, backend-uniform ownership + liveness model. Concretely:

- **Bearer-token identity behind a pluggable `Verifier` seam.** A `Verifier` resolves a bearer token to a `Principal{Tenant, Subject}`, with a default static token→principal implementation in `internal/loopauth`. The seam is import-light — it imports only `internal/event` — so both the Connect edge and the composition root can use it while `internal/runtime` never does. Rich verifiers (OIDC/JWT/mTLS) slot in behind this seam with no re-cut.
- **Identity + authorization live at the edge, not in the seam.** A `connect.Interceptor` (unary + streaming) resolves the bearer token to a `Principal` on the request context; the handler authorizes each verb. `internal/runtime` gains no auth import — ADR-0030 is preserved. The seam keeps taking `(ctx, tenant, loop, …)` values; the edge resolves the tenant/owner and threads them in.
- **Token is authoritative for tenant.** Tenant derives SOLELY from the verified `Principal`. A wire `tenant` field must be empty-or-equal to the principal's, else `CodePermissionDenied`; the wire field is never trusted as an input, only checked for agreement.
- **`Owner` is a NEW `LoopRecord` field distinct from `OwnerNodeID`.** `Owner` is the authorization subject; `OwnerNodeID` is the liveness/instance node id — the two were conflated in the original design. Ownership authorization reads `Owner` from the durable registry (the source of truth), never from the in-memory cache.
- **Authz error taxonomy mapped onto Connect codes.** Cross-owner within the same tenant → `CodePermissionDenied` (a distinguishable "forbidden"); cross-tenant or unknown → `CodeNotFound` (a tenant-scoped resolve miss); missing/invalid token → `CodeUnauthenticated`. This deliberately accepts a BOUNDED intra-tenant existence oracle — a caller can distinguish "exists but not mine" from "doesn't exist" WITHIN their own tenant — in exchange for debuggable authz; cross-tenant existence never leaks.
- **Owner liveness lease (heartbeat + TTL) in both backends.** `LoopRecord.LeaseExpiry` plus a `Heartbeat` registry method are added to fsstore and pgstore alike, behind the ADR-0031 conformance suite (no backend divergence). The owning instance renews while live; a cold instance treats `Live && lease-expired` as abandoned and may reap or re-own on resolve, enabling reconnect-and-resume after a redeploy.
- **No-verifier posture is fail-usable, not fail-open.** With no configured auth, a single loudly-logged built-in dev token is synthesized (→ the `_default` tenant) so local `loop-serve-net` stays usable, but EVERY request must still present a bearer token — the server never runs unauthenticated. The `loop_server.auth` config is a credential surface honored only from trusted config (ADR-0006/0019).

## Consequences

Enables: multi-tenant plus per-owner isolation is enforced on the wire while the runtime seam stays policy-free (ADR-0030 intact) and the tenancy model stays app-enforced uniformly across both backends (ADR-0031 intact); reconnect-to-a-different-instance-after-redeploy works via the durable registry plus lease re-own (the deployed "attach from your phone" story, over the ADR-0033 Connect streaming transport); and richer verifiers (OIDC/JWT/mTLS) drop in behind the `Verifier` seam with no re-cut. Costs / gives up: a bounded intra-tenant existence oracle is accepted for authz debuggability; the dev-token fallback ships a world-known credential if a deploy forgets to configure auth (mitigated by loud logging — a follow-up could randomize it); and ownership recording at `StartLoop` is a Resolve-then-Register upsert at the edge that can transiently lose one heartbeat's lease advance (self-healing; a dedicated `SetOwner` registry method would make it airtight). Relates to ADR-0030 (the policy-free multi-loop seam this preserves), ADR-0031 (the app-enforced tenancy + durable registry this extends with `Owner` and the liveness lease), and ADR-0033 (the Connect/protobuf transport whose present-but-unenforced `tenant` field this makes authoritative).
