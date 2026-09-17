<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0076 — fuse server deployment — a docker-compose stack and a Helm chart over the released image (not an operator)](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0076-fuse-server-helm-chart-compose-stack.md)**
<!-- docket:backlink:end -->

# fuse server deployment — compose stack + Helm chart — implementation plan

Change #76 · spec: `docs/superpowers/specs/2026-09-13-fuse-server-helm-chart-compose-stack-design.md` (on `docket`)
Branch: `feat/fuse-server-helm-chart-compose-stack` · base: `origin/main` @ b966b5e

> **Plan-role degradation.** `skills.plan` resolves to `superpowers:writing-plans`, which is not
> installed on this machine. Per the convention's missing-skill rule the role degraded to `auto` and
> this plan was authored by the implementer directly.

## What the reconcile settled

Every one of the spec's six open build questions was resolved against `origin/main` @ b966b5e and
recorded in the change's `## Reconcile log`. The five that change what gets built:

1. **`fuse version`'s backend line goes on its own THIRD line.** `TestVersionSubcommand`
   (`cmd/fuse/version_test.go:26-28`) asserts `lines[0] == "fuse "+version.Version` by **exact
   equality**. The spec's `fuse 0.3.0 (backends: fsstore,pgstore)` would break it; an appended line
   is already tolerated because the Go/platform line is only `strings.Contains`-checked.
2. **Add a `fuse healthcheck` subcommand.** The distroless image has no curl, so this is the only
   way compose gets a real healthcheck — and it hands Kubernetes an `exec`-probe option for free.
3. **A `/tmp` emptyDir is REQUIRED, not optional.** `internal/tools/sandbox/egress_proxy.go:407`
   calls `os.MkdirTemp("", "fuse-egress")` on the serve path whenever the bash tool is enabled, and
   the error is swallowed by `warnEgressBlackout` (`cmd/fuse/sandbox.go:479-484`): without a
   writable `/tmp`, `egress.mode: enforce` **silently degrades to deny-all** while the pod stays
   Ready. This is asserted in the chart tests, not just documented.
4. **The compose stack gets its OWN `prometheus.yml`.** `hasFuseScrapeTarget` in
   `deploy/observability/validate.go` asserts that file's exact
   `host.docker.internal:9090` triple, so retargeting it would fail `make test`. A new file mounted
   over the same container path leaves the standalone stack green.
5. **The validator is NOT generalisable by a `root` parameter.** `validate(root)` demands the full
   four-sidecar shape (prometheus + grafana + otel-collector + tempo, two provisioned datasources,
   two dashboard JSONs) that the compose stack does not own. A second, narrower entry point in the
   same `main` package reuses `registeredMetrics` / `requireRegisteredMetrics` / `readYAML`.

And two placement corrections: `release-dry-run` lives in `.github/workflows/integration.yml`
(not `release.yml`), and `observability-validate` is `go run` on a **single file**, so a new `.go`
file in that package is not compiled by that target unless the target is widened.

## Deferred by construction

`sandbox.mode: kubernetes` keeps `sandbox.kubernetes.minVersion: ""` in this change, so its guard
**refuses to render before any manifest is consulted**. `deploy/k8s/sandbox-rbac.yaml` does not
exist on `origin/main` — it is #75's deliverable — and this change needs no part of it. #75 supplies
both the manifest and the pinned `minVersion` when it lands. Task 10 tests only the refusal.

## Tooling note (test evidence honesty)

`helm`, `kind`, `docker`, and `kubectl` are all present on this machine and the docker daemon is up,
so the `helm lint` / `helm template` matrix and `docker compose config` run for real. The **kind**
and **compose bring-up** smokes stay operator-only per the spec and are NOT run by this build; they
follow `observability-compose-smoke`'s loud-SKIP shape (`Makefile:150`) rather than green-passing
when their tooling is absent.

---

## Task 1 — `event.Pinger` and the two store implementations

**Files:** `internal/event/store.go`, `internal/event/pgstore/store.go`,
`internal/event/fsstore/durable.go`, plus focused tests.

`/readyz` needs a cheap liveness probe on the durable store, and `internal/event` exports no such
method today. `CommittedDurableStore` (`store.go:78`) is the established optional-interface
precedent — a consumer type-asserts and degrades when absent — so `Pinger` follows it exactly:

```go
// Pinger is the OPTIONAL cheap-liveness seam a readiness probe asserts for.
type Pinger interface{ Ping(ctx context.Context) error }
```

- `pgstore.PGStore.Ping` → `s.pool.Ping(ctx)`. The `pool` field is unexported with no accessor, so
  the method must live in that package.
- `fsstore.FSDurableStore.Ping` → `os.Stat(s.baseDir)`, error if it is not a directory. Same
  reason: `baseDir` is unexported.

**Tests:** each store's `Ping` returns nil when healthy; fsstore's returns an error when `baseDir`
is removed. A store that does **not** implement `Pinger` must be treated as ready — that is asserted
in Task 3 where the assertion actually lives, not here.

**Do not** add `Ping` to `DurableStore`. Widening the required interface would force every test
double in the tree to grow a method, which is exactly what the optional-interface pattern exists to
avoid.

## Task 2 — `fuse version` reports the backend set

**Files:** `cmd/fuse/backends.go` (new, untagged), `cmd/fuse/backends_pg.go` (new, `pgstore`),
`cmd/fuse/main.go`, `cmd/fuse/version_test.go`.

Two mutually-exclusive one-line files mirroring the `durable_backend*.go` pair:

- `backends.go` (`//go:build !pgstore`): `const durableBackends = "fsstore"`
- `backends_pg.go` (`//go:build pgstore`): `const durableBackends = "fsstore,pgstore"`

`main.go`'s version handler gains a **third** `Fprintf`: `backends: %s\n`.

**Tests:** extend `version_test.go` to assert the output contains `backends: ` and that line 1 is
still byte-exactly `fuse <version>`. The new assertion must pass in **both** build modes, so assert
`strings.Contains(s, "backends: fsstore")` (a prefix true of both) rather than the full list.
Re-run `TestVersionFlagAliases` and `TestVersionPrecedesConfigLoad` — both must stay green.

## Task 3 — `/healthz`, `/readyz`, and the readiness gate

**Files:** `cmd/fuse/health.go` (new), `cmd/fuse/loop_serve_net.go`, `cmd/fuse/health_test.go` (new).

A small `readiness` type owning the three conditions the spec names, plus the 2s probe cache:

```go
type readiness struct {
    store    event.DurableStore // may or may not implement event.Pinger
    verifier loopauth.Verifier
    draining atomic.Bool
    mu       sync.Mutex
    lastAt   time.Time
    lastErr  error
}
```

- `/healthz` → always `200 ok` once `Serve` is running. Liveness only: the process answers HTTP.
- `/readyz` → `200 ok` when the store probe succeeds **and** `verifier != nil` **and**
  `!draining`. Otherwise `503` with an **empty body** — the reason goes to the log, never the
  response. A store not implementing `Pinger` counts as ready.
- The probe result (success or failure) is cached 2s so a probe storm cannot become a Postgres
  storm.

Both routes are registered on the mux in `serveNetObserved` **before** the Connect handler. They
need no auth exemption: the auth interceptor is a `connect.HandlerOption` on the Connect handler
only (`loop_serve_net.go:339`), not mux middleware, so a plain `mux.Handle` is unauthenticated by
construction.

**Tests:** `/healthz` is 200; `/readyz` is 200 with a healthy pinger; 503 with a failing pinger;
200 with a store that implements no `Pinger`; 503 once draining is set; the response body is empty
on 503; a second probe inside 2s does not re-invoke the pinger (count the calls); neither route
requires a bearer token while the Connect path still rejects one without it.

## Task 4 — SIGTERM and the bounded graceful drain

**Files:** `cmd/fuse/loop_serve_net.go`, `cmd/fuse/loop_serve_net_drain_test.go` (new).

1. `serveNetContext` becomes
   `signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)`.
2. A `--drain-timeout` flag (default `20s`) on the `loop-serve-net` flag set.
3. The shutdown goroutine in `serveNetObserved` changes from a bare `srv.Close()` to:
   flip readiness to draining → `srv.Shutdown(drainCtx)` with the timeout → `srv.Close()`
   unconditionally after, so a stuck stream cannot outlive the budget.

Observe streams are cut by design (ADR-0033: a dropped stream is normal, clients reattach), and
`Shutdown` does not wait for hijacked or streaming connections anyway — which is why the
unconditional `Close` follows rather than replaces it.

**Tests:** readiness reports 503 **before** the listener stops accepting (order is the
load-bearing property — a reversed order makes the drain useless because the LB keeps sending);
an in-flight unary call started before shutdown completes successfully; `Serve` returns nil on a
ctx-driven shutdown (not an error); the drain is bounded — a handler that blocks past the timeout
does not hang the shutdown. Drive these through the existing seam (a `127.0.0.1:0` listener +
ctx cancel), never a real signal.

## Task 5 — `FUSE_INSTANCE_ID` env fallback

**Files:** `cmd/fuse/observability.go` (or wherever the instance id is resolved into the service),
`cmd/fuse/observability_test.go`.

Resolution order: `config.Observability.InstanceID` (non-empty) → `$FUSE_INSTANCE_ID` → `os.Hostname()`
→ `""`. The **trusted config file still wins** — the env is consulted only when the file leaves the
field empty (ADR-0006: `observability` is honored from the trusted home file and nowhere else, and
this does not change that; it fills a gap the file cannot fill, since one Secret shared by N
replicas cannot carry N ids).

Resolve it **once** at service construction and store the result, so the three existing consumers
(`observability.go:153` log identity, `:173` trace resource, `:397` admin response) all see the same
value without three lookups.

**Tests:** config value wins over a set env; env is used when config is empty; hostname is the last
resort; all three consumers report the same resolved value.

## Task 6 — `fuse healthcheck` subcommand

**Files:** `cmd/fuse/healthcheck.go` (new), `cmd/fuse/main.go`, `cmd/fuse/healthcheck_test.go` (new).

`fuse healthcheck [--addr host:port] [--path /readyz] [--timeout 2s]` → GETs the URL, exits **0** on
2xx and **1** otherwise, printing nothing on success. Like `version`, it must be dispatched
**before** `config.Load()`: a container healthcheck has to work on a server whose config is broken,
which is exactly when you most want to know the container is unhealthy.

**Tests:** exit 0 against a 200 stub; exit 1 against a 503; exit 1 on a connection refusal; exit 1
on timeout; works with a deliberately broken `~/.fuse/config.yml` (the same regression shape as
`TestVersionPrecedesConfigLoad`).

## Task 7 — `-tags pgstore` on every release build

**Files:** `.goreleaser.yaml`, `Makefile`.

- `.goreleaser.yaml` build `id: fuse` gains `tags: [pgstore]`.
- The Makefile's `build` and `install` gain `-tags pgstore`.

The untagged path stays available for anyone who wants it (`go build` with no tag), and no test
enforces the "cmd/fuse must be pgx-free" comment in `durable_backend.go`'s header, so nothing
breaks. With no DSN the tagged selector still falls through to fsstore, so CLI behaviour is
unchanged — the cost is pgx in the binary, which the spec accepted as the human's call.

**Per the `declarative-config-validator-accepts-typos` learning** (from change 0082, same file):
`goreleaser check` passing does **not** prove `tags:` is a real key on a v2 build. Verify the key
against GoReleaser v2's own build schema and then prove it empirically: `make build && ./fuse version`
must print `backends: fsstore,pgstore`. That assertion — not the linter — is the evidence, and it is
exactly why Task 2 exists before this one.

## Task 8 — the compose dev stack

**Files (all new):** `deploy/compose/docker-compose.yml`, `deploy/compose/fuse.compose.yml`,
`deploy/compose/prometheus.yml`, `deploy/compose/README.md`.

- `docker-compose.yml`: `include: [../observability/docker-compose.yml]` (Compose v2.20+; the local
  toolchain is v5.1.2), adds `fuse` + `postgres`, and overrides the `prometheus` service's config
  mount to point at `deploy/compose/prometheus.yml`.
- `fuse`: `image: ${FUSE_IMAGE:-ghcr.io/ethanhinson/fuse:latest}`, command
  `loop-serve-net --addr 0.0.0.0:8787`; `FUSE_PG_DSN` at the `postgres` service;
  `FUSE_INSTANCE_ID=compose-1`; a named volume at `/home/nonroot/.fuse`; `./fuse.compose.yml` at
  `/home/nonroot/.fuse/config.yml:ro`; **a writable `/tmp`** (Task 3's finding applies to compose
  too); ports `127.0.0.1:8787:8787` and `127.0.0.1:9090:9090` (loopback publish, ADR-0043);
  `depends_on: postgres: condition: service_healthy`; `healthcheck` shelling out to
  `["/fuse","healthcheck"]` from Task 6.
- `postgres`: `postgres:16-alpine`, named volume, `pg_isready` healthcheck, credentials inline
  **because this stack is dev-only**, stated in the header.
- `fuse.compose.yml`: `loop_server.auth` with the documented dev token + `_default` tenant;
  `observability` with `metrics.bind: 0.0.0.0:9090`, `access: public`, traces to
  `otel-collector:4317`, stdout logging, the required cardinality block.
- **Header, on every file that carries a credential:** a multi-line `#` block in the
  `.goreleaser.yaml:1-17` prologue style saying every credential here is a checked-in placeholder
  and this stack is not a production deployment. The observability compose file's own one-liner is
  the minimum bar, not the model, because that file carries no credentials at all.
- `docker-socket` profile adds `/var/run/docker.sock:/var/run/docker.sock` to `fuse`, with a comment
  block quoting ADR-0044's ≈-host-root tradeoff. Without the profile bash reports unavailable.

**`prometheus.yml`:** `job_name: fuse`, `metrics_path: /metrics`, target `fuse:9090`, and it must
load the inherited `/etc/prometheus/alerts.yml`.

## Task 9 — compose + chart validation, wired into `make test`

**Files:** `deploy/observability/validate.go`, `deploy/observability/validate_test.go`, `Makefile`.

Add a second entry point in the same `main` package — `validateCompose(root, wantTarget string)` —
reusing `registeredMetrics`, `requireRegisteredMetrics`, and `readYAML`:

- every `fuse_*` identifier in the compose stack's `prometheus.yml` is a registered series;
- its `fuse` job targets `wantTarget` (`fuse:9090`) with `metrics_path: /metrics`;
- it loads the mounted alerts file.

Refactor `hasFuseScrapeTarget` to take the expected target as a **parameter** rather than adding a
second hard-coded literal, and call it with `host.docker.internal:9090` from the existing
`validate()` so that path is byte-for-byte unchanged in behaviour.

`main()` calls both roots and fails on either.

**Makefile:** `observability-validate` becomes `go run ./deploy/observability` (package form) so
every file in the package compiles — the current single-file `go run ./deploy/observability/validate.go`
would silently not compile the new code, and the check would never run in the gate it was added to.
Confirm the package form still works given `validate_test.go` is in the same directory (test files
are excluded from `go run` by the build system, so it does).

**Tests** in `validate_test.go`, matching the existing negative-test style (mutate a copied
artifact, assert the error): a compose `prometheus.yml` naming an unregistered metric is rejected;
one targeting `host.docker.internal:9090` instead of `fuse:9090` is rejected; the real compose
artifacts pass. Keep `TestValidateAcceptsReferenceArtifacts` green.

## Task 10 — the Helm chart

**Files (all new):** `deploy/charts/fuse/Chart.yaml`, `values.yaml`, `.helmignore`,
`templates/_helpers.tpl`, `templates/NOTES.txt`, and the templates from the spec's table:
`deployment.yaml`, `service.yaml`, `serviceaccount.yaml`, `hpa.yaml`, `pdb.yaml`,
`secret-config.yaml`, `secret-dsn.yaml`, `postgres-dev.yaml`, `servicemonitor.yaml`,
`prometheusrule.yaml`, `networkpolicy.yaml`, `ingress.yaml`, `sandbox.yaml`.

`Chart.yaml`: `apiVersion: v2`, `version`/`appVersion` `0.0.0-dev` in-tree, stamped at package time.

Build it in the spec's table order. The load-bearing invariants, each of which Task 11 asserts:

- **Auth guard.** `fail` unless `auth.tokens` is non-empty, `config.existingSecret` is set, or
  `auth.allowDevToken: true`.
- **DSN required.** `fail` unless `postgres.dsn`, `postgres.existingSecret`, or
  `postgres.dev.enabled` is set. (The spec says external DSN required *plus* a dev StatefulSet
  behind a flag — the flag is the third way to satisfy the same guard.)
- **Every pod** carries liveness `/healthz`, readiness + startup `/readyz`, `runAsNonRoot`,
  `readOnlyRootFilesystem: true`, `capabilities.drop: [ALL]`, the config checksum annotation, an
  emptyDir at `/home/nonroot/.fuse`, **an emptyDir at `/tmp`**, and
  `terminationGracePeriodSeconds = drainTimeout + 10`.
- **`sandbox.mode` tri-state.** `none` renders nothing; `docker-socket` `fail`s unless
  `sandbox.dockerSocket.acknowledgeHostRoot: true`; `kubernetes` `fail`s **always** in this change
  because `sandbox.kubernetes.minVersion` is `""` — the message must say "not yet available
  (requires change #75)", never a version-comparison error, so an operator reads the real reason.
- **`prometheusrule.yaml`** carries the groups from `deploy/observability/alerts.yml` verbatim.
- **ServiceMonitor bearer token** is an explicit `metrics.serviceMonitor.bearerSecret` reference.
  Never auto-mint a scrape principal into `auth.tokens`: a template that synthesizes a credential
  hides a live token from the operator who owns the auth surface.

`NOTES.txt` states the rolling-update semantics (`maxUnavailable: 0` / `maxSurge: 1`, Ready gated on
`/readyz`), the ADR-0044 warning when `docker-socket` is active, and the h2c/ingress caveat.

## Task 11 — chart tests (`helm lint` + `helm template` matrix)

**Files:** `deploy/charts/validate_test.go` (new).

Go-driven, **skip-clean when `helm` is absent** (`exec.LookPath`, `t.Skip` with a loud message —
the `observability-compose-smoke` posture, so a missing tool never green-passes as tested).

`helm lint` on the chart, then `helm template` across the spec's matrix: default (with a DSN),
`auth.allowDevToken=true`, `sandbox.mode=docker-socket` **with and without** the acknowledgement,
`sandbox.mode=kubernetes`, `postgres.dev.enabled=true`, `metrics.serviceMonitor.enabled=true`.

Assertions:
- the auth guard fires with no auth and no `allowDevToken`;
- the DSN guard fires with no DSN and no dev Postgres;
- `docker-socket` without the acknowledgement **fails**; with it, renders the hostPath mount;
- `kubernetes` fails with a message naming #75;
- **every** rendered Pod spec has both probes, `readOnlyRootFilesystem: true`, the config checksum
  annotation, and a `/tmp` mount — walk the rendered YAML rather than grepping for a string, so a
  Pod added later cannot slip through;
- the rendered PrometheusRule's groups **equal** `deploy/observability/alerts.yml`'s (parse both,
  compare structurally — a string compare would break on indentation).

## Task 12 — publishing

**Files:** `.github/workflows/release.yml`, `.github/workflows/integration.yml`.

- `release.yml`, in the existing `release` job after the image attestation (the GHCR login at step 5
  and `packages: write` already cover an OCI push):
  `helm package deploy/charts/fuse --version ${TAG#v} --app-version ${TAG#v}` then
  `helm push fuse-${TAG#v}.tgz oci://ghcr.io/ethanhinson/charts`.
  Pre-release tags publish a pre-release chart version; **no floating chart tag**, same rule as the
  image's `:latest`.
- `integration.yml`'s **`release-dry-run`** job (this is the file that job lives in) gains, after
  the snapshot build: `helm lint`, `helm template` with a minimal valid values set,
  `helm package --version 0.0.0-dev` (no push), and
  `docker compose -f deploy/compose/docker-compose.yml config`. Follow that job's documented
  **hard-fail, never green-skip** posture — it exists precisely so a broken release config is found
  on the PR and not at tag time.

## Task 13 — operator-only smokes

**Files:** `Makefile`.

Two targets modelled on `observability-compose-smoke` (`Makefile:150`) — outside CI, reporting a
loud `SKIP:` when tooling is absent rather than pretending the external system was tested:

- `compose-smoke`: requires docker + compose; `docker compose ... config`, brings the stack up,
  waits on `/readyz`, scrapes `/metrics`, runs one `loop.start` over the Go SDK, tears down.
- `helm-smoke`: requires helm + kind + kubectl; creates a cluster, installs with
  `postgres.dev.enabled=true`, waits for Ready, runs the same readiness + `loop.start` check,
  deletes the cluster.

Add both to `.PHONY`. **Neither runs in this build** — they are operator-only by design, and the
results file says so plainly rather than implying they passed.

## Task 14 — `docs/deploying.md`

**Files:** `docs/deploying.md` (new), `README.md` (a link, and the `go install` + `-tags pgstore`
note from the spec's §1a).

Compose quickstart; Helm quickstart; the config-as-Secret model and which keys are credential
surfaces; the bash-runtime tri-state and what each option costs (quoting ADR-0044); scaling and
rolling-update semantics **including the honest mid-turn caveat** — a loop's events are durable but
its in-memory execution is not, so a client sees a gap marker and a parked loop exactly as after a
crash; ingress and h2c; and the image-has-no-shell facts (no `kubectl exec` debugging — use the
endpoints and logs).

## Final gate

`make test` (which runs `observability-validate` then `go test ./...`) must be green, plus
`make build && ./fuse version` showing `backends: fsstore,pgstore` as Task 7's real evidence.
Record which suites ran and which smokes were deliberately not run in the results file.
