---
id: 63
slug: bash-container-substrate-env-scrub-off-switch
title: bash container substrate + env-scrub + off-switch — the sandbox container behind a pluggable OCI runtime seam
status: proposed
priority: high
type: feat
created: 2026-08-16
updated: 2026-08-20
depends_on: [58]
related: [64, 65]
discovered_from: [58]
adrs: [44]
spec: docs/superpowers/specs/2026-08-20-bash-container-substrate-env-scrub-off-switch-design.md
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
| Spec | [2026-08-20-bash-container-substrate-env-scrub-off-switch-design.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/2026-08-20-bash-container-substrate-env-scrub-off-switch-design.md) |
| ADRs | [ADR-0044](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0044-bash-tool-contained-not-credentialed.md) |
<!-- docket:artifacts:end -->

## Why

ADR-0044 established that `bash` is a **containment** problem, not a **credentialing** one: `internal/tools/bash.go` hands the model a real shell with `cmd.Env` never set, so the child inherits fuse's entire process environment — every ambient credential fuse holds — and can present it to any endpoint the host can reach. ADR-0036's delegation mechanism structurally cannot cover a shell (nothing to bind a token to, nowhere to present it), so the boundary has to be a container.

This is the **load-bearing first slice** of ADR-0044's deferred implementation: the container substrate itself, the fail-safe off-switch, and structural ambient-credential scrubbing. Egress control (#64) and per-tenant filesystem isolation (#65) both build on the container this change introduces, which is why it lands first and carries `high` priority. The design decisions are recorded in ADR-0044 — this stub tracks turning that deferral into build-ready work and is not a re-litigation of the decision.

## What changes

- **A pluggable runtime seam** — the same shape as ADR-0036's `TokenExchanger`/`CredentialSource` and ADR-0034's `Verifier`: a thin interface that selects an **OCI runtime handler** (not a bespoke API per runtime). `runc` is the zero-config default; gVisor (`runsc`) and Kata are drop-in OCI runtimes for the hardened multi-tenant tier. The bash child runs inside an OCI/Docker-shaped container so every subprocess it spawns (`curl`, `psql`, `git`) is inside the same namespace and the boundary holds regardless of what the model runs. One substrate serves both local and hosted — one code path, not two postures.
- **The seam is a typed isolation-HANDLER interface — isolation-mechanism-agnostic, not OCI-only** (per ADR-0044's 2026-08-16 Update). It must NOT be an `OCIRuntime`-only type, and equally must NOT be a broad all-encompassing `IsolationBoundary`: the shape is a handler seam where a **microVM handler (Firecracker / Cloud Hypervisor / Kata-as-VM) drops in behind the SAME seam later without re-widening it**. runc stays the zero-config default; the microVM handler is an in-seam handler, not a parallel mechanism. (PaaS/remote is explicitly out-of-scope here — it gets its own future ADR.)
- **kvm-absent ⇒ fail-CLOSED** (per ADR-0044's 2026-08-16 Update). This stub builds the container tier + off-switch, but the handler seam it lands must not foreclose this rule: if a microVM handler is selected on a host lacking `/dev/kvm`, it MUST refuse to run — it MUST NOT degrade to the host / no-container off-switch, which would be fail-open. The off-switch is a trusted-local opt-out, never an implicit fallback for a missing isolation capability.
- **Warm/snapshot pools MUST be strictly per-principal and reset — no cross-principal reuse** (per ADR-0044's 2026-08-16 Update). A resumed snapshot is not "empty by construction," so the env-scrub invariant must be re-established on every reused sandbox: pooled or snapshotted containers are scoped to a single principal and reset before reuse, never handed across principals.
- **A host / no-container binding that IS the seam's local off-switch** — a host binding is itself an implementation of the seam, so the off-switch falls out of the design rather than being bolted on. It is **fail-safe, never fail-open**: contained by default; absent or unreadable config ⇒ contained; disabling is **opt-out from trusted local config only** — never from model output, never from a wire field; and it is **structurally inert when the ADR-0034 hosted/loop-server posture is active** (a deployed context has no path to run `bash` uncontained).
- **Structural ambient-credential scrubbing** — the child starts from an **empty environment** and receives exactly an explicit allowlist of benign vars (`PATH`, `HOME`, `LANG`, plus operator-declared safe passthroughs). On the **host off-switch path**, `cmd.Env` is **SET to that same allowlist** rather than left unset — "inherit everything" is never the behavior in either mode. This closes the ambient-credential inheritance hole **by construction**, honoring ADR-0036's constraint (no ambient-credential passthrough) applied at the subprocess boundary.

**Grooming decisions (2026-08-20; detail in the linked spec):**

- **Default substrate is the container tier, driven by an auto-detected container CLI** (docker → nerdctl → podman), not raw `runc` and not microVM-first — microVM needs `/dev/kvm` (absent on macOS/many CI runners), where ADR-0044 mandates fail-closed, which would make local `bash` refuse to run. The container CLI keeps local dogfooding working on the dev Mac.
- **The microVM handler is validated in-spec but NOT built here** — an interface-conformance sketch proves `Handler`/`Runner`/`Env` accommodate a hardware-VM mechanism without re-widening the seam, so the later microVM change is a drop-in.
- **A basic per-loop warm pool IS in scope** — keyed strictly by principal, reset-and-re-scrubbed on every checkout, torn down on every loop early-return plus an idle-TTL reaper. The principal comes from the authenticated loop context via `toolidentity.PrincipalFrom(ctx)` (the existing identity seam), never from model output.
- **The off-switch config is a dedicated, gitignored, file-only local config** (`.fuse/sandbox.local.yml`) — no env-var opt-out; absent/unreadable/malformed ⇒ contained.
- **Full sandbox observability is in scope** — an operator must be able to see unhealthy containers and map every running loop to the container/host it runs on. This rides fuse's existing change-0051 event→projection seam rather than a bespoke metrics path: four new bounded event kinds (`sandbox.acquire`/`release`/`reap`/`health`, carrying handler + runtime + container-id, always under the `(tenant, loop, node)` envelope) are projected in `internal/observe` to Prometheus/OTEL/Loki, with `fuse_sandbox_active` (what's running where), `fuse_sandbox_unhealthy_total{reason}` (unhealthy containers: OOM/runtime-exit/pull-fail), `fuse_sandbox_cold_start_seconds`, and `fuse_sandbox_reaped_total{cause}` (leak signal), plus Grafana panels and alert rules under `deploy/observability`. The sandbox package emits events only — it never opens its own OTEL spans or registers its own meters (same choice `permission.decision` made in change 0067).

## Out of scope

- **Egress / network policy** — `--network none` floor and the operator-declared allowlist are Change #64.
- **Per-tenant filesystem isolation** — the ADR-0034 `Principal.Tenant` bind-mount and `working_dir` containment are Change #65.
- Any attempt to extend ADR-0036's delegation *mechanism* to the shell (ADR-0044 rules this out).
- A separate sandboxed code-exec tool (Deno was considered and rejected as the substrate for *this* tool in ADR-0044).
- A full lease-manager rewrite — the warm pool (now in scope, see grooming decisions) *hooks into* the ADR-0034 lease lifecycle for release-on-loop-end; it does not re-implement the lease mechanism.
- **Actually implementing a microVM handler** is a later change — this slice ships the container tier behind the mechanism-agnostic seam so the microVM handler can drop in later without re-widening it.
- **PaaS / remote-backend isolation** is out-of-scope and gets its own future ADR (ADR-0044's 2026-08-16 Update); the seam here must not foreclose it, but no PaaS work lands in this change.

## Open questions

All five open questions were resolved during grooming (2026-08-20) into the linked spec:

- **Container-image / rootfs & workdir mount** → configurable `image` with a pinned minimal default; working tree bind-mounted read-write at a fixed in-container path (`/workspace`). Resolved.
- **Off-switch config location & fail-safe read** → dedicated, gitignored, file-only `.fuse/sandbox.local.yml`; absent/unreadable/malformed ⇒ contained. Resolved.
- **Operator-declared safe-passthrough env mechanism** → an `env_passthrough` list in that config, resolved to host values and merged into the allowlist alongside `PATH`/`HOME`/`LANG`. Resolved.
- **ADR-0034 lease lifecycle interaction** → the per-loop warm pool releases its per-principal Runner on loop-end/reclaim via a release hook; the lease mechanism itself is untouched. Resolved.
- **Docker-in-Docker / mounted-socket privilege escalation** → this change never mounts the docker socket into the container; socket access (≈ host root) is an explicit non-goal. Resolved.

## Reconcile log

<!-- Appended by docket-implement-next's reconcile pass: dated entries of what changed. -->
