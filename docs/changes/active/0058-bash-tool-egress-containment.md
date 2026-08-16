---
id: 58
slug: bash-tool-egress-containment
title: bash tool egress containment — define the authz posture for a tool that can reach anything
status: in-progress
priority: medium
type: feat
created: 2026-08-11
updated: 2026-08-16
depends_on: [52]
related: [49, 55, 57]
discovered_from: [52]
adrs: []
spec: docs/superpowers/specs/2026-08-15-bash-tool-egress-containment-design.md
plan: docs/superpowers/plans/2026-08-16-bash-tool-egress-containment-plan.md
results:
trivial: false
auto_groomable:
branch: feat/bash-tool-egress-containment
claimed_at: 2026-08-16T19:46:42Z
pr:
blocked_by:
reconciled: true
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-08-15-bash-tool-egress-containment-design.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/2026-08-15-bash-tool-egress-containment-design.md) |
| Plan | [2026-08-16-bash-tool-egress-containment-plan.md](https://github.com/ethanhinson/fuse/blob/feat/bash-tool-egress-containment/docs/superpowers/plans/2026-08-16-bash-tool-egress-containment-plan.md) |
<!-- docket:artifacts:end -->

## Why

The `bash` built-in (`internal/tools/bash.go`) hands the model a shell. It can `curl` any reachable endpoint, touch any file the process can, and present whatever ambient credentials live in its environment — with **no declared downstream target and no per-principal identity**. Today its only control is the approval gate (mode: off/ask/deny, disable) — invoke-time authorization, not downstream authn. (The source confirms it: `exec.CommandContext(runCtx, "/bin/sh", "-c", …)` never sets `cmd.Env`, so the child inherits fuse's whole process environment.)

`bash` **cannot** use the #52 identity-propagation model (ADR-0036). Delegated, audience-bound token exchange requires fuse to know the target ahead of time (declared audience + scope) and to control the individual outbound call. `bash` inverts both: fuse mediates the *shell invocation*, not the arbitrary network/file calls inside it. So the RFC 8693 seam #52 built — and change #57 extends to `web_fetch`/`web_search` — structurally does not apply here. `bash` is a **containment** problem (bound what the shell can reach), not a **credentialing** problem (present the right identity for a known target).

For a single-tenant local dev tool this is acceptable — `bash` is expected to be powerful. For a **deployed multi-tenant service** it is the widest hole in the identity/isolation story: a loop's shell can exfiltrate ambient credentials or reach another tenant's resources with no per-principal boundary. This change **defines the posture** rather than leaving it implicit.

## What changes

This is a **design-only** change: it ships the linked design spec **and one new ADR** (recording "bash is contained, not credentialed" — a sibling to ADR-0036), and **no code**. Implementation is spun out into follow-on changes named in the spec. The settled posture:

- **Framing — containment, not credentialing.** Ratified and recorded as a new ADR: `bash` cannot use the #52 delegation seam (no declared target, no per-call choke point), so it is bounded, not credentialed. It reuses #52's constraints (root-of-trust from context not model output; no ambient-credential passthrough) even though it cannot use #52's mechanism.
- **One substrate everywhere — the container is the boundary.** The bash child runs inside a container (OCI/Docker-shaped) in every profile, local included. Because the container contains a real shell — every subprocess it spawns is inside the same box — the boundary holds no matter what the model runs, and the same mechanism serves local and hosted. This buys dogfooding confidence: a loose leash inside the box, bounded by the container, not the developer's host.
- **Pluggable container runtime.** The runtime backing the container is a seam, not a hardcode — runc as the zero-config default, gVisor/Kata (drop-in OCI runtimes) for the hardened multi-tenant tier, and a host/no-container binding that *is* the local off-switch. Same seam shape as ADR-0036's `TokenExchanger` and ADR-0034's `Verifier`. Keeps the posture from re-coupling to one host's capabilities.
- **Local off-switch — fail-safe, trusted-config only.** Containerizing every `bash` call has an ergonomic/latency cost, so a local-only off-switch runs `bash` directly on the host when not in a deployed context. The invariant: contained is the default (absent/unreadable config = contained), disabling is opt-out from trusted local config only (never model output), and the off-switch is structurally inert when the hosted/loop-server posture (ADR-0034) is active — so "forgot to configure it" fails toward *contained*.
- **Egress — the container's network.** Hosted defaults to no ambient egress (`--network none` floor) plus an operator-declared allowlist (host/CIDR/port) from trusted config; allowlisted declared targets may route through the #52 seam. Local defaults to allow-all.
- **Ambient-credential scrubbing.** Structural in-container (empty env + explicit allowlist); on the host off-switch path, `cmd.Env` is set to the same allowlist rather than left unset. The child never inherits fuse's ambient credentials in either mode.
- **Per-tenant filesystem isolation.** Hosted bind-mounts only the tenant's workdir/root (via ADR-0034 `Principal.Tenant`); `working_dir` resolves within the mount and cannot escape it. Local mounts the fuse working tree so the model can edit the repo.

The spec names three recommended follow-on changes to file (container substrate + env-scrub + off-switch → egress control → per-tenant FS isolation), and carries the container-runtime-tier decision (runc vs gVisor/Kata) plus the deploy-target coupling as ADR-worthy build-time questions.

## Out of scope

- **Built-in HTTP tool identity (`web_fetch`/`web_search`)** — those *can* use the #52 delegation seam; tracked as change #57. This change is specifically the tool that cannot.
- **The #52 egress seam itself** — settled; this change layers a containment boundary, it does not redesign token exchange.
- **General OS sandboxing of the whole fuse process** — this is about the `bash` child's egress/isolation, not host hardening.
- **Implementing any of the containment mechanisms** — design-only; each mechanism spins out as its own change.

## Note

Filed 2026-08-11 while reviewing #52 (PR #55) before merge. #52 propagates identity for tools with declared targets (MCP now, HTTP built-ins under #57); `bash` is the one egressing tool the delegation model cannot cover, so its posture is containment, not credentialing. Split from #57 deliberately because it is a different mechanism and a different (likely larger) design. Groomed 2026-08-15 to a design-only spec + ADR posture.

## Reconcile log

<!-- Appended by docket-implement-next's reconcile pass: dated entries of what changed. -->

### 2026-08-16 — reconcile at claim

Verified every premise the spec rests on against `origin/main` @ `4333c41`; **nothing invalidated, no
scope adjustment**. The change stays design-only.

- **The hole is still open, verbatim.** `internal/tools/bash.go` is byte-unchanged from the spec's
  §1 quote: `exec.CommandContext(runCtx, "/bin/sh", "-c", a.Command)` with `cmd.Env` never set, so
  the child still inherits fuse's whole process environment. No containment, env-scrub, or egress
  control has landed anywhere in the tree in the interim.
- **Dependency satisfied.** #52 is archived `done` (ADR-0036 accepted), as are #49 (ADR-0034) and
  #55. The §2 framing argument — that `bash` cannot use #52's declared-target/per-call-choke-point
  seam — is therefore evaluated against the merged seam, not a proposal.
- **`related` drift, non-blocking.** #57 (`egress-identity-builtin-http-tools`) is now `deferred`
  rather than proposed. It is only ever cited by this change as an *out-of-scope* sibling ("those
  CAN use the #52 seam"), so its deferral changes nothing here: the boundary between the two
  changes is mechanism-based, not schedule-based.
- **ADR numbering.** The ledger head is ADR-0043, so this change's ADR mints at 0044 — a sibling to
  ADR-0036 as the spec intends.
- **Intervening work reviewed for interaction** (#0051 observability, #0054 durable sessions, #0060
  Wander demo): none touches the tool-execution boundary, so none constrains this posture.

Follow-on changes A/B/C from spec §4 remain unfiled and are left to the human (`auto_capture` is
disabled repo-wide, and the spec explicitly reserves the filing to a human).
