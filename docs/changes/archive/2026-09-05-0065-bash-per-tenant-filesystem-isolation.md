---
id: 65
slug: bash-per-tenant-filesystem-isolation
title: bash per-tenant filesystem isolation — a Principal.Tenant-scoped bind-mount the working_dir cannot escape
status: done
priority: medium
type: feat
created: 2026-08-16
updated: 2026-09-05
depends_on: [63]
related: [63, 64, 74, 75, 77]
discovered_from: [58]
adrs: [44, 55, 56, 57]
spec: docs/superpowers/specs/2026-09-04-bash-per-tenant-filesystem-isolation-design.md
plan: docs/superpowers/plans/2026-09-05-bash-per-tenant-filesystem-isolation.md
results: docs/results/2026-09-05-bash-per-tenant-filesystem-isolation-results.md
trivial: false
auto_groomable:
branch: feat/bash-per-tenant-filesystem-isolation
claimed_at: 
pr: https://github.com/ethanhinson/fuse/pull/87
blocked_by:
reconciled: true
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-09-04-bash-per-tenant-filesystem-isolation-design.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/2026-09-04-bash-per-tenant-filesystem-isolation-design.md) |
| Plan | [2026-09-05-bash-per-tenant-filesystem-isolation.md](https://github.com/ethanhinson/fuse/blob/feat/bash-per-tenant-filesystem-isolation/docs/superpowers/plans/2026-09-05-bash-per-tenant-filesystem-isolation.md) |
| Results | [2026-09-05-bash-per-tenant-filesystem-isolation-results.md](https://github.com/ethanhinson/fuse/blob/feat/bash-per-tenant-filesystem-isolation/docs/results/2026-09-05-bash-per-tenant-filesystem-isolation-results.md) |
| PR | [#87](https://github.com/ethanhinson/fuse/pull/87) |
| ADRs | [ADR-0044](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0044-bash-tool-contained-not-credentialed.md), [ADR-0055](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0055-warm-pool-entries-certify-on-resolved-mount.md), [ADR-0056](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0056-sandbox-health-is-a-sibling-hooks-seam-emitting-only-observable-reasons.md), [ADR-0057](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0057-unsafe-tenant-ids-are-refused-never-normalised.md) |
<!-- docket:artifacts:end -->

## Why

ADR-0044 decided that **hosted filesystem access is a per-tenant bind-mount** scoped by ADR-0034 `Principal.Tenant`, and that the model-supplied `working_dir` resolves *within* that mount and **cannot escape it**. This is the third of ADR-0044's three deferred follow-ons (container substrate first, then egress control, then per-tenant filesystem isolation). It depends on the container substrate from Change #63 — there is no mount to scope without the container. The design decisions are recorded in ADR-0044 — this stub tracks turning that deferral into build-ready work.

## What changes

Change #63 already built the `working_dir` containment half of ADR-0044's rule
(`workspace()`, `resolveMountRoot`, `ErrWorkingDirRefused`, `WithTrustedRoot`). What is
missing is **tenancy**: `h.root` is one process-wide value fixed at startup, and one fuse
process hosts N loops across N tenants (ADR-0030), so today every tenant's `bash` shares one
mount.

- **Make the mount root a function of `Principal.Tenant`, derived per-Acquire inside the
  sandbox package.** `Pool.entries` is already keyed by the full `loopauth.Principal` with
  `certifyPrincipal` re-asserting on cache hits, so the identity is already in hand where the
  root must be chosen; the filesystem then follows the partition the pool already enforces.
  Host layout policy stays in the composition root, alongside the other SECURITY-CRITICAL
  options. The containment algorithm is **inherited unchanged** — only the root it resolves
  against becomes per-tenant.
- **microVM: seam only.** Define the tenant→root seam so a future microVM handler can satisfy
  it, implement it for the container handler alone, and record the binding conditions
  (per-tenant virtio-fs share or block image; non-escaping `working_dir`; per-principal
  snapshot pools) so the future handler inherits them.
- **Land #74's health emitter here** (deferred into this change 2026-09-04), **partially** —
  narrowed by the 2026-09-05 reconcile. Emit the reasons this substrate can honestly observe
  (`pull_failed`, `acquire_failed`, and a new exit-code classifier for `oom`/`runtime_exit`),
  and flip #63's E2E tripwire that currently asserts `fuse_sandbox_unhealthy_total` stays
  unfed. **`unresponsive`, `recovered`, and a real `ContainerID` go back to #74**: the build
  produces no long-lived container (`run --rm` per Exec is unchanged), so those three are not
  honestly observable here, and the spec's Decision 3 required amending rather than faking
  them. Never fabricate a health signal.

## Out of scope

- The container substrate, runtime seam, off-switch, and env-scrub — Change #63 (a hard dependency).
- Egress / network policy — Change #64.
- Local (single-tenant) filesystem posture beyond what the #63 substrate already mounts in — the per-tenant scoping is a hosted-profile concern.
- Deriving the tenant/principal from anything the model supplies — explicitly forbidden.
- **PaaS provider-volume work** — deferred to the future PaaS ADR (ADR-0044's 2026-08-16 Update). This change scopes the tenant mount for the container and microVM backings only; provider-managed volumes for a remote/PaaS backend are out-of-scope here.

## Open questions

<!-- Design settled in the linked spec; these are build-time details it defers. -->
- Host-side layout of per-tenant directories (naming, permissions, who creates them) — left
  to the resolver's implementation in the composition root.
- Lifecycle/cleanup of per-tenant mounts relative to ADR-0034 ownership/lease.
- How the working tree is presented within a per-tenant mount for the local single-tenant case.
- Whether the health emitter attaches as a new `sandbox.PoolHooks` field or a sibling seam.

## Reconcile log

### 2026-09-05 — reconcile before build (docket-implement-next)

Verified every load-bearing claim against `origin/main` @ `51dfc48` (not the working tree, per
the `reconcile-verify-claims-against-origin-not-working-tree` learning). Dependencies #63
(PR #79) and #64 (PR #84) are both merged and present.

**Spec claims confirmed:**
- `Tenant` appears in `internal/tools/sandbox/` only in `admission.go` (the #0077 concurrency
  gate) — the filesystem layer has no tenant notion. The gap is real and as described.
- `containerHandler.root` is a single process-wide value; `cmd/fuse/sandbox.go` derives it from
  `os.Getwd()` and `NewServiceFromRoot` applies `WithTrustedRoot` LAST.
- `Pool.entries` is `map[loopauth.Principal]*poolEntry` and `certifyPrincipal` re-asserts on
  every hit, including a `principalScoped` runner check — the Principal is in hand at Acquire,
  exactly where Decision 1 places the root derivation.
- `workspace()` containment (canonicalise, `EvalSymlinks`, `filepath.Rel` + `..`, non-directory
  refusal, no host-path disclosure) is intact and is inherited unchanged.

**Corrections to the spec's picture:**
1. A survey pass reported `resolveMountRoot` as never called in production. **False** — it is
   applied at `container.go:277` inside `newContainerHandler`, after options, canonicalising
   `h.root` once at construction. No remediation needed; recorded so the claim is not re-raised.
2. `container.go:635-643` already carries a `TODO(#0065)` at the single `-v` mount site naming
   this change, with an explicit invariant to preserve: the source is only ever the trusted
   root, and `""` means mount nothing rather than substitute.

**Scope narrowed — Decision 3's own conditional fired.** No container outlives an `Exec`
(`run --rm` per Exec; `Release` states "There is no container to stop"; `containerIdentified` is
implemented by nothing). Per-tenant mounts do not require persistence, so Decisions 1 and 2 are
unaffected; but `unresponsive`/`recovered` and a real `ContainerID` are not honestly observable
without a long-lived container, so all three defer back to #74 (still `deferred`, `depends_on:
[63]`). The spec carries a dated amendment recording this. Building a persistent-container
substrate was never in this change's scope and is not being adopted silently.

**Build-time constraints folded in from the learnings ledger:**
- `security-knob-inert-at-composition-root` — #64 shipped the enforcing object unwired in
  `cmd/fuse`. The tenant resolver must carry a composition-root wiring assertion, not only
  package-level unit tests.
- `trusted-root-never-model-selectable` — the per-tenant root must be applied on the trusted
  side and never reachable from `working_dir` or any model output; the resolver widens *which*
  root, never *who* chooses it.
- `cache-over-tenant-scoped-source-reassert-key-on-hit` — the warm-entry check must assert the
  resolved MOUNT, not merely the Principal, so a cache hit cannot carry another tenant's root.
- `canonicalize-once-before-every-matching-layer` — `Principal.Tenant` is `event.TenantID`, and
  `event.NormalizeTenant("")` collapses the empty tenant to `DefaultTenant "_default"`. The
  resolver must decide this explicitly: an unauthenticated/empty tenant must not silently share
  a root with a real one. Open question for build time, flagged below.
- `race-invisible-to-race-detector-without-concurrent-test` — two-tenant isolation needs a
  concurrent test, not just sequential ones, for `-race` to see anything.

**Precedent to follow:** #64's egress datapath is the shape for this. A concrete process-scoped
resource arrives via a `ServiceOption` from the composition root, is forwarded into the handler
as an unexported `containerOption` holding a narrow interface, and the per-principal value is
resolved at `Acquire` from the `Principal` parameter and stored immutably on the Runner
(`egressSocketSource.Listen(p, ...)`, `container.go:342-387`). The tenant root resolver should
mirror it rather than invent a second shape.

**Test-lane note:** the container E2E and integration tests gate at runtime on
`exec.LookPath{docker,nerdctl,podman}` and `t.Skipf` — no build tag — so they run under plain
`make test` when a runtime is present and skip green otherwise.

Scope otherwise stands. No kill, no fundamental invalidation.

<!-- Appended by docket-implement-next's reconcile pass: dated entries of what changed. -->
