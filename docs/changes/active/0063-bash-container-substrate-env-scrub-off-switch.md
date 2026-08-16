---
id: 63
slug: bash-container-substrate-env-scrub-off-switch
title: bash container substrate + env-scrub + off-switch — the sandbox container behind a pluggable OCI runtime seam
status: proposed
priority: high
type: feat
created: 2026-08-16
updated: 2026-08-16
depends_on: [58]
related: [64, 65]
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

ADR-0044 established that `bash` is a **containment** problem, not a **credentialing** one: `internal/tools/bash.go` hands the model a real shell with `cmd.Env` never set, so the child inherits fuse's entire process environment — every ambient credential fuse holds — and can present it to any endpoint the host can reach. ADR-0036's delegation mechanism structurally cannot cover a shell (nothing to bind a token to, nowhere to present it), so the boundary has to be a container.

This is the **load-bearing first slice** of ADR-0044's deferred implementation: the container substrate itself, the fail-safe off-switch, and structural ambient-credential scrubbing. Egress control (#64) and per-tenant filesystem isolation (#65) both build on the container this change introduces, which is why it lands first and carries `high` priority. The design decisions are recorded in ADR-0044 — this stub tracks turning that deferral into build-ready work and is not a re-litigation of the decision.

## What changes

- **A pluggable runtime seam** — the same shape as ADR-0036's `TokenExchanger`/`CredentialSource` and ADR-0034's `Verifier`: a thin interface that selects an **OCI runtime handler** (not a bespoke API per runtime). `runc` is the zero-config default; gVisor (`runsc`) and Kata are drop-in OCI runtimes for the hardened multi-tenant tier. The bash child runs inside an OCI/Docker-shaped container so every subprocess it spawns (`curl`, `psql`, `git`) is inside the same namespace and the boundary holds regardless of what the model runs. One substrate serves both local and hosted — one code path, not two postures.
- **A host / no-container binding that IS the seam's local off-switch** — a host binding is itself an implementation of the seam, so the off-switch falls out of the design rather than being bolted on. It is **fail-safe, never fail-open**: contained by default; absent or unreadable config ⇒ contained; disabling is **opt-out from trusted local config only** — never from model output, never from a wire field; and it is **structurally inert when the ADR-0034 hosted/loop-server posture is active** (a deployed context has no path to run `bash` uncontained).
- **Structural ambient-credential scrubbing** — the child starts from an **empty environment** and receives exactly an explicit allowlist of benign vars (`PATH`, `HOME`, `LANG`, plus operator-declared safe passthroughs). On the **host off-switch path**, `cmd.Env` is **SET to that same allowlist** rather than left unset — "inherit everything" is never the behavior in either mode. This closes the ambient-credential inheritance hole **by construction**, honoring ADR-0036's constraint (no ambient-credential passthrough) applied at the subprocess boundary.

## Out of scope

- **Egress / network policy** — `--network none` floor and the operator-declared allowlist are Change #64.
- **Per-tenant filesystem isolation** — the ADR-0034 `Principal.Tenant` bind-mount and `working_dir` containment are Change #65.
- Any attempt to extend ADR-0036's delegation *mechanism* to the shell (ADR-0044 rules this out).
- A separate sandboxed code-exec tool (Deno was considered and rejected as the substrate for *this* tool in ADR-0044).
- Warm/pooled-container performance mitigation is a likely follow-on, not required for this first slice.

## Open questions

<!-- Groomed into a build-ready spec later; the design decisions themselves are recorded in ADR-0044. -->
- Container-image / rootfs shape for the default `runc` handler, and how the working tree is mounted in so the model sees the repo it edits.
- Where the trusted local off-switch config lives and how it is read fail-safe (absent/unreadable ⇒ contained).
- Exact operator-declared safe-passthrough env var mechanism and its config surface.
- Interaction with ADR-0034 ownership/lease lifecycle for a per-loop warm/pooled container (deferred, but shapes the interface).
- Docker-in-Docker / mounted-socket privilege-escalation tradeoff (socket ≈ host root) that any container-capable implementation must address.

## Reconcile log

<!-- Appended by docket-implement-next's reconcile pass: dated entries of what changed. -->
