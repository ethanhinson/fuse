---
id: 64
slug: bash-egress-control-container-network-config
title: bash egress control — egress as the container's network configuration, not an in-process dialer allowlist
status: proposed
priority: medium
type: feat
created: 2026-08-16
updated: 2026-08-16
depends_on: [63]
related: [63, 65, 75, 77]
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

ADR-0044 decided that for `bash`, **egress is the container's network configuration**, not an in-process dialer allowlist — because a container holds a *real shell* and every subprocess it spawns runs inside the same namespace, so the network boundary must live at the container, not in fuse's own dialer. This is the second of ADR-0044's three deferred follow-ons (container substrate first, then egress control, then per-tenant filesystem isolation). It depends on the container substrate and its runtime seam from Change #63. The design decisions are recorded in ADR-0044 — this stub tracks turning that deferral into build-ready work.

## What changes

- **Egress expressed as boundary network config — across BOTH the container AND the microVM handler** (per ADR-0044's 2026-08-16 Update): the egress floor is a property of the isolation boundary #63's seam selects, not of any one mechanism. For a container that floor is `--network none`; for the microVM handler it is the equivalent **no-NIC** posture, with any declared egress applied host-side (tap device + nftables for the VM). Same floor, two boundary expressions, one policy surface.
- **Operator-declared allowlist**: an operator-declared **allowlist** (host/CIDR/port) sourced from **trusted config in the hosted profile**; **allow-all locally**. "What does 'bash reaches the internet' mean in a hosted deploy?" — nothing, unless an operator declared that egress.
- **Delegated identity for declared targets**: an allowlisted **declared** target MAY route through the #52 egress seam, so a bash call reaching a known service still carries delegated identity where a target is declarable; **everything else is denied**. This is the one place `bash` can borrow ADR-0036's mechanism — a *declared* target gives it the choke point and audience binding the mechanism needs.
- **Record the metadata-endpoint null-route as the gating egress acceptance criterion for the future PaaS ADR** (per ADR-0044's 2026-08-16 Update): a null-route of the cloud metadata endpoint (`169.254.169.254`) is the concrete acceptance test any future remote/PaaS backend must meet before it can host `bash`. No PaaS work lands in this change — this bullet only records the acceptance criterion so it isn't lost when the eventual PaaS ADR is written.

## Out of scope

- The container substrate, runtime seam, off-switch, and env-scrub — all Change #63 (a hard dependency).
- Per-tenant filesystem isolation — Change #65.
- Building a general in-process dialer allowlist (ADR-0044 explicitly rejects this framing for `bash`).
- Extending declared-target routing to arbitrary/undeclared destinations — those are denied, not delegated.
- **PaaS / remote-backend egress** — deferred to the future PaaS ADR (ADR-0044's 2026-08-16 Update). This change only *records* the metadata-endpoint null-route acceptance criterion that ADR will have to meet; it builds no remote backend.

## Open questions

<!-- Groomed into a build-ready spec later; the design decisions themselves are recorded in ADR-0044. -->
- Config surface/schema for the operator-declared host/CIDR/port allowlist in the hosted profile.
- Mechanics of enforcing the egress floor + allowlist across the isolation handlers the #63 seam selects — `--network none` for the OCI runtimes (runc / gVisor / Kata) and the no-NIC / host-side tap+nftables equivalent for a microVM handler.
- Exactly how an allowlisted declared target is matched to a #52 egress-seam route so the delegated credential is presented.
- Whether/how local allow-all interacts with the host off-switch binding from #63.

## Reconcile log

<!-- Appended by docket-implement-next's reconcile pass: dated entries of what changed. -->
