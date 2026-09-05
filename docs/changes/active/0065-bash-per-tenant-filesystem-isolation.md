---
id: 65
slug: bash-per-tenant-filesystem-isolation
title: bash per-tenant filesystem isolation — a Principal.Tenant-scoped bind-mount the working_dir cannot escape
status: in-progress
priority: medium
type: feat
created: 2026-08-16
updated: 2026-09-05
depends_on: [63]
related: [63, 64, 74, 75, 77]
discovered_from: [58]
adrs: [44]
spec: docs/superpowers/specs/2026-09-04-bash-per-tenant-filesystem-isolation-design.md
plan:
results:
trivial: false
auto_groomable:
branch: feat/bash-per-tenant-filesystem-isolation
claimed_at: 2026-09-05T03:56:21Z
pr:
blocked_by:
reconciled: false
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-09-04-bash-per-tenant-filesystem-isolation-design.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/2026-09-04-bash-per-tenant-filesystem-isolation-design.md) |
| ADRs | [ADR-0044](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0044-bash-tool-contained-not-credentialed.md) |
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
- **Land #74's health emitter here** (deferred into this change 2026-09-04). A persistent
  per-tenant container is what makes `ContainerID` real and a health probe possible, so
  populate `ContainerID`, emit the reasons the substrate can honestly observe, build the
  exit-code classifier for `oom`/`runtime_exit`, and flip #63's E2E tripwire that currently
  asserts `fuse_sandbox_unhealthy_total` stays unfed. Never fabricate a health signal.

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

<!-- Appended by docket-implement-next's reconcile pass: dated entries of what changed. -->
