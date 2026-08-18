---
id: 47
slug: optional-observer-capabilities
title: Optional observer capabilities extend observability behavior without widening the Observer interface
status: Accepted
date: 2026-08-17
supersedes: []
reverses: []
relates_to: [40]
change: 71
---

## Context

Change 0071 gave interactive loops turn-scoped trace roots: `fuse.loop.run` now ends at the
first park and each later Send→park exchange opens a `fuse.loop.turn` root span linked to the
session carrier. Two problems surfaced during its review that could not be solved from the
runtime alone:

1. A **resumed** loop deliberately starts no `fuse.loop.run` span (spec D4). But `Observer.Start`
   is also where the production composite observer (`cmd/fuse/observability.go:metricsObserver`)
   stamps its tenant key into the context, keyed off a `tenant` field that only the `loop.run`
   descriptor carries. Skipping the span therefore also skipped the metrics scope, so every
   resumed session recorded `tenant=""` across turn, model, tool and store metrics — silently
   mis-attributing change 0051's per-tenant Prometheus series for the life of the session.
2. `StartFromCarrier(..., delayed=true)` with a nil or invalid carrier fell through to a plain
   `Start`, which made `fuse.loop.turn` a CHILD of the already-ended session root instead of a
   new root — quietly defeating the whole change whenever a carrier could not be recovered.

The constraint is ADR-0040: the Observer contract is provider-neutral, and the runtime must not
type-assert to the concrete otel implementation.

## Decision

Extend observability behavior through **optional capability interfaces plus a free probe
function**, never by adding methods to the `Observer` interface — the pattern already
established by `TraceCarrierProvider` and `CarrierStarter` in `internal/observe/contracts.go`.

Concretely, change 0071 added `observe.ScopeDecorator` with the free function
`observe.DecorateScope(o, ctx, d)`: an observer that implements it can decorate a context with
loop-scope values (the metrics tenant key) WITHOUT starting a span, and one that does not
implement it degrades to a no-op. The resumed launch applies it with the same descriptor
`loop.run` would have carried, so the metrics scope survives without minting a second root.

Separately, and for the same neutrality reason, the "still a root" guarantee for delayed work
was fixed **in the otel adapter** rather than in the runtime: `StartFromCarrier` with
`delayed=true` and a nil/invalid carrier now starts an unlinked `trace.WithNewRoot()` span
instead of falling through to `Start`. The runtime cannot strip a span from a context
provider-neutrally, so the adapter is the only ADR-0040-clean place to honor the method's own
documented contract. Immediate (`delayed=false`) behavior is unchanged.

## Consequences

- Enables: per-observer capability growth with no breaking change to every `Observer`
  implementation, and no concrete-type assertions in `internal/runtime`. ADR-0040 holds.
- Costs: capability probing is invisible in the interface, so a reader must know to look for the
  optional interfaces; and a capability an observer silently lacks degrades quietly rather than
  failing loudly — the degradation paths need explicit tests (change 0071 added them).
- Gives up: compile-time enforcement that an observer supports scope decoration. A composite
  wrapper that forgets to forward `ScopeDecorator`/`TraceCarrier`/`StartFromCarrier` to its
  primary silently disables the feature; `metricsObserver` forwards all three.
- Note for future readers: the delayed-with-nil-carrier change also affects
  `internal/observe/runner.go`'s replay projection path — a projected span whose replayed event
  carries no valid trace is now an unlinked root rather than a child of the runner's context.
  That is the intended semantics of delayed work.
