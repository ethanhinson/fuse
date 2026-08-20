---
id: 65
slug: bash-per-tenant-filesystem-isolation
title: bash per-tenant filesystem isolation — a Principal.Tenant-scoped bind-mount the working_dir cannot escape
status: proposed
priority: medium
type: feat
created: 2026-08-16
updated: 2026-08-16
depends_on: [63]
related: [63, 64, 74]
discovered_from: [58]
adrs: [44]
spec:
plan:
results:
trivial: false
auto_groomable:
branch:
pr:
blocked_by:
reconciled: false
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| ADRs | [ADR-0044](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0044-bash-tool-contained-not-credentialed.md) |
<!-- docket:artifacts:end -->

## Why

ADR-0044 decided that **hosted filesystem access is a per-tenant bind-mount** scoped by ADR-0034 `Principal.Tenant`, and that the model-supplied `working_dir` resolves *within* that mount and **cannot escape it**. This is the third of ADR-0044's three deferred follow-ons (container substrate first, then egress control, then per-tenant filesystem isolation). It depends on the container substrate from Change #63 — there is no mount to scope without the container. The design decisions are recorded in ADR-0044 — this stub tracks turning that deferral into build-ready work.

## What changes

- **Per-tenant bind-mount**: hosted filesystem access is a bind-mount into the container scoped by ADR-0034 `Principal.Tenant`, so one tenant's shell cannot see another tenant's files.
- **Extend the tenant-scoped, non-escaping mount to the microVM handler** (per ADR-0044's 2026-08-16 Update): the same `Principal.Tenant`-scoped isolation must hold when #63's seam selects a microVM handler, expressed as the VM-native equivalent of the container bind-mount — a per-tenant **virtio-fs share** OR a per-tenant **block image**. Same tenant-scoping rule, one backing per boundary mechanism.
- **`working_dir` containment**: the **model-supplied** `working_dir` resolves **within** the mount and cannot escape it (no `..`/symlink/absolute-path escape). This honors ADR-0044's inherited ADR-0036 constraint — the root of trust (the tenant/principal scoping the mount) comes from the **authenticated loop-start context, never from model output** (not the `command`, not `working_dir`).

## Out of scope

- The container substrate, runtime seam, off-switch, and env-scrub — Change #63 (a hard dependency).
- Egress / network policy — Change #64.
- Local (single-tenant) filesystem posture beyond what the #63 substrate already mounts in — the per-tenant scoping is a hosted-profile concern.
- Deriving the tenant/principal from anything the model supplies — explicitly forbidden.
- **PaaS provider-volume work** — deferred to the future PaaS ADR (ADR-0044's 2026-08-16 Update). This change scopes the tenant mount for the container and microVM backings only; provider-managed volumes for a remote/PaaS backend are out-of-scope here.

## Open questions

<!-- Groomed into a build-ready spec later; the design decisions themselves are recorded in ADR-0044. -->
- Host-side layout of per-tenant directories and how the bind-mount source is resolved from `Principal.Tenant`.
- Canonical mechanism for guaranteeing `working_dir` cannot escape the mount (resolve-and-verify vs. mount-namespace confinement) across the isolation handlers the #63 seam selects — including the microVM backing (virtio-fs share vs. per-tenant block image).
- Lifecycle/cleanup of per-tenant mounts relative to ADR-0034 ownership/lease.
- How the working tree the model edits is presented within the per-tenant mount.

## Reconcile log

<!-- Appended by docket-implement-next's reconcile pass: dated entries of what changed. -->
