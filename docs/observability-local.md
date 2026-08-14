# Running fuse locally with observability

A step-by-step walkthrough for seeing traces and metrics from a **local** `fuse`
run — `fuse shell` or one-shot `fuse <task>` — in Grafana, using the dev
stack under `deploy/observability`.

This is development-only. The listeners below are unauthenticated and loopback-scoped;
never expose them as a production deployment. See [observability.md](observability.md)
for the operational/production guidance and the metric-label rules.

## 1. Start the dev stack

```bash
docker compose -f deploy/observability/docker-compose.yml up -d
```

This brings up four containers and publishes these **host** ports:

| Service    | In-container | Host port | Notes |
|------------|--------------|-----------|-------|
| Prometheus | 9090         | **9091**  | UI on 9091; it scrapes `host.docker.internal:9090` |
| Grafana    | 3000         | 3000      | dashboards + Explore |
| Tempo      | 3200         | 3200      | trace store + query API |
| Collector  | 4317 / 4318  | 4317/4318 | OTLP gRPC / HTTP receivers |

The single most important consequence: **Prometheus scrapes `host.docker.internal:9090`**,
so `fuse` must expose its metrics endpoint on **host port 9090**. (Prometheus' own UI
is deliberately remapped to 9091 to leave 9090 free for exactly this.) Traces go to the
collector on **4317** (gRPC).

## 2. Configure fuse

Add the observability block to `~/.fuse/config.yml`. This is the known-good minimum —
every key here is load-bearing; omitting one makes fuse refuse to start with a precise
error (fail-fast is intentional):

```yaml
observability:
  instance_id: "local"
  metrics:
    enabled: true
    bind: "127.0.0.1:9090"      # matches Prometheus' scrape target
    path: "/metrics"
    access: public               # loopback only
  traces:
    enabled: true
    endpoint: "127.0.0.1:4317"   # collector OTLP gRPC
    protocol: grpc
    insecure: true
    sample_ratio: 1.0            # sample everything while testing
  logging:
    enabled: true
    output: stdout
    level: info
    max_override_ttl: "1h"
  cardinality:
    hash_version: "sha256-64-v1"
    salt: "local"
    tenant: { budget: 50 }
    model:  { budget: 50 }
    tool:   { budget: 100 }
```

If port 9090 is already taken on your host, fuse warns and runs anyway — traces still
flow, but nothing scrapes metrics. Check with `lsof -i :9090`.

## 3. Run a turn

Use a **cheap gateway model**, never a Claude model, for local traffic. One-shot needs
the model named explicitly:

```bash
fuse --model glm "Read cmd/fuse/shell.go with your tools and summarize runShell in one sentence."
```

Or interactively — `fuse shell --model glm` — and issue a turn that makes a tool call
or a spawn.

## 4. See it in Grafana

Open **http://localhost:3000**.

- **Prebuilt dashboard** — the "Fuse Loop Observability" dashboard is provisioned at
  **http://localhost:3000/d/fuse-loop** (throughput, outcomes, p95 latency, active
  loops, retries/timeouts, tool/spawn failures, cardinality health). Metrics appear
  after Prometheus completes a scrape (one scrape interval).
- **Traces** — Explore → **Tempo** datasource → Search, service `fuse`. Each run is a
  root `fuse.loop.run` span with the model attempts and tool/spawn calls nested under
  it, e.g.:

  ```
  fuse.loop.run
  ├─ fuse.model.attempt.complete
  ├─ fuse.tool.execute
  └─ fuse.spawn ...
  ```

You can also query the raw APIs directly:

```bash
# metrics (scrape the endpoint while a run is in flight)
curl -s localhost:9090/metrics | grep fuse_

# traces (durable in Tempo, queryable after the run)
curl -s "http://localhost:3200/api/search?tags=service.name%3Dfuse&limit=10"
```

## Notes

- Local runs have no authenticated tenant, so metrics attribute to
  `tenant_id="__overflow__"`. That is expected, not a misconfiguration.
- One-shot `fuse <task>` runs are short; the `_total` counters barely accumulate before
  the process exits. **Traces are the durable signal** for a one-shot — they are pushed
  to the collector during the run and persist in Tempo. `fuse shell` is long-lived, so
  its metrics accumulate across the session and scrape normally.
- Tear down the stack with
  `docker compose -f deploy/observability/docker-compose.yml down`.
