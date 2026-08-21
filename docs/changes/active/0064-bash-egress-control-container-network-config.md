---
id: 64
slug: bash-egress-control-container-network-config
title: bash egress control — egress as the container's network configuration, not an in-process dialer allowlist
status: proposed
priority: medium
type: feat
created: 2026-08-16
updated: 2026-08-21
depends_on: [63]
related: [63, 65, 75, 77]
discovered_from: [58]
adrs: [44]
spec: docs/superpowers/specs/2026-08-21-bash-egress-control-container-network-config-design.md
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
| Spec | [2026-08-21-bash-egress-control-container-network-config-design.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/2026-08-21-bash-egress-control-container-network-config-design.md) |
| ADRs | [ADR-0044](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0044-bash-tool-contained-not-credentialed.md) |
<!-- docket:artifacts:end -->

## Why

ADR-0044 decided that for `bash`, **egress is the container's network configuration**, not an in-process dialer allowlist — because a container holds a *real shell* and every subprocess it spawns runs inside the same namespace, so the network boundary must live at the container, not in fuse's own dialer. This is the second of ADR-0044's three deferred follow-ons (container substrate first, then egress control, then per-tenant filesystem isolation). It depends on the container substrate and its runtime seam from Change #63. The design decisions are recorded in ADR-0044 — this stub tracks turning that deferral into build-ready work.

## What changes

Container handler only — the microVM boundary is recorded as a contract a future handler must meet, not built here (the microVM handler is still an unwired stub). Full enforcement stack for the one boundary. See the linked spec for design detail.

- **The floor — `--network none`.** When egress enforcement is on, the container argv gains `--network none` at the reserved insertion point (`container.go:414`): the workload container has no route to the internet by construction.
- **The path out — a shared host-side egress proxy.** One fuse-managed proxy per host, reachable by the `--network none` container through a single controlled hole; it is the only egress path, enforces the allowlist per call, and denies every undeclared destination. Its per-connection **principal-scoping is a first-class invariant** (a shared proxy must bind the requesting principal's allowlist and credential, never leak across principals); a per-container sidecar is the recorded build-time fallback if that proves too hairy.
- **Operator-declared allowlist**, selected by an explicit `egress.mode` knob (`allow-all` default / `enforce`). Each entry is `{host-or-CIDR, port}`; sourced from trusted-local config. Allow-all locally, enforce in the hosted profile. Malformed-under-enforce fails toward deny-all, never fail-open.
- **Delegated identity for declared targets** — per-entry opt-in: an allowlist entry MAY name a #52 `CredentialSource` audience, and the proxy injects the delegated credential resolved via `CredentialFor(principal, target)` for that entry; entries without one are plain allow-through. Everything undeclared is denied. This is the one place `bash` can borrow ADR-0036's mechanism — a *declared* target gives it the choke point and audience binding.
- **Local ↔ off-switch:** egress is a property of the container boundary; on the host off-switch path there is no container, so egress is unconstrained by design — consistent with #63's machine-trust opt-out.
- **Record the metadata-endpoint null-route** (`169.254.169.254`) as the concrete acceptance criterion any future remote/PaaS backend (#75) must meet before it can host `bash`. No PaaS work lands here — this only records the criterion so it isn't lost.

## Out of scope

- The container substrate, runtime seam, off-switch, and env-scrub — all Change #63 (a hard dependency).
- Per-tenant filesystem isolation — Change #65.
- Building a general in-process dialer allowlist (ADR-0044 explicitly rejects this framing for `bash`).
- Extending declared-target routing to arbitrary/undeclared destinations — those are denied, not delegated.
- **PaaS / remote-backend egress** — deferred to the future PaaS ADR (ADR-0044's 2026-08-16 Update). This change only *records* the metadata-endpoint null-route acceptance criterion that ADR will have to meet; it builds no remote backend.

## Open questions

<!-- Design settled in the linked spec; these residuals are owned by the reconcile/plan pass. -->
- The container→proxy hole under `--network none` — bind-mounted proxy socket vs. userspace-net path — settled at plan time against the supported OCI CLIs, without re-opening general egress.
- Proxy protocol surface (HTTP CONNECT first vs. raw TCP for `psql`).
- How the proxy lifecycle and principal-scoping compose with #63's per-principal warm pool.

## Reconcile log

<!-- Appended by docket-implement-next's reconcile pass: dated entries of what changed. -->
