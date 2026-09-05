<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0065 — bash per-tenant filesystem isolation — a Principal.Tenant-scoped bind-mount the working_dir cannot escape](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0065-bash-per-tenant-filesystem-isolation.md)**
<!-- docket:backlink:end -->

# bash per-tenant filesystem isolation — design

Change #65 · ADR-0044 (and its 2026-08-16 Update) · depends on #63 (done)

## The gap, stated precisely

ADR-0044 decided that "hosted filesystem access is a per-tenant bind-mount scoped by
ADR-0034 `Principal.Tenant`; the model-supplied `working_dir` resolves within the mount
and cannot escape it."

Change #63 built **the second half of that sentence and none of the first.**

What #63 shipped, and this change inherits unchanged:

- `WithTrustedRoot` (`internal/tools/sandbox/service.go`) — the bind-mount source, supplied
  by the composition root, documented SECURITY-CRITICAL as never model-derived.
- `containerHandler.workspace()` (`internal/tools/sandbox/container.go`) — the containment
  algorithm: relative paths join the root, both sides are canonicalised through
  `EvalSymlinks`, escape is rejected via `filepath.Rel` + `..` prefix test, non-directories
  and unresolvable paths are refused, and the host path is never echoed back.
- `resolveMountRoot()` — canonicalises the mount source and returns `""` for anything it
  cannot prove is an existing directory, so a bind-mount source is never invented.
- `ErrWorkingDirRefused` / `ErrNoTrustedRoot`, and the degraded-safe state: no trusted root
  ⇒ nothing mounted ⇒ any `working_dir` refused.

**What is missing is tenancy.** `h.root` is a single process-wide value resolved once at
startup (`cmd/fuse/sandbox.go`, `NewServiceFromRoot`). Grepping `Tenant` across
`internal/tools/sandbox/` returns hits only in `admission.go` and `config.go` — the #0077
concurrency gate. The filesystem layer has no notion of tenant at all.

This matters because **fuse does not run one process per tenant.** ADR-0030: "one process
hosts N concurrent loops." `loop_server.auth` is a *list* of token→principal entries, each
with its own `tenant`, all served by one `loop-serve-net` process; the permanent CI
acceptance lane drives two authenticated principals through one server concurrently. So
today every tenant's `bash` in a hosted deployment shares one mount.

## Decision 1 — derive the root per-Acquire, inside the sandbox package

The mount root becomes a function of the `loopauth.Principal`, resolved at
`Pool.Acquire` / `Service.Acquire` time rather than frozen at construction.

**Why here.** `Pool.entries` is already `map[loopauth.Principal]*poolEntry` — warm containers
are *already* partitioned by the full Principal, and `certifyPrincipal` re-asserts ownership
on every cache hit rather than trusting the map key (the
`cache-over-tenant-scoped-source-reassert-key-on-hit` learning is precisely this defence).
The Principal is therefore already in hand at exactly the moment a root must be chosen, and
deriving the mount there makes the filesystem follow the partition the pool already enforces
instead of inventing a second, parallel one. Any other placement has to re-plumb an identity
the package already holds.

**Trust direction is unchanged.** The tenant comes from the authenticated loop-start context
(ADR-0034 `Principal.Tenant`, established at the Connect edge), never from the `command`,
never from `working_dir`, never from any model output — ADR-0044's inherited ADR-0036
constraint (1). This change moves *which* root is selected; it does not move *who* selects it.

**Layout policy stays in the composition root.** The sandbox package receives a way to map a
Principal to a root; it does not invent host directory layout. Concretely: a resolver
supplied alongside the existing SECURITY-CRITICAL options (`WithHostedPosture`,
`WithTrustedRoot`), so the package stays layout-agnostic and the policy lives where the
other trusted knobs already live. `WithTrustedRoot`'s single-root behaviour remains the
local/single-tenant default — the resolver is the hosted-profile widening, not a replacement.

**The containment algorithm is not rewritten.** `workspace()` keeps its exact logic; the
only change is that the root it resolves against is the tenant's rather than the handler's.
Every property #63 established — canonical comparison, `..` rejection, symlink resolution,
no host-path disclosure, refusal over fallback — is inherited by construction. This is the
single most important implementation constraint in this spec: **do not reimplement
containment while making it tenant-aware.**

### Required properties

1. Two principals with different `Tenant` values resolve to different, non-overlapping roots.
2. A `working_dir` from tenant A can never resolve inside tenant B's root — the existing
   escape test, applied against the *caller's* root.
3. No trusted root resolvable for a principal ⇒ the existing degraded-safe state (mount
   nothing, refuse any `working_dir`), never a fallback to a shared or parent root.
4. A warm container is never reused across principals — already guaranteed by
   `certifyPrincipal`; this change must not weaken it, and the mount must be part of what
   makes an entry certifiable rather than an attribute assumed constant.
5. Local/single-tenant posture is byte-identical to today when no resolver is configured.

## Decision 2 — microVM: seam only, container is the sole implementation

ADR-0044's 2026-08-16 Update requires the same tenant-scoped, non-escaping mount for the
microVM handler, expressed VM-natively (per-tenant virtio-fs share **or** per-tenant block
image).

**No microVM handler exists in the tree.** Building its filesystem story now would be
speculative. This change therefore defines the tenant→root seam so a future microVM handler
can satisfy it, implements it for the container handler only, and **records the binding
conditions** the future handler must meet:

- The VM-native backing is per-tenant (virtio-fs share or block image — one backing per
  boundary mechanism, not both).
- `working_dir` remains non-escaping within that backing, with the same canonicalise-then-
  compare discipline rather than a guest-side check the guest could influence.
- Per ADR-0044's Update, warm/snapshot pools stay strictly per-principal and reset; a
  tenant-scoped mount must not become the thing that makes a snapshot reusable across
  principals.

Documenting these here is deliberate: the constraint is inherited by the future handler
rather than rediscovered when it is written.

## Decision 3 — a persistent per-tenant container, and #74's health emitter lands here

#0074 (`sandbox health emitter`) was deferred into this change on 2026-09-04, because four
of `SandboxHealthPayload`'s six reasons are unobservable on the current stateless-per-`Exec`
substrate (`docker run --rm`, nothing outliving an `Exec`, `ContainerID` always `""`).
**Both obligations land in this change.**

This is the one decision that *widens* scope rather than narrowing it, so state the
consequence plainly: a per-tenant mount is most useful over a container that persists across
`Exec`s, and that persistence is also what makes `ContainerID` real and a health probe
possible. Those two facts are the same fact, which is why #74 belongs here rather than after.

- **Populate `ContainerID`.** Once a container outlives an `Exec`, the field carries a real
  value instead of `""`, in the acquire/release/reap payloads as well as health.
- **Land the emitter, with the reasons the substrate can honestly observe.** `pull_failed`
  and `acquire_failed` are reachable already (`container.go` has a real single-flight
  pre-pull with its own `pullErr`; `pool.acquireFresh` sees the acquire error).
  `oom` / `runtime_exit` need an exit-code classifier separating substrate failure (e.g.
  exit 137) from an ordinary non-zero command exit — in scope here.
  `unresponsive` / `recovered` **are DEFERRED back to #74 by the reconcile of 2026-09-05**,
  under this decision's own if-and-only-if: the build does NOT produce a long-lived
  container (see the amendment below), so neither reason is honestly observable and
  emitting either would be the fabrication the next bullet forbids.
- **Never fabricate a signal.** #74's out-of-scope rule carries over verbatim: an emit
  inserted purely to make the metric non-zero is worse than an empty metric, because an
  operator would trust it.
- **Flip the #63 tripwire.** `internal/tools/sandbox_metrics_e2e_test.go` currently asserts
  `fuse_sandbox_unhealthy_total` gains *no* series from a real run, with a failure message
  instructing whoever lands an emitter to extend it. That guard must be flipped to drive a
  real unhealthy transition and assert the family moves on a live `/metrics` scrape.
- Payload discipline is unchanged: closed enum only, never raw error text, command, or
  environment — the same rule `sandboxCause` already enforces.

### Amendment 2026-09-05 (reconcile) — no long-lived container in this change

Decision 3 made `unresponsive`/`recovered` conditional on this change actually producing a
persistent container, and required the spec be **amended rather than the emitter faked** if it
did not. It does not, so this is that amendment.

Verified on `origin/main` @ `51dfc48`: `(*containerRunner).argv` builds `run --rm` per Exec
(`container.go:557-560`), `(*containerRunner).Release` documents "There is no container to stop"
(`container.go:835-843`), and `containerIdentified` (`pool.go:169-176`) is documented as
implemented by nothing. Warm pooling reuses a `*containerRunner` **object** — its env and egress
lease — never a running container.

**A per-tenant mount does not require persistence.** The mount source is chosen at `Acquire`
from the `Principal` and handed to each `run --rm`; that is the whole of Decision 1, and it is
unaffected. Converting the substrate to long-lived containers is a separate change with its own
lifecycle, reap, and cross-Exec state-leakage design — never scoped here, and adopting it
silently mid-build would be the larger error.

Consequences, and the scope that stands:
- **In scope, unchanged:** per-tenant mount root (Decision 1), the microVM seam (Decision 2),
  the `oom`/`runtime_exit` exit-code classifier, `pull_failed`, `acquire_failed`.
- **`ContainerID` stays `""`** on this substrate. The `containerIdentified` seam is left exactly
  as it is; populating it would require inventing an id for a container that does not outlive
  the Exec.
- **Deferred back to #74** (`deferred`, `depends_on: [63]`): `unresponsive`, `recovered`, and a
  real `ContainerID` — all three gated on a persistent-container substrate.
- **The #63 tripwire in `internal/tools/sandbox_metrics_e2e_test.go:163-171` is still flipped**,
  because this change *does* land a real emitter: the E2E must drive an honestly-observable
  unhealthy transition and assert `fuse_sandbox_unhealthy_total` moves on a live `/metrics`
  scrape. The guard asserts "no emitter exists"; that stops being true here.

## Out of scope

- The container substrate, runtime seam, off-switch, env-scrub (#63) and egress control
  (#64) — both `done`; this change consumes them.
- PaaS provider-managed volumes — ADR-0044's Update defers these to the PaaS ADR (#75).
- Deriving the tenant or principal from anything the model supplies — forbidden, not deferred.
- Rewriting `workspace()`'s containment algorithm (see Decision 1).
- A microVM handler itself (#75 territory); only the seam and its binding conditions are here.

## Verification

The security properties are the deliverable, so they are asserted directly, not implied:

1. **Cross-tenant escape, negative test.** Two principals, distinct tenants; every
   `working_dir` form that reaches for the other tenant's tree (`..` chains, absolute path,
   symlink pointing out of the root, doubled separators) is refused by `ErrWorkingDirRefused`
   — reusing the shapes #63's tests already cover, now across a tenant boundary.
2. **Isolation, positive test.** Tenant A writes a file; tenant B's container cannot see it.
   This is the property an operator actually cares about and must not be left to inference
   from the negative tests.
3. **No warm-container bleed.** A container warmed for principal A is never handed to
   principal B — extend the existing `certifyPrincipal` coverage to assert the *mount*
   differs, not merely that the entry was rejected.
4. **Degraded-safe.** An unresolvable per-tenant root mounts nothing and refuses
   `working_dir`; it never falls back to a shared or parent root.
5. **Local parity.** With no resolver configured, the container argv is byte-identical to
   today's — the single-tenant path must not regress.
6. **Health end-to-end.** Drive at least one real unhealthy transition against a real
   container and assert `fuse_sandbox_unhealthy_total{...}` moves on the live `/metrics`
   scrape; flip #63's negative guard as part of the same change. Per the 2026-09-05
   amendment the transition driven must be one this substrate can honestly observe
   (`pull_failed`, `acquire_failed`, or a classified `oom`/`runtime_exit`) — never
   `unresponsive`/`recovered`.
7. Run under `-race`, consistent with the repo's CI `unit-race` lane.

## Open questions for build time

- Host-side layout of per-tenant directories (naming, permissions, who creates them) — a
  composition-root policy question, deliberately left to the resolver's implementation.
- Lifecycle and cleanup of per-tenant mounts relative to ADR-0034 ownership/lease: when a
  tenant's lease expires, what happens to its tree.
- How the working tree the model edits is presented within a per-tenant mount for the
  *local* single-tenant case, where the repo root is the tree.
- Whether the health emitter attaches as a new `sandbox.PoolHooks` field or a sibling seam —
  health is not a pool checkout/hand-back/reap event, which is the tension #74 named.
