---
name: distroless-container-healthcheck
title: Distroless containers have no shell — gate readiness host-side, not via HEALTHCHECK
promotion_state: candidate
changes: [8]
created: 2026-08-04
updated: 2026-08-04
topics: [docker, testing, integration, ci]
---

Distroless container images (e.g. `ghcr.io/navikt/mock-oauth2-server`) ship no shell, no `wget`, and no `curl`. A Docker `HEALTHCHECK` that runs any of those commands always exits 1, so the container is perpetually "unhealthy" and any `depends_on: condition: service_healthy` or CI wait loop that polls the health status will block indefinitely.

**Rule:** For distroless containers in Docker Compose, omit the `HEALTHCHECK` instruction entirely. Gate service readiness host-side: poll an HTTP endpoint from `TestMain` (with a retry loop and timeout), a `make` target using `curl --max-time`, or a CI wait step — whichever owns the orchestration for the environment.

**How to apply:** Remove `healthcheck:` from the Compose service. Add a host-side readiness poll before the test suite runs (or before the dependent service starts). A short `curl --max-time 2 --retry 30 --retry-delay 1 <healthcheck-url>` is usually sufficient.

## War story

(#8, PR #8) — Change 0008 (MCP integration test harness). The `mock-oauth2-server` service used `wget` in its `HEALTHCHECK`, which never ran inside the distroless image. CI blocked until the healthcheck was removed and replaced with a host-side `curl` readiness check in the `make test-integration` target.
