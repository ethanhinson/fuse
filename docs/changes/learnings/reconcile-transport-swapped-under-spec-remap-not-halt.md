---
slug: reconcile-transport-swapped-under-spec-remap-not-halt
hook: "When the just-in-time reconcile finds the spec was written against a mechanism that a LATER change replaced under it (a transport, an API, a store), first ask whether the design DECISIONS still hold on the new mechanism. If they do, the spec is scope-ADJUSTABLE — re-map the mechanism-specific mechanics (handshake→interceptor, JSON-RPC code→connect.Code, HTTP replay→server-stream) and build, recording the re-map in the ## Reconcile log. Halt only when a decision itself is invalidated, not merely because the words name a dead mechanism."
topics: [reconcile, spec-drift, transport, design-vs-mechanism, build-loop]
changes: [49]
created: 2026-08-11
updated: 2026-08-11
promotion_state: candidate
promoted_to:
---

## Apply

A spec is authored at propose/brainstorm time; by the time `docket-implement-next` reconciles it,
the base branch has moved. A common and initially-alarming form of drift: the spec is written
entirely against some **mechanism** — a wire transport, an RPC style, a storage API — that a
*different, later* change **removed or replaced** on `origin/main` before this change was built. The
spec's every code-level instruction now names something that no longer exists.

The wrong reflex is to treat that as a fundamental-invalidation and halt. The right first question is
**"do the design *decisions* still hold on the new mechanism?"** — decisions live one altitude above
the mechanism. If they do, the spec is **scope-adjustable, not invalidated**: keep every decision,
**re-map only the mechanism-specific mechanics**, and build. Record the re-map as a dated
`## Reconcile log` entry (and, if a decision was sharpened, an ADR) so the divergence between the
spec's words and the built code is deliberate and traceable, never silent.

Concretely, the re-map is usually a small dictionary of mechanism substitutions, e.g.:

- WebSocket handshake header → **Connect/gRPC interceptor**
- JSON-RPC numeric error codes → **`connect.Code*`** taxonomy
- separate HTTP replay endpoint → the **server-stream already subsumes it**

Each substitution is mechanical *once you have decided the decision survives*. The judgment is the
survives-check, not the substitution.

**Halt (fundamental-invalidation) only when a decision itself dies** — e.g. the new mechanism makes a
load-bearing guarantee impossible, or removes the seam the design depended on. "The spec says
`WebSocket` and we now use Connect" is not that; "the design assumed an ordered replay the new
transport cannot provide" would be. Distinguish *the words name a dead mechanism* (adjust) from *a
decision no longer holds* (halt).

**Reconcile can also find scope SHRINKAGE, not just drift.** The same pass that catches the swapped
mechanism often finds that intervening changes already did some of the spec's anticipated work — a
seam that now already carries the field, a cache that already re-asserts the invariant. Subtract that
from the plan too, and record it: the built change is the spec **minus what reality already
provides**, plus what only it can add.

### Provenance

Change #49 (auth/multi-tenancy, PR #53). The spec was written against change #48's
JSON-over-WebSocket + HTTP-replay binding, which change #55 (ADR-0033, supersedes ADR-0032) had
**removed** in favor of a Connect/protobuf `fuse.loop.v1` transport over h2c before #49 was built.
All seven design decisions held, so the reconcile re-mapped the transport mechanics (WS handshake
header → Connect interceptor; JSON-RPC codes → `connect.Code*`; HTTP replay → the existing `Observe`
server-stream) rather than halting, and separately found scope *reductions* — the runtime seam
already carried tenant and the `r.loops` cache already re-asserted it (#47/#55), so #49 added only
edge-side identity/authz + the registry `Owner`/lease fields. Both the re-map and the shrinkage are
recorded in the change's `## Reconcile log` and ADR-0034. See also
[[cache-over-tenant-scoped-source-reassert-key-on-hit]] for the cache-re-assert half of that
shrinkage.
