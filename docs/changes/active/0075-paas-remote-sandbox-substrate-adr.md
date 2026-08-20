---
id: 75
slug: paas-remote-sandbox-substrate-adr
title: PaaS/remote sandbox substrate — the provision/attach/teardown seam ADR, with a k8s Pod-per-Exec handler as first implementation
status: proposed
priority: medium
type: feat
created: 2026-08-20
updated: 2026-08-20
depends_on: [63]
related: [63, 64, 65, 77]
discovered_from: [63]
adrs: [44, 34, 36]
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

ADR-0044 shipped the bash sandbox as a **local, host-spawned** substrate: `docker run --rm` per `Exec` behind a pluggable isolation **handler** seam (`internal/tools/sandbox` `Handler.Acquire(ctx, principal, env) → Runner{Exec, Release}`). That covers a container-capable host running the runtime itself. It does **not** cover running the sandbox on an orchestrator fuse does not own — Kubernetes (a Job/Pod per Exec), Fly Machines, Modal, E2B, Fargate, Depot, Daytona.

ADR-0044's 2026-08-16 Update is explicit that this class is **NOT another handler on the existing seam** and is **out of scope until its own ADR is written**. The reasons are recorded and are real:

- When fuse does not own the isolation primitive, `"contained"` stops being verifiable-by-construction — it becomes a remote control-plane call fuse cannot confirm. That forks ADR-0044's "one substrate, one code path, fail-safe-by-observation" rationale (network client, retry/idempotency, sandbox-lifecycle GC, provider egress model).
- On any substrate exposing a metadata endpoint (`169.254.169.254`) that cannot be null-routed, the `--network none` egress floor **breaks**: the substrate can inject ambient credentials the in-container env-scrub cannot reach.

So the boundary is a **remote provision/attach/teardown seam**, distinct from the in-process isolation-handler seam. This change writes the ADR ADR-0044 mandates, then lands the first implementation (k8s Pod-per-Exec) behind it.

This is the orchestration answer for the **sandbox layer** — running bash workloads as Pods is a substrate handler, NOT a Helm chart and NOT an operator. The **server layer** (deploying fuse itself on k8s) is Change #76 and is entirely separate.

## What changes

- **A new ADR (next number, ~0051)** defining the remote provision/attach/teardown seam for substrates fuse does not own. It relates-to ADR-0044 and MUST clear ADR-0044's five acceptance gates as explicit criteria:
  1. **Real-shell containment** — every subprocess the command spawns stays inside the boundary (the Deno-killer test).
  2. **Fail-safe-under-remote-failure** — a remote provision/attach that fuse cannot confirm resolves to *refuse*, never to run-anyway or degrade to the host off-switch (which would be fail-open).
  3. **`--network none` / metadata-endpoint floor** — the egress floor holds; a substrate with a non-null-routable `169.254.169.254` metadata endpoint that injects ambient credentials is disqualified or explicitly mitigated.
  4. **Tenant-scoped, non-escaping filesystem** — `Principal.Tenant`-scoped, `working_dir` cannot escape (consistent with #65).
  5. **No provisioning-credential passthrough** — the credential fuse uses to provision/attach the remote sandbox is never observable by the sandboxed child.
- **A `Handler`-adjacent remote seam** (provision → attach → exec → teardown, with idempotency + GC of orphaned remote sandboxes), NOT a widened "OCI-or-remote" umbrella on the existing in-process handler interface.
- **First implementation: a Kubernetes handler** — a Job/Pod-per-`Exec` (or a warm per-principal Pod once #65's persistent substrate exists), with Pod `resources.limits` and a per-tenant `ResourceQuota`/admission ceiling mapping onto #77's limit model.

## Out of scope

- Deploying the fuse **server** on k8s (Helm chart, HPA, Postgres dependency) — Change #76. That is the server layer; this is the sandbox layer.
- The in-seam microVM handler (Firecracker/Cloud Hypervisor/Kata) — ADR-0044 already places that on the *existing* handler seam, not this remote one; it is its own follow-on.
- Egress policy mechanism (#64) and per-tenant FS mechanism (#65) — this ADR references them as acceptance gates but does not re-decide them.
- Choosing a single provider — the ADR defines the seam and the gates; provider-specific handlers beyond the k8s first implementation are follow-ons.

## Open questions

<!-- Groomed into a build-ready spec later; the seam-vs-handler decision itself belongs in the new ADR. -->
- Pod-per-`Exec` vs. warm per-principal Pod: the former is the honest analogue of today's `run --rm` (stateless, self-cleaning) but pays scheduling latency per command; the latter needs #65's persistent-substrate lifecycle + strict per-principal reset (the same no-cross-principal-reuse rule ADR-0044 pins for microVM warm pools).
- How the remote seam expresses fail-safe-by-observation: what "confirm-or-refuse" looks like as a concrete control-plane check, and what timeout/retry/idempotency contract prevents a partial provision from leaking an orphaned Pod.
- Metadata-endpoint floor on k8s specifically (IMDS, cloud-provider metadata, projected service-account tokens): can it be null-routed / disabled per-Pod, or is it a disqualifier on certain clusters?
- Whether GC of orphaned remote sandboxes belongs in the handler or in a separate reaper analogous to the pool's idle reaper.
- Relationship to the resource-limit model (#77): does the k8s handler *enforce* limits via Pod spec while the local handler enforces via cgroup flags, behind one config surface?

## Reconcile log

<!-- Appended by docket-implement-next's reconcile pass: dated entries of what changed. -->
