# Deploying the fuse server

`fuse loop-serve-net` is the networked agent-loop server: Connect over h2c (ADR-0033),
bearer-token auth at the edge (ADR-0034), and a durable Postgres event store so a client
reattaches to its loop after a redeploy landed it on a different replica (ADR-0031).

Two supported deployments ship in this repository:

| | Where | What it is |
|---|---|---|
| docker compose | [`deploy/compose/`](../deploy/compose) | a single-node **dev** stack — fuse, Postgres, and the full observability sidecar set |
| Helm chart | [`deploy/charts/fuse/`](../deploy/charts/fuse) | a Kubernetes Deployment over the released image, publishable to GHCR as an OCI chart |

There is no operator and no CRD. Both deployments run the **released image**,
`ghcr.io/ethanhinson/fuse`, unmodified.

## The binary you need: `-tags pgstore`

The Postgres event store is behind a build tag. `fuse version` prints the backends it was
compiled with on its **third line**:

```
$ fuse version
fuse 0.3.0
go1.24.0 darwin/arm64
backends: fsstore,pgstore
```

- The **released binaries and image**, and `make build`, are built with `-tags pgstore` ⇒
  `backends: fsstore,pgstore`.
- A plain `go install` or an untagged `go build` gives you `backends: fsstore` — no Postgres.
  `make build BUILD_TAGS=` reproduces that untagged binary if you need to see it.

So if you are installing from source and intend to point fuse at Postgres, pass the tag:

```sh
go install -tags pgstore github.com/ethanhinson/fuse/cmd/fuse@latest
fuse version | sed -n 3p     # must say: backends: fsstore,pgstore
```

Deploying the image needs none of this — the image already has both backends. Check the third
line before you debug a DSN that "does nothing": an fsstore-only binary ignores it.

## Health endpoints

Two routes, both **unauthenticated by construction** — they sit on the mux ahead of the auth
interceptor, so a probe never needs a token:

| Route | Meaning |
|---|---|
| `GET /healthz` | **liveness.** 200 once `Serve` is running. "The process answers HTTP." |
| `GET /readyz` | **readiness.** 200 only when the durable store answers a cheap `Ping`, **and** the verifier is constructed, **and** the server is not draining. Otherwise **503 with an empty body**. |

The 503 body is empty on purpose: `/readyz` is a status code, not an oracle. The *reason* goes
to the log and nowhere else. Probe results are cached for 2s, so a probe storm cannot become a
Postgres storm.

Use `/healthz` for liveness and **never** `/readyz`: readiness goes 503 when Postgres is
unreachable, and a liveness probe on it would restart the whole fleet during a database blip
instead of just removing it from the Service.

**Startup is strict.** When a DSN is configured and Postgres cannot be opened at startup, the
server refuses to start — it logs `refusing to start on the filesystem store` and exits 1 —
rather than silently falling back to the per-loop filesystem store. That fallback would report
Ready (a nil store is "nothing to probe"), accept loops, and lose every one of them at the next
restart; it happened live when the server and the dev Postgres StatefulSet started concurrently.
Under Kubernetes the refusal is a restart until the database is up (the startup probe budget
covers it); under Compose `depends_on: service_healthy` means it never fires.

### `fuse healthcheck`

```sh
fuse healthcheck [--addr 127.0.0.1:8787] [--path /readyz] [--timeout 2s]
```

Exits 0 or 1. It is dispatched **before config load**, so it still answers on a server whose
config file is broken — and it is the only probe the image can run itself (see below).

## Compose quickstart

```sh
docker compose -f deploy/compose/docker-compose.yml up -d
docker compose -f deploy/compose/docker-compose.yml logs -f fuse
docker compose -f deploy/compose/docker-compose.yml down -v
```

That brings up fuse, Postgres, and the observability stack (Prometheus, OTEL collector, Tempo,
Grafana). `FUSE_IMAGE` overrides the image — including a GoReleaser snapshot tag, so the stack
is testable before a release tag exists. Connect is on `127.0.0.1:8787` and `/metrics` on
`127.0.0.1:9090`; every published port is bound to loopback (ADR-0043). The container
healthcheck is `["CMD", "/fuse", "healthcheck"]`.

The dev bearer token and the `_default` tenant live in `deploy/compose/fuse.compose.yml`.
**Every credential in that directory is a checked-in placeholder** committed to a public
repository, and the loopback bind is the only thing keeping them from being an open door.

The bash tool is unavailable in this stack by default; the `docker-socket` profile is a sibling
service, mutually exclusive with `fuse` on the same ports:

```sh
docker compose -f deploy/compose/docker-compose.yml \
  --profile docker-socket up -d postgres fuse-docker-socket
```

**Name those services — do not run `--profile docker-socket up` bare.** A Compose profile gates
only the *profiled* service, and `fuse` has no `profiles:` key, so it starts on every invocation
including that one. Both services publish `127.0.0.1:8787` and `:9090`, so the bare form dies with
a port-bind error. Compose cannot express "exclude this service while that profile is active" — a
service with no `profiles:` key is always in the default set — which is why the command names what
it wants instead.

Read that tradeoff in the tri-state section below before you type it.

**Full details — ports, the Prometheus override merge, the profile, the files —** are in
[`deploy/compose/README.md`](../deploy/compose/README.md). This page does not duplicate them.

## Helm quickstart

The chart is published as an OCI artifact. `helm push` appends the chart name, so the reference
is `oci://ghcr.io/ethanhinson/charts/fuse`, and the chart version is version-locked to the image
tag:

```sh
helm install fuse oci://ghcr.io/ethanhinson/charts/fuse --version <version> \
  --set 'auth.tokens[0].token=<a real bearer token>' \
  --set 'auth.tokens[0].tenant=_default' \
  --set postgres.dsn='postgres://user:pass@pg.default.svc:5432/fuse?sslmode=require'
```

For a throwaway cluster with no external database, the dev shortcuts — **neither is for
production**:

```sh
helm install fuse oci://ghcr.io/ethanhinson/charts/fuse --version <version> \
  --set auth.allowDevToken=true \
  --set postgres.dev.enabled=true
```

Read [`deploy/charts/fuse/values.yaml`](../deploy/charts/fuse/values.yaml) for the real keys —
it is the reference, and every non-obvious one carries its reasoning inline.

### Two guards: the chart refuses to render without an answer

1. **Auth.** One of `auth.tokens`, `config.existingSecret`, or `auth.allowDevToken=true`. With
   no `loop_server.auth` the server synthesizes a loudly-logged dev token rather than running
   unauthenticated (ADR-0034) — fine for a binary, wrong as the silent default of a rendered
   chart, since that token is published in this repository.
2. **A DSN.** One of `postgres.dsn`, `postgres.existingSecret`, or `postgres.dev.enabled=true`
   (dev-only). Without one the server falls back to the filesystem store in an emptyDir:
   replicas share no loops, a redeploy loses all of them, and `/readyz` stays green the whole
   time — because an fsstore in a writable directory genuinely *is* healthy. Nothing would tell
   you at runtime, so the chart refuses at render time.

> **`helm lint` does NOT surface these guards.** It logs them as `[INFO] Fail:` lines and still
> exits 0 with "0 chart(s) failed". Only `helm template` (or `helm install`) actually fails.
> Gate CI on `helm template`.

### Config is a Secret, not a ConfigMap

The chart renders `.Values.config` (plus `auth.tokens`, layered in as `loop_server.auth`) into a
**Secret**, mounted by `subPath` at `/home/nonroot/.fuse/config.yml` — the trusted home file
(ADR-0006). It is a Secret because `loop_server.auth`, `observability`, and `tool_identity` are
honored from *that file and nowhere else*. Set `config.existingSecret` to supply your own
instead (it must carry a `config.yml` key); the chart then renders none of its own.

The Deployment carries a **checksum annotation over the rendered config**, so changing the
Secret rolls the pods. When `config.existingSecret` is set the checksum cannot see inside the
Secret, and the annotation says so rather than letting a stable checksum read as proof nothing
changed.

**The credential surfaces are `auth.tokens`, `postgres.dsn`, and `config`.** A Kubernetes Secret
is base64, not encryption: anyone with `get secrets` in the namespace can read them, and so can
anyone who can read the values file you passed. For a real deployment prefer
`config.existingSecret` / `postgres.existingSecret` fed by your secrets manager, and keep tokens
out of your git history.

`metrics.serviceMonitor.bearerSecret` is likewise an **explicit** reference. The chart will not
mint a scrape principal into `auth.tokens` for you: a synthesized credential would never appear
in your values, would rotate silently on every `helm upgrade`, and nobody would know which
principal was reading the fleet's metrics. Create the token, put it in `auth.tokens` *and* in a
Secret, and name the Secret there.

### Instance identity

`observability.instance_id` is consulted first from the **trusted config file**; only when it is
left empty does `FUSE_INSTANCE_ID` apply, and failing that `os.Hostname()`. One Secret shared by
N replicas cannot carry N ids, so the chart leaves the field empty and sets `FUSE_INSTANCE_ID`
from `metadata.name` through the downward API.

### A writable `/tmp` is required, not a nicety

The egress proxy creates its per-principal socket root with `os.MkdirTemp("", "fuse-egress")` on
the serve path whenever the bash tool is enabled — and **that error is swallowed**. Without a
writable `/tmp`, `egress.mode: enforce` silently degrades to **deny-all** while the pod stays
Ready: every declared destination is unreachable and nothing is unhealthy. The chart mounts an
emptyDir at `/tmp` (and one at `/home/nonroot/.fuse`), which is what lets
`readOnlyRootFilesystem: true` hold for everything else; the compose stack mounts a volume for
the same reason. Do not "clean up" either mount.

## The bash tool: a tri-state, and what each option costs

`sandbox.mode` — ADR-0044 makes a container boundary the bash tool's security boundary in every
profile, and the released image is distroless: no shell, no docker CLI, no container runtime.

**`none`** — the default, and the safe one. The bash tool reports itself unavailable and says so
at startup. Nothing extra is rendered. With no runtime to provide a boundary, unavailable is the
correct answer, not a defect.

**`docker-socket`** — mounts the node's `/var/run/docker.sock` (hostPath) into the pod. The chart
refuses to render unless you also set `sandbox.dockerSocket.acknowledgeHostRoot=true`. That flag
is not a safety mechanism and changes nothing about the risk; it is a forcing function, so the
tradeoff is accepted by a human in a reviewable file rather than inherited from a default.
ADR-0044, verbatim:

> Docker-in-Docker and mounted-socket setups carry a privilege-escalation tradeoff — socket
> access ≈ host root — that any implementation must address rather than assume away.

Concretely: a model-authored bash command reaching that socket can start a privileged container
and own the node — and from the node, every other workload scheduled on it, including its
Secrets. Put this on a dedicated, tainted node pool on a cluster you are willing to lose, and
schedule nothing else there.

**`kubernetes`** — **not yet available.** The guard refuses unconditionally, before any manifest
is consulted, naming the reason: the feature requires change #75, which ships the pod-per-sandbox
handler and its RBAC. The released image contains no such handler, so rendering RBAC for it would
grant pod-create rights to a server that cannot use them. There is no Kubernetes-sandbox RBAC to
configure in this chart version; do not hand-set `sandbox.kubernetes.minVersion` hoping to unlock
it.

## Scaling, rolling updates, and the mid-turn caveat

Replicas are stateless in the sense that matters: loops live in Postgres, and ownership is a
lease (ADR-0034). Scale with `replicaCount`, or `autoscaling.enabled=true` for an HPA;
`podDisruptionBudget` and `topologySpreadConstraints` are there too.

The Deployment strategy is `maxUnavailable: 0`, `maxSurge: 1`. A surge pod must reach **Ready**
— meaning `/readyz` returned 200, meaning the durable store answered — before an old pod is
removed, so a rollout that cannot reach Ready **stalls with the old replicas still serving**
instead of emptying the Service.

On `SIGTERM` (or `SIGINT`), in order:

1. readiness flips to **draining** first, so the endpoint leaves the Service before anything is
   refused;
2. `srv.Shutdown` lets in-flight unary calls finish, bounded by `--drain-timeout` (default
   `20s`, `server.drainTimeout`);
3. an unconditional `Close`.

`terminationGracePeriodSeconds` is derived as `drainTimeout + 10` (30s by default), so the
kubelet's SIGKILL always lands *after* the drain budget, never inside it. `server.drainTimeout`
must be whole seconds (`"20s"`, `"90s"`) — the chart fails closed on anything else, because that
arithmetic strips the `s` and has no duration parser behind it.

**What this does not promise.** A turn that was mid-flight on the dying pod is **not** resumed.
The loop's *events* are durable; its *in-memory execution* is not. The client sees a gap marker
and a parked loop — exactly what it sees after a crash — and the loop is re-owned once its
ownership lease expires. Observe streams are cut by design (ADR-0033): clients reattach by
subscribing before replaying, deduplicating at the watermark, and reading the gap markers. Plan
rollouts accordingly; this is a correctness-preserving interruption, not a transparent one.

## Ingress and h2c

The server speaks **h2c** (cleartext HTTP/2) and terminates no TLS.

- **Connect over HTTP/1.1 works through any ingress controller** — a connect-go or connect-es
  client is fine as installed.
- **A gRPC or gRPC-web client is not.** gRPC needs HTTP/2 end to end, and most controllers
  default to an HTTP/1.1 upstream, which downgrades the backend hop and makes those clients fail
  in ways that look like a broken server. Set the controller's h2c/GRPC backend annotation (on
  ingress-nginx: `nginx.ingress.kubernetes.io/backend-protocol: "GRPC"`). That makes the whole
  path gRPC, so a mixed deployment wants two Ingress objects on two hosts.

Leaving `ingress.tls` empty sends bearer tokens (ADR-0034) across the internet in cleartext to
that hop. Add a TLS block.

## Debugging: the image has no shell

The image is `gcr.io/distroless/static-debian12:nonroot` (UID 65532). There is **no shell, no
curl, no `ps`** — `kubectl exec ... -- sh` will not work, and neither will any other exec. This
is not something to work around; it is the attack surface you are paying for. Debug with the
endpoints and the logs:

```sh
kubectl -n <ns> logs deploy/fuse -f
kubectl -n <ns> port-forward svc/fuse 8787:8787
curl -s -o /dev/null -w '%{http_code}\n' localhost:8787/readyz
```

A 503 from `/readyz` tells you the pod is not ready and deliberately nothing more. The reason is
in the logs. In compose, the container's own `fuse healthcheck` is the equivalent probe.

## Validating a change without a cluster

```sh
docker compose -f deploy/compose/docker-compose.yml config
docker compose -f deploy/compose/docker-compose.yml --profile docker-socket config
helm template fuse deploy/charts/fuse --set auth.allowDevToken=true --set postgres.dev.enabled=true
```

`make test` gates the `docker compose config` render and a `helm lint` + `helm template` matrix
([`deploy/charts/validate_test.go`](../deploy/charts/validate_test.go), which skips loudly when
`helm` is absent).

Two **operator-only** smokes stand up real infrastructure and are not part of any CI run. Each
prints `SKIP:` rather than passing when its tooling is missing:

```sh
make compose-smoke   # docker + compose: up, wait on /readyz, scrape /metrics, one loop.start, down
make helm-smoke      # helm + kind + kubectl: create cluster, install, wait Ready, same check, delete
```

## See also

- [`deploy/compose/README.md`](../deploy/compose/README.md) — the compose stack in full
- [`deploy/charts/fuse/values.yaml`](../deploy/charts/fuse/values.yaml) — every chart value, with
  its reasoning
- [`observability.md`](observability.md) — metrics, traces, logs, and the cardinality budgets
- [`observability-local.md`](observability-local.md) — the standalone local observability stack
- [`releasing.md`](releasing.md) — how the image and the chart get published
