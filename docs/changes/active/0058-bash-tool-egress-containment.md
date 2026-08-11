---
id: 58
slug: bash-tool-egress-containment
title: bash tool egress containment — define the authz posture for a tool that can reach anything
status: proposed
priority: medium
type: feat
created: 2026-08-11
updated: 2026-08-11
depends_on: [52]
related: [49, 55, 57]
discovered_from: [52]
adrs: []
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

## Why

The `bash` built-in (`internal/tools/bash.go`) hands the model a shell. It can `curl` any reachable endpoint, touch any file the process can, and present whatever ambient credentials live in its environment — with **no declared downstream target and no per-principal identity**. Today its only control is the approval gate (mode: off/ask/deny, disable) — invoke-time authorization, not downstream authn.

`bash` **cannot** use the #52 identity-propagation model. Delegated, audience-bound token exchange requires fuse to know the target ahead of time (declared audience + scope) and to control the individual outbound call. `bash` inverts both: fuse mediates the *shell invocation*, not the arbitrary network/file calls inside it. So the RFC 8693 seam #52 built — and change #57 extends to `web_fetch`/`web_search` — structurally does not apply here. `bash` is a **containment** problem (bound what the shell can reach), not a **credentialing** problem (present the right identity for a known target).

For a single-tenant local dev tool this is acceptable — `bash` is expected to be powerful. For a **deployed multi-tenant service** it is the widest hole in the identity/isolation story: a loop's shell can exfiltrate ambient credentials or reach another tenant's resources with no per-principal boundary. This change defines the posture rather than leaving it implicit.

## What changes (proposal altitude — design in brainstorm)

Define and implement the **egress/execution containment posture for `bash`** in a multi-tenant deployment. This is a design-first change: the brainstorm settles the model before any code.

Likely scope to settle in design:

- **Containment vs. delegation (the framing).** Confirm bash is contained, not credentialed: default-deny network egress, an allowlist/proxy boundary, credential scrubbing from the child environment, and per-tenant filesystem/workdir isolation — rather than trying to mint a per-call token for an unknown target.
- **Egress control mechanism.** How outbound network from the bash child is bounded (no ambient egress; an explicit egress proxy that itself routes through the #52 seam for declared targets; or default-off with opt-in per-target allow). Decide what "bash reaches the internet" even means in a hosted deployment.
- **Ambient-credential scrubbing.** The child process environment must not inherit fuse's process/downstream credentials (the #52 root-of-trust and no-passthrough constraints, applied to a subprocess boundary).
- **Per-tenant isolation.** Filesystem/workdir and any writable state scoped per tenant/principal, consistent with #49's ownership model and #52's per-tenant isolation.
- **Deployment-profile gating.** Whether `bash` is simply **disabled by default** in the multi-tenant profile (the safe floor) and only enabled under an explicit contained configuration — vs. always-contained. The single-tenant local profile keeps today's powerful behavior.

## Out of scope

- **Built-in HTTP tool identity (`web_fetch`/`web_search`)** — those *can* use the #52 delegation seam; tracked as change #57. This change is specifically the tool that cannot.
- **The #52 egress seam itself** — settled; this change layers a containment boundary, it does not redesign token exchange.
- **General OS sandboxing of the whole fuse process** — this is about the `bash` child's egress/isolation, not host hardening.

## Note

Filed 2026-08-11 while reviewing #52 (PR #55) before merge. #52 propagates identity for tools with declared targets (MCP now, HTTP built-ins under #57); `bash` is the one egressing tool the delegation model cannot cover, so its posture is containment, not credentialing. Split from #57 deliberately because it is a different mechanism and a different (likely larger) design. Needs a brainstorm to settle the containment model and deployment-profile gating before it is build-ready.

## Reconcile log

<!-- Appended by docket-implement-next's reconcile pass: dated entries of what changed. -->
