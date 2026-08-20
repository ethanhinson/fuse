---
id: 76
slug: fuse-server-helm-chart-compose-stack
title: fuse server deployment — a container image, a full docker-compose stack, and a Helm chart (not an operator)
status: proposed
priority: medium
type: feat
created: 2026-08-20
updated: 2026-08-20
depends_on: []
related: [75, 77]
discovered_from: [63]
adrs: [31, 34, 30, 33]
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
<!-- docket:artifacts:end -->

## Why

fuse can already run as a networked, multi-tenant, multi-loop **server** — `cmd/fuse loop-serve-net` over the ADR-0033 Connect transport, with edge-enforced auth (ADR-0034), a durable Postgres backend and loop registry (ADR-0031 `internal/event/pgstore`), and reconnect-after-redeploy via the durable ownership lease. What it does **not** have is a way to *deploy* that server: there is no Dockerfile for the server binary, the only compose file is the observability sidecar stack (`deploy/observability/docker-compose.yml` — Prometheus/OTEL/Tempo/Grafana), and there is no Helm chart.

This is the **server layer** of the orchestration question, and it is deliberately separate from the sandbox layer (#75): running fuse itself on k8s is a Helm chart over a stateless-with-external-Postgres service, whereas running the bash *sandbox* on k8s is a substrate handler (#75). Conflating them is the trap this stub exists to avoid.

**Helm chart, not an operator.** An operator earns its keep when there is a custom resource with a non-trivial reconciliation lifecycle (CRDs users author, leader election, stateful failover a StatefulSet can't give). The fuse server is stateless with durable state in Postgres (ADR-0030 "one process hosts N concurrent loops", ADR-0031 "reachable from any instance"), so N replicas behind a Service + HPA is the correct shape. Revisit an operator only if fuse later grows tenant-as-CRD or per-tenant dedicated infrastructure.

## What changes

- **A server container image** (`Dockerfile` / `deploy/`): a minimal image building `cmd/fuse` and running `loop-serve-net`. None exists today; it is the prerequisite for both compose and Helm.
- **A full local docker-compose stack**: fuse server + Postgres (the ADR-0031 pgstore backend) + the existing observability sidecars, so a single `docker compose up` gives a working networked deployment for dev/single-node. Reuse/extend `deploy/observability/docker-compose.yml` rather than forking a second stack.
- **A Helm chart** (`deploy/charts/fuse/`): Deployment (N replicas), Service, HPA, and a `values.yaml` surface for — the auth `Verifier` config (ADR-0034; never a world-known dev token in a real deploy), the pgstore DSN, and the observability wiring (OTEL endpoint, the Prometheus scrape/alerts #63 shipped). Postgres as a chart dependency or a documented external prerequisite.

## Out of scope

- Running the bash **sandbox** as k8s Pods — Change #75 (a substrate handler behind a remote seam, gated on its own ADR). This chart deploys the server, whose bash tool then selects whatever sandbox substrate its config names.
- An operator / CRDs — explicitly rejected above unless a future tenant-as-CRD need appears.
- Sandbox resource limits and per-tenant quotas — Change #77 (though the chart's `values.yaml` should leave a seam for them once they exist).
- Provider-specific hardening (managed Postgres, secrets managers, service meshes) beyond documented values hooks — follow-ons.

## Open questions

<!-- Groomed into a build-ready spec later. -->
- Image shape: distroless/scratch static Go binary vs. a base with the container CLI present — note that a server whose bash tool uses the **local** container substrate (#63) needs a container runtime *reachable from the pod*, which reintroduces the Docker-in-Docker / mounted-socket privilege tradeoff ADR-0044 flagged; a deploy using the #75 remote handler does not. The chart must make this choice explicit rather than assume.
- Postgres: bundled subchart (dev convenience) vs. external-only (production honesty) vs. both behind a flag.
- How the auth `Verifier` config and the pgstore DSN are surfaced as secrets vs. values, honoring ADR-0006/0019 (credential surfaces from trusted config only).
- Whether the observability sidecars belong in the app chart or a separate umbrella/dependency chart.
- Health/readiness probes: what liveness signal the loop-server exposes for a k8s probe, and whether the durable-lease heartbeat (ADR-0034) is the right readiness gate.

## Reconcile log

<!-- Appended by docket-implement-next's reconcile pass: dated entries of what changed. -->
