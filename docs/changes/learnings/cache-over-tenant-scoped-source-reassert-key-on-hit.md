---
name: cache-over-tenant-scoped-source-reassert-key-on-hit
slug: cache-over-tenant-scoped-source-reassert-key-on-hit
title: An in-memory cache/projection over a tenant-scoped source of truth must re-assert the tenant on every hit — a bare loop_id hit returns the wrong tenant's data
hook: "Caching a projection over a tenant-scoped (or otherwise key-partitioned) source of truth? A cache keyed by the inner id alone returns the wrong tenant's object on a hit. Assert the cached entry's tenant matches the request, and fall through to the scoped source on mismatch."
promotion_state: candidate
changes: [47]
created: 2026-08-11
updated: 2026-08-11
topics: [go, multi-tenancy, cache, correctness, isolation]
---

## Apply

When you demote an in-memory map to a **cache/projection over a durable source of truth** whose
real key is a *compound* key — `(tenant_id, loop_id)`, `(org, resource)`, `(namespace, name)` — but
the cache is still keyed by only the **inner** id (because that map predates the tenant boundary),
a cache **hit** silently skips the tenant scoping the durable path enforces. The request for
`(tenantB, loop7)` hits the entry `loop7` that `tenantA` warmed, and you return the wrong tenant's
store. It passes every single-tenant test (there is only one tenant, so the id is unique) and only
bites once two tenants share an inner id — exactly the boundary you added the cache under.

**Rule:** a cache over a key-partitioned source is only sound if **every hit re-asserts the full
key**:

```go
if l, ok := r.loops[loopID]; ok && l.tenant == reqTenant {   // assert, don't just find
    return l.store
}
// mismatch OR miss → fall through to the tenant-scoped durable registry (source of truth)
return r.registry.Resolve(ctx, reqTenant, loopID)
```

Cache the compound key (`r.loops[StreamKey{tenant, loop}]`) if you can; when the map can't be
rekeyed cheaply, guard the hit. Either way the durable/scoped path stays the source of truth and
the cache is a pure accelerator — a mismatch is a *miss*, never a wrong answer. In change 0047
this was `Runtime.Send` threading `tenant` and the `r.loops` hit asserting
`l.tenant == request.tenant`, with regression tests `TestSendResolvesUnderRequestedTenant` and
`TestCacheHitDoesNotCrossTenants`.

**Related:** [[deglobalize-holder-also-per-instance-the-shared-graph]] — deglobalizing a holder to
per-instance state is only half the job; this is the *tenant* half of the same clobber hazard, one
layer down at the cache. [[live-control-reads-state-at-decision-point]] — read the authoritative
scoped state at the decision point, not a stale unscoped snapshot.
