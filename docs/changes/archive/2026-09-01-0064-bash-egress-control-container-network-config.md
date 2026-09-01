---
id: 64
slug: bash-egress-control-container-network-config
title: bash egress control — egress as the container's network configuration, not an in-process dialer allowlist
status: done
priority: medium
type: feat
created: 2026-08-16
updated: 2026-09-01
depends_on: [63]
related: [63, 65, 75, 77]
discovered_from: [58]
adrs: [44, 51, 52, 53]
spec: docs/superpowers/specs/2026-08-21-bash-egress-control-container-network-config-design.md
plan: docs/superpowers/plans/2026-09-01-bash-egress-control-container-network-config-plan.md
results: docs/results/2026-09-01-bash-egress-control-container-network-config-results.md
trivial: false
auto_groomable:
branch: feat/bash-egress-control-container-network-config
pr: https://github.com/ethanhinson/fuse/pull/84
blocked_by:
claimed_at: 
reconciled: true
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-08-21-bash-egress-control-container-network-config-design.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/2026-08-21-bash-egress-control-container-network-config-design.md) |
| Plan | [2026-09-01-bash-egress-control-container-network-config-plan.md](https://github.com/ethanhinson/fuse/blob/main/docs/superpowers/plans/2026-09-01-bash-egress-control-container-network-config-plan.md) |
| Results | [2026-09-01-bash-egress-control-container-network-config-results.md](https://github.com/ethanhinson/fuse/blob/main/docs/results/2026-09-01-bash-egress-control-container-network-config-results.md) |
| PR | [#84](https://github.com/ethanhinson/fuse/pull/84) |
| ADRs | [ADR-0044](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0044-bash-tool-contained-not-credentialed.md), [ADR-0051](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0051-network-none-reaches-its-proxy-by-mounted-socket-plus-supplied-forwarder.md), [ADR-0052](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0052-delegated-identity-on-bash-egress-is-forward-proxy-only.md), [ADR-0053](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0053-whole-file-config-discard-salvages-the-posture.md) |
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

### 2026-09-01 — reconciled at claim; scope unchanged, three constraints folded in

Re-read against `origin/main` (`7753451`), the linked spec, ADR-0044, and changes
63/65/75/77. **The design holds** — no scope was dropped or added, and both escape
hatches (obsolete, fundamentally invalidated) were considered and declined.

What changed since the spec was authored (2026-08-21), all folded into the spec:

- **#0077 landed** (resource limits + admission gate, merged as `fa13ee0`). It moved
  the reserved `TODO(#0064)` insertion point — the spec's `container.go:414` is
  stale; the marker now sits directly after `r.handler.limits.argv()`. #0077's own
  comment names that placement as deliberate so egress lands beside it, so this is a
  friendly move, not a collision. `argv` also gained a trailing `--pull=never` backed
  by a separately-timed `prePull`, which the `--network none` floor must not break.
- **`Config` gained a posture split** — `resolveDefaults(hosted bool)` now defaults
  caps ON when hosted and OFF locally. `egress.mode` deliberately does NOT join that
  split (the spec's scope decision is an explicit knob, never hosted-derived); the
  spec now says so at the point of confusion, because the two would otherwise sit in
  the same function.
- **Two new ADRs bind the allowlist matcher.** ADR-0049 confirms the allowlist (not
  denylist) shape on a no-human deterministic-allow path. ADR-0048 rule 3 is the
  sharper one: canonicalize the host ONCE before every layer, with a recorded live
  bug where a trailing dot converted a configured deny into an auto-approve. The
  egress matcher is the same shape and must carry the same canonicalization plus
  regression coverage.

Dependencies re-verified: **#63 is `done`** (archived 2026-08-21) and its seam is on
`main` as specced. **#52's identity seam is real and unchanged** —
`toolidentity.CredentialSource.CredentialFor(ctx, Principal, Target)`, with `Target`
already carrying the audience binding the per-entry `credential:` opt-in needs.
`pool.go`'s existing `certifyPrincipal` / `principalScoped` guard is the per-principal
reuse mechanism the proxy's scoping invariant composes with. Out-of-scope boundaries
are unmoved: **#65** (per-tenant FS isolation) and **#75** (PaaS substrate) are both
still `proposed`/needs-brainstorm, so nothing this change defers to them has been
built elsewhere.

Auto-capture is disabled in this repo (`auto_capture.enabled: false`), so adjacent
work is reported in prose rather than minted: none surfaced beyond the already-tracked
#65/#75 deferrals.
