---
id: 76
slug: fuse-server-helm-chart-compose-stack
title: fuse server deployment — a docker-compose stack and a Helm chart over the released image (not an operator)
status: proposed
priority: medium
type: feat
created: 2026-08-20
updated: 2026-09-13
depends_on: [82]
related: [75, 77, 82, 63]
discovered_from: [63]
adrs: [31, 34, 30, 33, 44]
spec: docs/superpowers/specs/2026-09-13-fuse-server-helm-chart-compose-stack-design.md
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
| Spec | [2026-09-13-fuse-server-helm-chart-compose-stack-design.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/2026-09-13-fuse-server-helm-chart-compose-stack-design.md) |
| ADRs | [ADR-0031](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0031-durable-distributed-event-store-loop-registry.md), [ADR-0034](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0034-edge-enforced-auth-multi-tenancy-loop-ownership.md), [ADR-0030](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0030-deglobalize-eventstore-multiloop-hosting.md), [ADR-0033](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0033-networked-binding-connect-protobuf-fuse-loop-v1.md), [ADR-0044](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0044-bash-tool-contained-not-credentialed.md) |
<!-- docket:artifacts:end -->

## Why

fuse can already run as a networked, multi-tenant, multi-loop **server** — `cmd/fuse loop-serve-net` over the ADR-0033 Connect transport, with edge-enforced auth (ADR-0034), a durable Postgres backend and loop registry (ADR-0031 `internal/event/pgstore`), and reconnect-after-redeploy via the durable ownership lease. #82 built its image (`ghcr.io/ethanhinson/fuse`, distroless, no shell, no container runtime). What it does **not** have is a way to *run* that image as a service: the only compose file is the observability sidecar stack (`deploy/observability/docker-compose.yml`), and there is no Helm chart.

Grooming surfaced three server-side gaps that make this more than packaging:

- **The released binary has no Postgres backend.** pgstore and its selector are behind a `pgstore` build tag that neither the release build nor the Makefile sets, so the published image runs the filesystem store — two replicas share nothing, and a redeploy loses every loop. The DSN plumbing exists and is unreachable.
- **No liveness or readiness endpoint** — a Kubernetes probe has nothing to hit.
- **SIGTERM is not handled** (only SIGINT), so a container stop is a hard kill with no cleanup or drain.

This is the **server layer** of the orchestration question, and it is deliberately separate from the sandbox layer (#75): running fuse itself on k8s is a Helm chart over a stateless-with-external-Postgres service, whereas running the bash *sandbox* on k8s is a substrate handler (#75). Conflating them is the trap this stub exists to avoid.

**Helm chart, not an operator.** An operator earns its keep when there is a custom resource with a non-trivial reconciliation lifecycle. The fuse server is stateless with durable state in Postgres (ADR-0030, ADR-0031), so N replicas behind a Service + HPA is the correct shape. Revisit an operator only if fuse later grows tenant-as-CRD or per-tenant dedicated infrastructure.

## What changes

Settled at groom on 2026-09-13; design detail in the linked spec.

- **Server prerequisites (code, small):** `-tags pgstore` on every release build and in the Makefile (one binary, one image; `fuse version` reports the backend set); unauthenticated status-code-only `/healthz` and `/readyz` (readiness pings the store via a new optional `event.Pinger`); SIGTERM handling with a bounded graceful drain (`--drain-timeout`, readiness flips first); `FUSE_INSTANCE_ID` as the env fallback for `observability.instance_id`.
- **A compose dev stack (`deploy/compose/`)** that `include`s the existing observability stack and adds `fuse` + Postgres, a dev config with the documented dev token and a loud placeholder header, loopback-published ports, and an opt-in `docker-socket` profile for the bash tool with ADR-0044's tradeoff quoted. Validated by the generalised observability validator and `docker compose config` in CI; an operator-only smoke target brings it up and runs one loop.
- **A Helm chart (`deploy/charts/fuse/`)**: Deployment (probes, nonroot + read-only root FS, config Secret mounted at the nonroot home with a rolling checksum, downward-API instance id), Service, optional HPA/PDB/NetworkPolicy/Ingress, config-as-Secret or `existingSecret`, **external DSN required plus an in-chart dev Postgres StatefulSet behind a flag**, optional **ServiceMonitor and a PrometheusRule derived from `alerts.yml`** (asserted identical by the validator), and a **`sandbox.mode` tri-state** — `none` (default), `docker-socket` (fails to render without an explicit host-root acknowledgement), `kubernetes` (renders #75's RBAC; refuses until the deployed version ships #75). A template-level **auth guard** refuses to render the dev token silently. Go-driven `helm lint`/`helm template` matrix tests; an operator-only kind smoke.
- **Publishing:** the release workflow packages and pushes the chart to `oci://ghcr.io/ethanhinson/charts`, version-locked to the tag; no floating chart version.
- **`docs/deploying.md`** — compose and Helm quickstarts, the config-as-Secret model, the bash-runtime tradeoff, scaling and rolling-update semantics including the honest mid-turn caveat.

## Out of scope

- Running the bash **sandbox** as k8s Pods — Change #75 (a substrate handler behind a remote seam, gated on its own ADR). This chart renders #75's manifest when told to and otherwise leaves bash unavailable.
- An operator / CRDs — explicitly rejected above unless a future tenant-as-CRD need appears.
- TLS termination (ingress- or mesh-owned; the server speaks h2c), chart signing/attestation, managed-Postgres and secrets-manager specifics beyond values hooks — follow-ons.
- Resuming a mid-turn loop across an instance death — the spec states the gap; the lease model handles reattach, not resumption.

## Reconcile log

<!-- Appended by docket-implement-next's reconcile pass: dated entries of what changed. -->
