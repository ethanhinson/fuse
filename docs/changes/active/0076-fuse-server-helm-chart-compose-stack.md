---
id: 76
slug: fuse-server-helm-chart-compose-stack
title: fuse server deployment — a docker-compose stack and a Helm chart over the released image (not an operator)
status: implemented
priority: medium
type: feat
created: 2026-08-20
updated: 2026-09-13
depends_on: [82]
related: [75, 77, 82, 63]
discovered_from: [63]
adrs: [31, 34, 30, 33, 44]
spec: docs/superpowers/specs/2026-09-13-fuse-server-helm-chart-compose-stack-design.md
plan: docs/superpowers/plans/2026-09-13-fuse-server-helm-chart-compose-stack-plan.md
results: docs/results/2026-09-13-fuse-server-helm-chart-compose-stack-results.md
trivial: false
auto_groomable:
branch: feat/fuse-server-helm-chart-compose-stack
claimed_at: 2026-09-13T23:22:10Z
pr: https://github.com/ethanhinson/fuse/pull/90
blocked_by:
reconciled: true
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-09-13-fuse-server-helm-chart-compose-stack-design.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/2026-09-13-fuse-server-helm-chart-compose-stack-design.md) |
| Plan | [2026-09-13-fuse-server-helm-chart-compose-stack-plan.md](https://github.com/ethanhinson/fuse/blob/feat/fuse-server-helm-chart-compose-stack/docs/superpowers/plans/2026-09-13-fuse-server-helm-chart-compose-stack-plan.md) |
| Results | [2026-09-13-fuse-server-helm-chart-compose-stack-results.md](https://github.com/ethanhinson/fuse/blob/feat/fuse-server-helm-chart-compose-stack/docs/results/2026-09-13-fuse-server-helm-chart-compose-stack-results.md) |
| PR | [#90](https://github.com/ethanhinson/fuse/pull/90) |
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

### 2026-09-13 — reconcile before build

Verified every load-bearing claim in the spec against `origin/main` @ `b966b5e` (local `main`
equals `origin/main`, so on-disk reads are valid here — the
`reconcile-verify-claims-against-origin-not-working-tree` learning's stale-tree hazard does not
apply). **The design holds in full; no decision is invalidated.** Scope is unchanged. The spec's
six `## Open build questions` are all resolved below, plus four mechanics corrections.

**Claims confirmed as written**

- `cmd/fuse/durable_backend_pg.go` is `//go:build pgstore`; the untagged `durable_backend.go` wires
  fsstore at `session.DefaultLogDir()`. Neither `.goreleaser.yaml` (`builds:` → `flags: [-trimpath]`,
  no `tags:`) nor the Makefile's `build`/`install` sets the tag. `pgDSN()` reads `FUSE_PG_DSN` then
  `DATABASE_URL`. The no-pgx-import constraint is a **comment only** — no test enforces it — so
  adding the tag to the Makefile breaks nothing.
- No `/healthz` or `/readyz` anywhere. `serveNetObserved` registers exactly: the Connect path, the
  metrics path (only when `metrics.bind` is empty), and three `observability*Path` admin routes.
  The auth interceptor is a `connect.HandlerOption` on the **Connect handler only**, so new plain
  `mux.Handle` routes are unauthenticated by construction — no interceptor bypass needed.
- `serveNetContext` is `signal.NotifyContext(context.Background(), os.Interrupt)` — SIGTERM absent.
  Shutdown is a goroutine doing a bare `srv.Close()` on `ctx.Done()`; there is **no**
  `srv.Shutdown` call at all, so today a stop is an immediate listener close.
- `observability.instance_id` has no env or hostname fallback: `config.Config.Observability.InstanceID`
  is read directly at `cmd/fuse/observability.go:153` and `:173`.
- No `event.Pinger`. `internal/event` exports `DurableStore`, `CommittedDurableStore`, `LoopRegistry`
  — `CommittedDurableStore` is the established **optional-interface** precedent a `Pinger` follows.

**Resolved open build questions**

1. **Compose `include:`** — available. The pinned local toolchain is Docker Compose **v5.1.2**,
   far past the v2.20 floor; `include:` is used as designed, no `extends` fallback.
2. **Distroless healthcheck** — take the spec's stated default: add a `fuse healthcheck` subcommand
   that dials `/readyz` and exits 0/1. It gives compose a real `healthcheck` on a shell-less image
   *and* Kubernetes an `exec`-probe option, and it is the only option that does not document a
   limitation in place of a check.
3. **`docker compose config` in CI** — runs daemon-free. It is already relied on that way by the
   existing `observability-compose-smoke` target, so the pattern is proven in this repo.
4. **`readOnlyRootFilesystem` vs `/tmp`** — **an emptyDir at `/tmp` IS required.** Two real writers
   outside `~/.fuse`: `internal/tools/sandbox/egress_proxy.go:407` (`os.MkdirTemp("", "fuse-egress")`
   for the per-principal socket root — on the serve path whenever the bash tool is enabled) and
   `cmd/fuse/cli_adapter.go:58` (a HITL socket under `os.TempDir()`). Both compose and the chart
   mount a writable `/tmp`.
5. **ServiceMonitor bearer token** — do **not** auto-mint a scrape principal into `auth.tokens`.
   Synthesizing a credential inside a template hides a real token from the operator who owns the
   auth surface; `metrics.serviceMonitor.bearerSecret` stays an explicit reference, and the default
   `metrics.access: public` needs none.
6. **`fuse version` golden test** — the spec's proposed `fuse 0.3.0 (backends: fsstore,pgstore)`
   **would break `TestVersionSubcommand`**, which asserts `lines[0] == "fuse " + version.Version`
   by exact equality (`cmd/fuse/version_test.go:29`). The backend inventory therefore goes on its
   own **third line** (`backends: fsstore,pgstore`), set by a build-tagged var. The existing test
   checks the Go/platform line with `strings.Contains`, so an added line is already tolerated, and
   `TestVersionFlagAliases`/`TestVersionPrecedesConfigLoad` stay green.

**Mechanics corrections (no scope change)**

- **`release-dry-run` is in `.github/workflows/integration.yml`, not `release.yml`.** The spec puts
  the packaging dry-run "in the `release-dry-run` job", which is correct — the file is not. The
  `helm lint`/`helm template`/`helm package --version 0.0.0-dev` and `docker compose config` CI
  steps land in `integration.yml`'s `release-dry-run`; only the real `helm push` goes in
  `release.yml`.
- **The observability validator is not generalisable by a `root` parameter alone.**
  `deploy/observability/validate.go`'s `validate(root)` demands the full four-sidecar shape
  (prometheus+grafana+otel-collector+tempo services, two provisioned datasources, two dashboard
  JSONs) — pointing it at `deploy/compose/` would fail on artifacts that stack legitimately does
  not own. Implemented instead as a **second, narrower entry point in the same `main` package**
  reusing `registeredMetrics`/`requireRegisteredMetrics`/`readYAML`: it checks the compose stack's
  `prometheus.yml` (registered metric names, and a `fuse` job whose target is `fuse:9090`) and
  asserts the chart's PrometheusRule groups equal `alerts.yml`. `hasFuseScrapeTarget` hard-codes
  `host.docker.internal:9090`, so the target becomes a parameter rather than a second literal.
- **`sandbox.mode: kubernetes` needs no file from #75.** The spec's table says the mode "renders
  `deploy/k8s/sandbox-rbac.yaml`'s objects"; that path does not exist on `origin/main` (#75 is still
  `proposed`). Because `sandbox.kubernetes.minVersion` stays `""` in this change, the guard
  **refuses to render before any manifest is consulted** — so the mode is implemented as
  fail-with-a-clear-message only, and #75 supplies both the manifest and the pinned `minVersion`
  when it lands. This is the seam the change already promised, not a reduction of it.
- **`make test` gates on `observability-validate`** (`Makefile:58`), so the new compose/chart
  validation joins that target and runs in the default suite. The kind and compose smokes follow
  `observability-compose-smoke`'s established skip-clean-and-say-so shape (`Makefile:150`).

**Discovered, not captured** (`auto_capture.enabled` is `false`, so reported in prose only): the
untagged-build "must stay pgx-free" invariant asserted in `cmd/fuse/durable_backend.go`'s header
comment has **no test enforcing it**, unlike the sibling import-direction gates in
`internal/runtime/import_direction_test.go` and `internal/event/store_hooks_test.go`. Worth its own
`chore` change; deliberately out of scope here, since this change's whole point is that the shipped
binary *is* tagged.

**Three build-time traps confirmed by a second verification pass** (same commit, independent sweep):

- **`observability-validate` is `go run ./deploy/observability/validate.go` — a SINGLE-FILE invocation**
  (`Makefile:133`). A new `.go` file in that package is therefore **not compiled by that target**,
  only by `go test ./...`. The Makefile target must name both files (or become
  `go run ./deploy/observability`), otherwise the new validation silently never runs in the
  `make test` gate it is supposed to join.
- **The compose stack needs its OWN `prometheus.yml`; the observability one must not be edited.**
  `hasFuseScrapeTarget` asserts the exact triple `job_name: fuse` / `metrics_path: /metrics` /
  target `host.docker.internal:9090`, so retargeting the existing file to `fuse:9090` would fail
  `make test`. The spec's "overrides Prometheus's config mount" is the right mechanic: a new file
  under `deploy/compose/`, mounted over the same container path, leaving the standalone stack green.
- **Under a read-only root FS with no writable `/tmp`, egress fails CLOSED and SILENTLY.**
  `NewProxy`'s `os.MkdirTemp` error is caught by `warnEgressBlackout`
  (`cmd/fuse/sandbox.go:479-484`) and `egress.mode: enforce` degrades to deny-all rather than
  crashing the server. So the `/tmp` emptyDir is not a crash-avoidance detail — without it a
  deployment looks healthy while every declared egress destination is unreachable. Worth an
  assertion in the chart tests that `/tmp` is mounted whenever `readOnlyRootFilesystem` is set.
