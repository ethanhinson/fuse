# fuse server — docker-compose dev stack

A single-node stack that runs the released `fuse` server image (`loop-serve-net`)
against Postgres, with the full observability sidecar set (Prometheus, OTEL
collector, Tempo, Grafana) pulled in from `deploy/observability/`.

> ## DEV-ONLY. NOT A PRODUCTION DEPLOYMENT.
>
> Every credential in this directory is a **checked-in placeholder** committed to
> a public repository: the Postgres user/password in `docker-compose.yml` and the
> bearer token in `fuse.compose.yml` are known to anyone who can read the repo.
> Every published port is bound to `127.0.0.1` (ADR-0043), and that loopback bind
> is the only thing keeping those placeholders from being an open door. Do not
> reuse them, do not point this stack at data you care about, and do not derive a
> production deployment from it.
>
> For Kubernetes, use `deploy/charts/fuse`, whose auth guard refuses to render a
> dev token unless you explicitly ask for one.

## Requirements

- Docker Compose **v2.20+** — `docker-compose.yml` uses `include:`.
- The `fuse` image. By default `ghcr.io/ethanhinson/fuse:latest`; override with
  `FUSE_IMAGE`, which also accepts a GoReleaser snapshot tag so the stack is
  testable before a release tag exists:

  ```sh
  goreleaser release --snapshot --clean
  FUSE_IMAGE=ghcr.io/ethanhinson/fuse:0.0.1-SNAPSHOT-amd64 \
    docker compose -f deploy/compose/docker-compose.yml up -d
  ```

## Run it

```sh
# Render and check the merged config without touching the daemon:
docker compose -f deploy/compose/docker-compose.yml config

docker compose -f deploy/compose/docker-compose.yml up -d
docker compose -f deploy/compose/docker-compose.yml logs -f fuse
docker compose -f deploy/compose/docker-compose.yml down -v   # -v drops the volumes
```

| Endpoint | URL | Notes |
|---|---|---|
| Connect / h2c | `http://127.0.0.1:8787` | bearer `fuse-dev-token` (placeholder) |
| liveness | `http://127.0.0.1:8787/healthz` | unauthenticated, status-code only |
| readiness | `http://127.0.0.1:8787/readyz` | 503 until Postgres answers; also the container healthcheck |
| fuse metrics | `http://127.0.0.1:9090/metrics` | `access: public` — dev only |
| Prometheus | `http://127.0.0.1:9091` | port 9091, because 9090 is fuse's |
| Grafana | `http://127.0.0.1:3000` | provisioned dashboards from `deploy/observability/` |
| Tempo | `http://127.0.0.1:3200` | traces, via `otel-collector:4317` |

The container healthcheck is `["CMD", "/fuse", "healthcheck"]`. The image is
distroless — no shell, no curl — so `fuse healthcheck` (defaulting to
`127.0.0.1:8787/readyz`) is the only thing available to probe with, and it is
dispatched before config load so it still answers on a server whose config is
broken.

## The bash tool: the `docker-socket` profile

**By default the bash tool is unavailable and says so at startup.** The released
image is distroless: no shell, no docker CLI, no container runtime, so a bash
tool running inside it has nothing local to contain itself with. ADR-0044 makes a
container boundary the bash tool's security boundary in every profile, so no
runtime means no bash. That is the default, and it is the safe one.

The `docker-socket` profile mounts the host's Docker socket into a second
service, `fuse-docker-socket`:

```sh
docker compose -f deploy/compose/docker-compose.yml \
  --profile docker-socket up -d postgres fuse-docker-socket
```

> **ADR-0044's deployment constraint: socket access is approximately host root.**
> A model-authored bash command running against that socket can start a
> privileged container and own the machine. ADR-0044 requires an implementation
> to *address* this tradeoff rather than assume it away; addressing it here means
> off by default, behind a profile you have to type, and stated plainly. Accept it
> only on a dev machine you are willing to lose.

**Why a second service rather than a conditional mount:** Compose profiles gate
whole *services*, not individual mounts, so a mount cannot be made conditional.
`fuse-docker-socket` is the `fuse` service re-used through a YAML anchor plus the
socket mount, carrying `profiles: ["docker-socket"]` so it neither starts nor
contends for the published ports unless the profile is active. The two are
mutually exclusive by hand — they publish the same host ports — so name the
service you want (as above) instead of bringing the whole profile up, or stop
`fuse` first. Both renders are checked:

```sh
docker compose -f deploy/compose/docker-compose.yml config
docker compose -f deploy/compose/docker-compose.yml --profile docker-socket config
```

## Files

| File | What it is |
|---|---|
| `docker-compose.yml` | `include:`s `../observability/docker-compose.yml`, adds `fuse` + `postgres`, overrides Prometheus's config mount |
| `fuse.compose.yml` | the server config, mounted at `/home/nonroot/.fuse/config.yml` — the trusted home file (ADR-0006) |
| `prometheus.yml` | mounted over the inherited `/etc/prometheus/prometheus.yml`; scrapes `fuse:9090` |

Two merge facts worth knowing before you edit:

- **The Prometheus override is a merge, not a replacement.** Compose merges
  `volumes` by container target, so `./prometheus.yml` replaces the inherited
  `prometheus.yml` mount while the inherited `alerts.yml` mount survives — the
  alert rules stay shared with the standalone stack rather than forked. Confirm
  with `docker compose ... config` after any change here.
- **`deploy/observability/prometheus.yml` must not be retargeted** to `fuse:9090`
  to "simplify" this. `hasFuseScrapeTarget` in `deploy/observability/validate.go`
  asserts that file's exact `host.docker.internal:9090` triple — it is the
  standalone stack's contract, and editing it fails `make test`.

## Two things this stack does that look optional and are not

- **A writable `/tmp`** (the `fuse-tmp` volume). The egress proxy calls
  `os.MkdirTemp("", "fuse-egress")` on the serve path whenever the bash tool is
  enabled, and that error is swallowed: with no writable `/tmp`,
  `egress.mode: enforce` silently degrades to **deny-all** while the container
  stays healthy. The distroless base has no writable `/tmp` of its own.
- **`FUSE_INSTANCE_ID=compose-1`.** `fuse.compose.yml` deliberately leaves
  `observability.instance_id` empty so the env fills it in — the trusted file
  wins when set, and one config file cannot carry N distinct ids for N replicas.
