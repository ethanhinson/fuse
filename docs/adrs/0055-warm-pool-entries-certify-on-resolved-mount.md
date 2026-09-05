---
id: 55
slug: warm-pool-entries-certify-on-resolved-mount
title: A warm sandbox pool entry is certified on its resolved mount, not only on its Principal
status: Accepted
date: 2026-09-04
supersedes: []
reverses: []
relates_to: [44, 34, 30]
change: 65
---

## Context

Change 0065 makes the container bind-mount root a function of `Principal.Tenant`, resolved
per-`Acquire`. `internal/tools/sandbox/pool.go` already partitioned warm entries by the full
`loopauth.Principal` and re-asserted ownership on every cache hit via `certifyPrincipal` — the
defence the docket learning `cache-over-tenant-scoped-source-reassert-key-on-hit` describes.

The question at build time: once a *mount* is derived from the Principal, is that Principal check
sufficient on its own?

## Decision

No. The pool now ALSO pins the mount root resolved at cold start on the `poolEntry`, and refuses any
warm hit whose Runner no longer agrees.

- `certifyPrincipal` was renamed `certifyEntry`.
- A new unexported `mountScoped` optional interface (mirroring the existing `principalScoped`)
  exposes the Runner's resolved root.
- A mismatch takes the identical `claimTeardown` / `dropLocked` / `CauseStaleCheckout` discard path
  as a principal mismatch.

The pool cannot RE-DERIVE a tenant's root — host layout belongs to the composition root, and
resolution needs the authenticated Principal that `Acquire` had in hand — but it can PIN what was
resolved and refuse disagreement. That is the strongest statement this layer can make alone.

**Why this is not merely a test concern** — the load-bearing part of the record. The build first
considered asserting the mount only in tests, reasoning that the root is derived deterministically
from a Principal that `certifyEntry` already compares, so a certified entry structurally carries the
right root. That argument is true today and was still rejected: a test-only assertion goes GREEN
under a refactor that decouples the root from the Principal (e.g. memoising the first resolved root
handler-wide). That exact mutation was run — it turned the observable argv red in the concurrent
two-tenant test, while a test-only mount assertion would not have caught the pool-level hole. Two
independent mutations pin both halves: the decoupling refactor, and deleting the `mountScoped`
comparison.

## Consequences

- One optional-interface check per warm hit — negligible cost.
- A Runner type that does not implement `mountScoped` certifies as before, so non-container
  substrates are unaffected.
- A future substrate whose mount legitimately changes across an entry's life would have to revisit
  this decision.
- The guard is what makes the isolation property survive refactors rather than depend on a
  currently-true derivation.
