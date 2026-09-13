<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0076 — fuse server deployment — a docker-compose stack and a Helm chart over the released image (not an operator)](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0076-fuse-server-helm-chart-compose-stack.md)**
<!-- docket:backlink:end -->

# fuse server deployment — a docker-compose stack and a Helm chart over the #82 image

Design spec for change #0076. Turns the published server image (#82) into two
runnable deployments — a single-node docker-compose stack for dev, and a Helm
chart for Kubernetes — plus the three small server-side changes without which
neither deployment is honest. Depends on #82 (image + release pipeline), which
is `done`. Related to #75 (the Kubernetes sandbox substrate), which the chart
leaves a seam for and does not require.

## Problem

`fuse loop-serve-net` is a networked, multi-tenant, multi-loop server: Connect
over h2c (ADR-0033), bearer-token auth at the edge (ADR-0034), a durable
Postgres event store and loop registry with an ownership lease (ADR-0031), so a
client can reattach after a redeploy on a different instance. #82 built its
image: `ghcr.io/ethanhinson/fuse`, distroless, entrypoint `/fuse`, no shell, no
container runtime.

What does not exist is a way to *run* that image as a service. The only compose
file is the observability sidecar stack; there is no Helm chart. And reading the
code for this groom surfaced three gaps that turn "no chart" into "no
deployable server":

1. **The released binary has no Postgres backend.** The pgstore backend and its
   selector (`cmd/fuse/durable_backend_pg.go`) are behind `//go:build pgstore`,
   and neither `.goreleaser.yaml` nor the Makefile sets the tag. The image runs
   the filesystem store under `/home/nonroot/.fuse/sessions`; two replicas share
   nothing, and a redeploy loses every loop. The DSN plumbing (`FUSE_PG_DSN`,
   else `DATABASE_URL`) is already there and unreachable.
2. **No liveness or readiness endpoint.** The mux serves the Connect path, the
   metrics path (when `metrics.bind` is empty), and three authenticated admin
   routes. A Kubernetes probe has nothing to hit.
3. **SIGTERM is not handled.** `serveNetContext` traps `os.Interrupt` only. A
   Kubernetes or compose stop delivers SIGTERM, Go's default handler exits
   immediately, and the deferred cleanups — egress proxy sockets, sandbox pools,
   the store's LISTEN connection — never run.

Two more facts shape the deployment: the config file is a **trusted home file**
(`~/.fuse/config.yml`; `loop_server.auth`, `observability`, and
`tool_identity` are honored from nowhere else, ADR-0006), and with no
`loop_server.auth` the server synthesizes a loudly-logged dev token rather than
running unauthenticated (ADR-0034). So a deployment's config is a Secret, and a
production render must refuse to fall back to the dev token silently.

## Scope decisions (settled at groom, 2026-09-13)

- **`-tags pgstore` on every release build.** One binary, one image. With no
  DSN the tagged selector still falls through to fsstore, so the CLI archives
  are unaffected in behaviour; the cost is pgx in the binary. `make build` and
  `make install` gain the same tag so a local build matches the release shape.
  (Human's call.)
- **Postgres in Helm: external DSN required, plus an in-chart dev
  StatefulSet behind `postgres.dev.enabled`.** No third-party subchart
  dependency. (Human's call.)
- **Observability in Helm: expose the endpoints, and optionally render a
  ServiceMonitor and a PrometheusRule** derived from the existing
  `deploy/observability/alerts.yml`. The sidecars (Prometheus, OTEL collector,
  Tempo, Grafana) stay in the compose dev stack. (Human's call.)
- **Helm chart, not an operator** — unchanged from the stub: stateless
  replicas over Postgres behind a Service, HPA optional.
- **`/healthz` and `/readyz` are unauthenticated and status-code-only.** Health
  is not a loop verb and carries nothing tenant-scoped; a 503 says nothing
  about *why* (reasons go to the log), so the endpoint is not an oracle.
- **SIGTERM + bounded graceful drain.** Readiness flips to 503 first, the
  server stops accepting, in-flight unary calls finish within
  `--drain-timeout` (default 20s), then Close. Observe streams are cut by
  design — a dropped stream is normal and clients reattach (ADR-0033).
- **Config is a Secret mounted at the nonroot home**, DSN via
  `FUSE_PG_DSN` from a Secret, per-replica instance id via the downward API.
- **The bash tool's runtime is an explicit tri-state in values**, per
  ADR-0044: `none` (default; the tool reports unavailable), `docker-socket`
  (opt-in behind an acknowledgement flag; documented as ≈ host root), and
  `kubernetes` (renders #75's RBAC; refuses to render until the deployed fuse
  version carries the handler). Compose gets the same choice as a profile.
- **The chart is published as an OCI artifact by the release pipeline**,
  version-locked to the image tag, in the same workflow #82 built.
- **One compose stack, not two.** The new stack `include`s the observability
  file rather than forking it.

## Design

### 1. Server-side prerequisites (code)

**1a. Build tag.** `.goreleaser.yaml` build `fuse` gains `tags: [pgstore]`;
the Makefile's `build`/`install` gain `-tags pgstore`. `fuse version` reports
the backend set (`fuse 0.3.0 (backends: fsstore,pgstore)`) so the
`release-dry-run` job can assert the tag took, and an operator can check a
binary without a database. `go install` users who want Postgres pass the tag
themselves; the README says so.

**1b. Health endpoints.** Two routes on the main mux, before the Connect
handler, never behind the auth interceptor:

- `GET /healthz` → `200 ok` once `Serve` is running. Liveness: the process
  answers HTTP.
- `GET /readyz` → `200 ok` when (a) the durable store answers a cheap probe,
  (b) the verifier is constructed, and (c) the server is not draining.
  Otherwise `503` with an empty body. The store probe is a new optional
  `event.Pinger{ Ping(ctx) error }` implemented by pgstore (`pool.Ping`) and
  fsstore (`Stat` of the base dir); a store that does not implement it is
  treated as ready. Probe results are cached for 2s so a probe storm cannot
  become a Postgres storm.

**1c. Signals and drain.** `serveNetContext` traps `os.Interrupt` **and**
`syscall.SIGTERM`. On cancel: readiness → 503; `srv.Shutdown(ctx, drain)` with
`--drain-timeout` (flag, default 20s) then `Close`; then the existing deferred
cleanups run in order. The runtime keeps renewing loop leases until the process
exits, so a loop owned by the draining instance is abandoned only when its
lease expires; another instance re-owns it on the next resolve (ADR-0034).
What this change does **not** promise: resuming a turn that was mid-flight on
the dying instance. The loop's events are durable; its in-memory execution is
not, and the client sees a gap marker and a parked loop, exactly as after a
crash. This is stated in the deploy doc, not hidden.

**1d. Instance identity from the environment.** `observability.instance_id`
empty ⇒ `FUSE_INSTANCE_ID` env ⇒ hostname. A config file shared by N replicas
cannot carry N ids; the chart sets `FUSE_INSTANCE_ID` from the Pod name. The
env is consulted only when the config leaves the field empty — the trusted
file still wins.

**1e. Writable home.** The server writes under `~/.fuse/` (`sessions/` for
fsstore, `workspaces/` for the hosted sandbox root, `mcp-tokens/`,
`skills/`). Both deployments mount a writable volume at `/home/nonroot/.fuse`
and the config **file** over it at `/home/nonroot/.fuse/config.yml` (Secret
`subPath`), so `readOnlyRootFilesystem: true` holds for everything else.

### 2. The compose stack — `deploy/compose/`

```
deploy/compose/
  docker-compose.yml      # include: ../observability/docker-compose.yml
  fuse.compose.yml        # the server config, DEV token, loud header
  prometheus.yml          # job fuse → fuse:9090 (overrides the host.docker.internal target)
  README.md
```

- `include:` pulls the four observability services in unchanged; this file
  adds `fuse` and `postgres` and overrides Prometheus's config mount so the
  scrape target is the `fuse` service, not `host.docker.internal`. The
  observability stack still works standalone.
- `fuse`: `image: ${FUSE_IMAGE:-ghcr.io/ethanhinson/fuse:latest}`; command
  `loop-serve-net --addr 0.0.0.0:8787`; `FUSE_PG_DSN` pointing at `postgres`;
  `FUSE_INSTANCE_ID=compose-1`; volumes: a named volume at
  `/home/nonroot/.fuse`, `./fuse.compose.yml` at
  `/home/nonroot/.fuse/config.yml:ro`; ports `127.0.0.1:8787:8787` and
  `127.0.0.1:9090:9090` (loopback publish, ADR-0043's posture); `depends_on:
  postgres: condition: service_healthy`; healthcheck via `/readyz`
  (distroless has no curl — the healthcheck is a `docker compose` external
  check documented in the README, or omitted; see open questions).
- `postgres`: `postgres:16-alpine`, a named volume, `pg_isready` healthcheck,
  credentials in the compose file **because this stack is dev-only**, stated in
  the same header the observability file already carries.
- `fuse.compose.yml`: `loop_server.auth` with the documented dev token and
  `_default` tenant, `observability` with `metrics.bind: 0.0.0.0:9090`,
  `access: public`, traces to `otel-collector:4317`, logging to stdout, the
  required cardinality block. The header says, in the words #60's demo config
  uses, that every credential is a checked-in placeholder.
- **bash runtime**: profile `docker-socket` adds
  `/var/run/docker.sock:/var/run/docker.sock` to `fuse`, with a comment block
  quoting ADR-0044's tradeoff. Without the profile the bash tool is
  unavailable and says so at startup; the README shows both.
- `FUSE_IMAGE` accepts a GoReleaser snapshot image so the stack is testable
  before a tag.

**Validation.** `deploy/observability/validate.go` is generalised to walk both
directories: the compose stack's `prometheus.yml` must name only registered
metrics (it inherits the alerts file, which it already checks), and `docker
compose -f deploy/compose/docker-compose.yml config` runs in
`compose-validate` (CI, Docker-free via `docker compose config` — see open
questions) and the operator-only `compose-smoke` target brings it up, waits on
`/readyz`, scrapes `/metrics`, and runs one `loop.start` with the Go SDK.

### 3. The Helm chart — `deploy/charts/fuse/`

`Chart.yaml`: `apiVersion: v2`, `version` and `appVersion` stamped from the
release tag at package time (the checked-in values are `0.0.0-dev`).

Templates and the values that drive them:

| Template | Renders | Key values |
|---|---|---|
| `deployment.yaml` | N replicas of `loop-serve-net --addr 0.0.0.0:8787 --drain-timeout <d>`; env `FUSE_PG_DSN` (secretKeyRef), `FUSE_INSTANCE_ID` and `FUSE_POD_IP` (downward API — `FUSE_POD_IP` is #75's claim); probes on `/healthz` (liveness) and `/readyz` (readiness + startup); `securityContext` nonroot, `readOnlyRootFilesystem`, drop ALL; emptyDir at `/home/nonroot/.fuse`; config Secret `subPath`; a checksum annotation of the config so a changed Secret rolls the pods; `terminationGracePeriodSeconds` = drain + 10 | `replicaCount`, `image.{repository,tag,digest}`, `server.drainTimeout`, `resources`, `nodeSelector`/`tolerations`/`affinity` |
| `service.yaml` | ClusterIP; ports `connect` 8787 and `metrics` 9090 | `service.type`, `service.annotations` |
| `hpa.yaml` (opt) | CPU-based HPA | `autoscaling.{enabled,min,max,targetCPU}` |
| `pdb.yaml` (opt) | PodDisruptionBudget | `podDisruptionBudget.{enabled,minAvailable}` |
| `secret-config.yaml` | the whole `config.yml` rendered from `config:` (a free-form map merged over chart defaults) **or** skipped when `config.existingSecret` is set | `config`, `config.existingSecret`, `auth.tokens[]`, `auth.allowDevToken` |
| `secret-dsn.yaml` (opt) | `FUSE_PG_DSN` when given inline; else `postgres.existingSecret` | `postgres.dsn`, `postgres.existingSecret`, `postgres.dev.*` |
| `postgres-dev.yaml` (opt) | one-replica StatefulSet + Service + Secret, `postgres:16-alpine`, a small PVC; the template header and NOTES say **not for production** | `postgres.dev.{enabled,storage,image}` |
| `servicemonitor.yaml` (opt) | ServiceMonitor on the `metrics` port; bearer token from a Secret when `metrics.access: authenticated` | `metrics.serviceMonitor.{enabled,interval,bearerSecret}` |
| `prometheusrule.yaml` (opt) | the groups from `deploy/observability/alerts.yml`, copied in by `make chart` and asserted identical by the validator | `metrics.prometheusRule.enabled` |
| `networkpolicy.yaml` (opt) | ingress to 8787/9090 from selected namespaces; egress to Postgres, the gateway, OTLP | `networkPolicy.*` |
| `ingress.yaml` (opt) | HTTP ingress; NOTES explains Connect works over HTTP/1.1 but gRPC clients need an h2-capable ingress | `ingress.*` |
| `sandbox-*.yaml` (mode-gated) | `none`: nothing. `docker-socket`: hostPath mount + **template fails** unless `sandbox.dockerSocket.acknowledgeHostRoot: true`; NOTES prints the ADR-0044 warning. `kubernetes`: renders `deploy/k8s/sandbox-rbac.yaml`'s objects and **fails** unless `image.tag` ≥ the version that ships #75 (a values-level `sandbox.kubernetes.minVersion` the build pins once #75 lands, `""` until then ⇒ always fails with "not yet available") | `sandbox.mode` |
| `serviceaccount.yaml` | the server's ServiceAccount | `serviceAccount.*` |

**Auth guard.** `helm template` fails unless `auth.tokens` is non-empty,
`config.existingSecret` is set, or `auth.allowDevToken: true`. The dev token is
never the silent default of a rendered chart, mirroring the server's own
loud-but-usable posture.

**Rolling update semantics** (documented in NOTES and the deploy doc):
`maxUnavailable: 0`, `maxSurge: 1`; a new pod is not Ready until `/readyz`
passes (store reachable); the old pod drains. Loop ownership follows the lease
as in §1c.

**Validation.** `deploy/charts/validate_test.go` (Go, skip-clean when `helm` is
absent) runs `helm lint` and `helm template` under a matrix of values —
default, `auth.allowDevToken`, `sandbox.mode: docker-socket` with and without
the acknowledgement, `postgres.dev.enabled`, `metrics.serviceMonitor.enabled` —
and asserts: the auth guard fires; no rendered Pod lacks probes,
`readOnlyRootFilesystem`, or the config checksum; the PrometheusRule groups
equal `alerts.yml`. `make helm-smoke` (operator-only, like
`observability-compose-smoke`) installs into kind with `postgres.dev.enabled`
and runs the same readiness + `loop.start` check as the compose smoke.

### 4. Publishing

`release.yml` gains, after GoReleaser and the image attestation: `helm package
deploy/charts/fuse --version ${TAG#v} --app-version ${TAG#v}` and `helm push
… oci://ghcr.io/ethanhinson/charts`. Pre-release tags publish a pre-release
chart version; there is no floating chart tag (same rule as the image's
`:latest`). The `release-dry-run` job packages without pushing. Chart
provenance attestation is a follow-on.

### 5. Documentation — `docs/deploying.md`

Compose quickstart (`docker compose -f deploy/compose/docker-compose.yml
up`, the dev token, where the Grafana is); Helm quickstart (`helm install fuse
oci://ghcr.io/ethanhinson/charts/fuse --set auth.tokens[0].token=…`); the
config-as-Secret model and which keys are credential surfaces; the bash-runtime
tri-state and what each costs; scaling and rolling-update semantics, including
the mid-turn caveat from §1c; ingress and h2; the image-has-no-shell facts
(no `exec` debugging; use the endpoints and logs).

## Recorded, not built

- **Chart provenance attestation / cosign** — with #82's own signing follow-on.
- **A `fuse-server` split image** — rejected for now (one binary); revisit if
  pgx weight becomes a CLI complaint.
- **Postgres schema migrations as a Job** — pgstore applies its embedded
  schema on `Open` today; a pre-upgrade Job becomes necessary only when a
  migration stops being idempotent.
- **Multi-region / active-active** — the lease model is single-registry.

## Out of scope

- The Kubernetes sandbox handler and its RBAC semantics — #75. This chart
  renders #75's manifest when told to and otherwise leaves bash unavailable.
- An operator or CRDs.
- TLS termination — the ingress or mesh owns it; the server speaks h2c.
- Managed-Postgres, secrets-manager, and service-mesh specifics beyond values
  hooks.
- Resuming a mid-turn loop across an instance death (§1c states the gap).

## Open build questions (for the reconcile/plan pass)

- Compose `include:` needs Compose v2.20+; confirm the pinned CI version, else
  fall back to `extends`/`-f` stacking.
- A distroless image has no `curl`/`wget` for a compose `healthcheck`; either
  document an external check, or add a `fuse healthcheck` subcommand that
  dials `/readyz` (cheap, and it also gives Kubernetes an `exec` probe
  option). Default here: add the subcommand.
- Whether `docker compose config` can run in CI without a daemon (it can, but
  the runner image must have the plugin).
- `readOnlyRootFilesystem` vs. anything that writes outside `~/.fuse`
  (`os.TempDir` users?) — grep at plan time; add `/tmp` emptyDir if needed.
- ServiceMonitor bearer token: whether to mint a dedicated scrape principal in
  `auth.tokens` automatically when `metrics.access: authenticated`.
- Confirm `fuse version`'s backend line does not break the existing
  `TestVersionSubcommand` golden.

## Acceptance

- The released binary and image report `pgstore` in `fuse version`; with
  `FUSE_PG_DSN` set, two replicas share loops (a loop started on one is
  observable from the other).
- `docker compose -f deploy/compose/docker-compose.yml up` yields a server
  answering `/readyz` 200, Prometheus scraping it, and a `loop.start` over
  the SDK succeeding with the dev token.
- `helm template` fails without auth unless `allowDevToken`; fails for
  `sandbox.mode: docker-socket` without the acknowledgement; fails for
  `sandbox.mode: kubernetes` until #75 ships.
- `helm install` on kind with `postgres.dev.enabled` reaches Ready; a rolling
  restart keeps at least one Ready replica throughout and a client reattaches
  to its loop afterwards.
- SIGTERM drains: readiness flips to 503 before the listener closes, unary
  calls in flight complete, and the process exits within the grace period with
  the egress proxy directory removed.
- The PrometheusRule rendered by the chart equals `alerts.yml`; the validator
  fails if either drifts.
- The release workflow publishes `oci://ghcr.io/ethanhinson/charts/fuse:<ver>`
  alongside the image.
