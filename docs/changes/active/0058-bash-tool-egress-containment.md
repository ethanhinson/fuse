---
id: 58
slug: bash-tool-egress-containment
title: bash tool egress containment — define the authz posture for a tool that can reach anything
status: proposed
priority: medium
type: feat
created: 2026-08-11
updated: 2026-08-15
depends_on: [52]
related: [49, 55, 57]
discovered_from: [52]
adrs: []
spec: docs/superpowers/specs/2026-08-15-bash-tool-egress-containment-design.md
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
| Spec | [2026-08-15-bash-tool-egress-containment-design.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/2026-08-15-bash-tool-egress-containment-design.md) |
<!-- docket:artifacts:end -->

## Why

The `bash` built-in (`internal/tools/bash.go`) hands the model a shell. It can `curl` any reachable endpoint, touch any file the process can, and present whatever ambient credentials live in its environment — with **no declared downstream target and no per-principal identity**. Today its only control is the approval gate (mode: off/ask/deny, disable) — invoke-time authorization, not downstream authn. (The source confirms it: `exec.CommandContext(runCtx, "/bin/sh", "-c", …)` never sets `cmd.Env`, so the child inherits fuse's whole process environment.)

`bash` **cannot** use the #52 identity-propagation model (ADR-0036). Delegated, audience-bound token exchange requires fuse to know the target ahead of time (declared audience + scope) and to control the individual outbound call. `bash` inverts both: fuse mediates the *shell invocation*, not the arbitrary network/file calls inside it. So the RFC 8693 seam #52 built — and change #57 extends to `web_fetch`/`web_search` — structurally does not apply here. `bash` is a **containment** problem (bound what the shell can reach), not a **credentialing** problem (present the right identity for a known target).

For a single-tenant local dev tool this is acceptable — `bash` is expected to be powerful. For a **deployed multi-tenant service** it is the widest hole in the identity/isolation story: a loop's shell can exfiltrate ambient credentials or reach another tenant's resources with no per-principal boundary. This change **defines the posture** rather than leaving it implicit.

## What changes

This is a **design-only** change: it ships the linked design spec **and one new ADR** (recording "bash is contained, not credentialed" — a sibling to ADR-0036), and **no code**. Implementation is spun out into follow-on changes named in the spec. The settled posture:

- **Framing — containment, not credentialing.** Ratified and recorded as a new ADR: `bash` cannot use the #52 delegation seam (no declared target, no per-call choke point), so it is bounded, not credentialed. It reuses #52's constraints (root-of-trust from context not model output; no ambient-credential passthrough) even though it cannot use #52's mechanism.
- **Always-contained — one code path.** The bash child always runs inside the containment boundary, in every profile including local. No uncontained bypass anywhere — eliminating the "forgot the flag, shipped an uncontained shell" failure class.
- **Profile-parameterized policy.** The boundary is uniform; its policy varies. Local ships permissive defaults (egress allow-all, wide filesystem root) so dogfooding stays practical; hosted/multi-tenant ships locked-down defaults (deny + allowlist egress, per-tenant filesystem scope). **Env-scrub is on in every profile.**
- **Egress — default-deny + operator allowlist.** In hosted, the child has no ambient network egress; an operator-declared allowlist (host/CIDR/port) from trusted config — never model output — is the only exit, and allowlisted declared targets may route through the #52 seam.
- **Ambient-credential scrubbing.** `cmd.Env` is set to an explicitly-constructed scrubbed environment rather than left unset; the child never inherits fuse's process/downstream credentials.
- **Per-tenant filesystem isolation.** In hosted, the child's workdir/filesystem is scoped per tenant/principal (consuming ADR-0034 `Principal.Tenant`); `working_dir` resolves within that scope and cannot escape it.

The spec names three recommended follow-on changes to file (scaffold+env-scrub+profile-policy → egress control → per-tenant FS isolation), mirroring how #52 spun out #57/#58.

## Out of scope

- **Built-in HTTP tool identity (`web_fetch`/`web_search`)** — those *can* use the #52 delegation seam; tracked as change #57. This change is specifically the tool that cannot.
- **The #52 egress seam itself** — settled; this change layers a containment boundary, it does not redesign token exchange.
- **General OS sandboxing of the whole fuse process** — this is about the `bash` child's egress/isolation, not host hardening.
- **Implementing any of the containment mechanisms** — design-only; each mechanism spins out as its own change.

## Note

Filed 2026-08-11 while reviewing #52 (PR #55) before merge. #52 propagates identity for tools with declared targets (MCP now, HTTP built-ins under #57); `bash` is the one egressing tool the delegation model cannot cover, so its posture is containment, not credentialing. Split from #57 deliberately because it is a different mechanism and a different (likely larger) design. Groomed 2026-08-15 to a design-only spec + ADR posture.

## Reconcile log

<!-- Appended by docket-implement-next's reconcile pass: dated entries of what changed. -->
