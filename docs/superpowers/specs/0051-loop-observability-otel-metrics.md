<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0051 — Observability for the loop — OTEL traces + Prometheus metrics + Grafana + structured logs](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0051-loop-observability-otel-metrics.md)**
<!-- docket:backlink:end -->

# Loop Observability: OpenTelemetry, Prometheus, Grafana, and Structured Logs

## Design Summary

Fuse observability is a projection over the canonical, typed, durable per-loop event stream. The event stream remains the source of truth for loop activity and full-fidelity replay. Observability consumers derive bounded operational metrics, structured event summaries, and trace correlation without copying prompts, responses, tool arguments, or tool results into routine telemetry.

The initial implementation runs in process and separates provider-neutral instrumentation contracts from concrete adapters:

- The runtime emits canonical events and invokes only minimal provider-neutral timing/span hooks where an event cannot represent a required timing.
- An in-process projector converts committed events into metrics and structured log records.
- A Prometheus adapter exposes process-local metrics through `/metrics`.
- An OpenTelemetry adapter creates and exports nested spans through OTLP.
- A structured JSON logger writes to stdout by default or to an optional concurrency-safe file sink.
- Authenticated administration controls live log levels and file reopening.
- A reference Docker Compose environment provides Prometheus, Grafana, an OpenTelemetry Collector, and Grafana Tempo.
- Loki remains an optional external consumer of stdout or file logs and is neither shipped to directly nor hosted by Fuse.

Runtime packages must not import OpenTelemetry, Prometheus, Grafana, Tempo, or Loki libraries. Concrete imports are confined to adapters, bindings, and application composition.

## Architecture

```text
SDK / external caller
    │ W3C trace context
    ▼
Authenticated API binding
    │ context.Context
    ▼
Loop runtime ───────── provider-neutral operation hooks ────────┐
    │                                                           │
    │ committed canonical events                                ▼
    ▼                                                   OpenTelemetry adapter
Durable event store                                             │
    │                                                           ▼
    ├── authenticated replay API / SDK                    OTLP Collector
    │                                                           │
    ▼                                                           ▼
In-process observability projector                            Tempo
    │
    ├── Prometheus recorder ── /metrics ── Prometheus ── Grafana
    │
    └── structured logger ── stdout or file ── external collector
                                                   │
                                                   └── optional Loki
```

### Package Boundaries

1. **Runtime contracts** own canonical event production and provider-neutral observation context and operation hooks. They contain no vendor SDK imports and no sampling, export, retention, label, or backend policy.
2. **Projection layer** consumes the same committed event representation used by production subscribers, derives bounded metric observations and payload-free logs, and keeps only process-local state required for deduplication and active-operation gauges.
3. **Adapters** implement Prometheus recording and HTTP exposure, OpenTelemetry spans and OTLP export, and structured JSON stdout/file sinks.
4. **Composition and bindings** instantiate adapters, register `/metrics` and authenticated administration, install transport trace propagation, and own startup and shutdown of projectors, exporters, expiration workers, and sinks.

## Provider-Neutral Interfaces

Precise package names may follow the repository layout, but dependency direction and capabilities must remain equivalent to these contracts:

```go
type OperationKind string

const (
	OperationLoop   OperationKind = "loop"
	OperationModel  OperationKind = "model"
	OperationTool   OperationKind = "tool"
	OperationSpawn  OperationKind = "spawn"
	OperationStore  OperationKind = "store"
	OperationPubSub OperationKind = "pubsub"
)

type OperationDescriptor struct {
	Kind          OperationKind
	TenantID      string
	LoopID        string
	NodeID        string
	EventSequence uint64

	ModelProvider string
	ToolClass     string
	Attempt       int
}

type OperationObserver interface {
	Start(context.Context, OperationDescriptor) (context.Context, OperationHandle)
}

type OperationHandle interface {
	Event(name string, fields ...Field)
	RecordError(error)
	End(Outcome)
}

type EventProjection interface {
	Project(context.Context, CanonicalEvent) error
}
```

`OperationObserver` is optional and always has a no-op implementation. Public fields and interfaces use repository-owned types; they never expose OpenTelemetry spans, Prometheus collectors, backend clients, or backend-specific attributes.

Projection receives events only after durable append succeeds. Failed appends may be observed by inline hooks but must not appear as committed-event metrics or logs. Adapters are independently replaceable and composable.

## Data Flows

### Synchronous Execution

1. The API binding authenticates the caller and resolves tenant scope.
2. It extracts validated W3C trace context into `context.Context`.
3. The runtime starts a provider-neutral loop operation.
4. Model, tool, spawn, store, and publish operations derive child contexts.
5. A canonical event is durably appended.
6. Only after success is the event delivered to the projector.
7. The projector updates metrics and emits a payload-free structured log. Projection failure is self-observed without failing the loop.
8. Full payloads remain available only through authenticated replay.

### Cross-Process and Asynchronous Work

W3C `traceparent` and `tracestate` are injected into repository-owned transport metadata across process, store, and pub/sub boundaries. Arbitrary baggage is not persisted or forwarded by default. Tenant identity always comes from authenticated Fuse context, never trace headers or baggage.

Immediately consumed messages continue causal context using OpenTelemetry messaging conventions. Durable replay or substantially delayed work begins a new trace with a span link to stored context instead of making a historical span the active parent. Malformed or untrusted trace context is ignored without rejecting the application request.

### Projection Delivery and Deduplication

Projection identity is `(tenant_id, loop_id, sequence)`. Duplicate committed events within a process lifetime must not increment counters or emit routine logs twice.

A replay-plus-live path subscribes first, captures the replay watermark, replays through it, then deduplicates buffered and live events by the full identity. Tenant is reasserted on every cache or deduplication hit. Fanout snapshots subscriber IDs and revalidates membership before sending to avoid send-on-closed races.

Projection failure is isolated, counted, and rate-limited through a fallback sink. It never causes runtime event retries.

## Metric Catalog

All names use the `fuse_` prefix. Counters end in `_total`; durations use seconds.

| Metric | Type | Labels | Source |
|---|---|---|---|
| `fuse_loop_operations_total` | Counter | `tenant_id`, `operation`, `outcome` | Loop lifecycle |
| `fuse_loop_operation_duration_seconds` | Histogram | `tenant_id`, `operation`, `outcome` | Lifecycle or inline timing |
| `fuse_loop_active` | Gauge | `tenant_id`, `operation` | Start/terminal events |
| `fuse_model_calls_total` | Counter | `tenant_id`, `model`, `outcome` | Model events |
| `fuse_model_call_duration_seconds` | Histogram | `tenant_id`, `model`, `outcome` | Model timing |
| `fuse_model_call_attempts` | Histogram | `tenant_id`, `model`, `outcome` | Terminal attempt count |
| `fuse_tool_calls_total` | Counter | `tenant_id`, `tool`, `outcome` | Tool events |
| `fuse_tool_call_duration_seconds` | Histogram | `tenant_id`, `tool`, `outcome` | Tool timing |
| `fuse_spawn_operations_total` | Counter | `tenant_id`, `outcome` | Spawn lifecycle |
| `fuse_spawn_operation_duration_seconds` | Histogram | `tenant_id`, `outcome` | Spawn timing |
| `fuse_event_projection_total` | Counter | `event_kind`, `outcome` | Projection attempts |
| `fuse_event_projection_duration_seconds` | Histogram | `outcome` | Projection processing |
| `fuse_observability_export_errors_total` | Counter | `signal`, `reason` | Adapter/export failures |
| `fuse_observability_dropped_total` | Counter | `signal`, `reason` | Policy/queue/size drops |
| `fuse_observability_log_reopens_total` | Counter | `outcome` | File reopen operations |
| `fuse_observability_log_overrides` | Gauge | `scope` | Active live overrides |
| `fuse_metrics_label_overflow_total` | Counter | `dimension`, `metric` | Label values collapsed to overflow |
| `fuse_metrics_label_admitted_values` | Gauge | `dimension` | Individually admitted values |
| `fuse_metrics_label_budget` | Gauge | `dimension` | Configured admission budget |

Fixed outcomes are `success`, `error`, `timeout`, `cancelled`, `rejected`, and `other`. Operations, event kinds, signals, reasons, scopes, overflow dimensions, and metric identifiers are bounded repository enums. Stable opaque tenant IDs, model names, and tool names are permitted and enabled by default only on the curated metric families that declare them. Human-readable tenant names never become labels; Grafana may resolve stable IDs to display names through a separate metadata source.

### Cardinality Policy

`tenant_id`, `model`, and `tool` each have an explicit finite configurable budget under `observability.metrics.cardinality_budgets`. Budget enforcement is independent per dimension and occurs before label combinations are built. Each metric declares its exact label set; instrumentation never attaches all available dimensions automatically.

Admission is deterministic across replicas and restarts. Candidate values come from versioned tenant, model, and tool catalog snapshots. Configured pinned values are admitted first; configuration is invalid when pins exceed a budget. Remaining capacity is selected by ascending stable hash of `(dimension, value)` using a documented versioned algorithm and salt. Identical snapshots and configuration therefore produce identical admission sets regardless of request arrival order. Reconciliation happens only when a catalog snapshot or cardinality configuration changes—never by TTL, LRU, or request-path eviction. Unknown and non-admitted values map to the reserved `__overflow__` sentinel.

`fuse_metrics_label_overflow_total` increments once per overflowing dimension and observation without exposing the rejected value. Operators can compare admitted-value and configured-budget gauges and pin tenants that require guaranteed individual alerting. Removal or addition of one catalog value may replace at most one non-pinned admitted value for that dimension at the reconciliation boundary.

The worst-case application series count for every metric is documented as the product of each budgeted dimension plus its overflow value, multiplied by other bounded labels. Capacity estimates also include replicas and Prometheus-added target labels.

Prometheus labels still never contain loop or node IDs; event sequence, trace, or span IDs; payload content; error strings; URLs; file paths; human-readable tenant names; or undeclared/uncontrolled values. Registration and policy tests reject forbidden or undeclared labels.

Histogram buckets are startup configuration. Process-local counters reset on restart; the projector does not rebuild lifetime counters from durable history, and dashboards use restart-safe rate queries.

## Tracing

### Span Hierarchy

```text
external application span
└── fuse.api.request
    └── fuse.loop
        ├── fuse.store.append
        ├── fuse.model.call
        │   ├── fuse.model.attempt
        │   └── fuse.model.attempt
        ├── fuse.tool.call
        ├── fuse.spawn
        │   └── child loop or linked remote trace
        └── fuse.pubsub.publish
```

Every model call, including child calls, creates a bounded parent call span and one span per attempt. Attempts record configured timeout, retry ordinal, outcome, and elapsed time, and end on every success, error, timeout, cancellation, and early return. Tool, spawn, store, and pub/sub operations follow the same lifecycle rule. All timers, goroutines, queues, and exporters are torn down on every exit path.

### Attributes

First-class identity attributes are `fuse.tenant.id`, `fuse.loop.id`, `fuse.node.id`, and `fuse.event.sequence`. Bounded operational attributes include operation, outcome, model-provider category, tool class, retry attempt/max, and timeout.

Raw tenant, loop, and node IDs are allowed in protected traces as the drill-down mechanism. Only stable opaque tenant IDs may also enter curated, budget-controlled metric families; loop and node IDs never do. Raw error text is excluded by default because it may contain sensitive data.

### Sampling and Export

Sampling is composition-time OpenTelemetry SDK policy. OTLP to an OpenTelemetry Collector is the initial exporter; Fuse introduces no trace query API. Export has finite timeouts, bounded queues, explicit drop counters, deterministic flush/shutdown deadlines, and no unbounded retries. Exporter outages must not make loop execution unavailable.

### SDK Correlation

SDK operations initiating API calls accept `context.Context` and inject W3C trace context. External callers may send valid W3C headers. Responses expose the accepted server trace identifier through documented metadata for correlation, without exposing a telemetry query endpoint.

Tests cover SDK-to-API, API-to-loop, model attempts, tools, spawns, store, and pub/sub hops.

## Structured Logging

Routine logs are newline-delimited JSON containing timestamp, severity, stable event/message name, service and instance identity, trace/span IDs, tenant/loop/node IDs where scoped, event sequence, bounded operation/outcome, and payload-free error category.

Routine records never contain prompts, model responses, streaming deltas, tool arguments/results, authorization headers, trace headers, or raw secrets. Stdout is the default. Each record is serialized and written as one unit so concurrent writers cannot interleave JSON fragments.

### Optional File Sink

The file sink supports append, whole-record serialization, explicit flush/close, authenticated reopen, `SIGHUP`, atomic concurrency-safe replacement, bounded fallback error reporting, and shutdown drain deadlines.

Reopen opens and validates the replacement before changing state, stops admission to the old writer, drains in-flight writes, atomically swaps, closes the old descriptor, and resumes queued writes. Open failure leaves the prior sink valid. Documentation prescribes rename/create plus reopen; `copytruncate` is unsupported.

## Live Debug Controls

Effective level precedence is loop override, tenant override, current global level, then configured default. Tenant and loop overrides require an absolute expiry or bounded TTL. Loop overrides are keyed by tenant and loop.

Existing authenticated administration exposes operations to inspect global/scoped/effective state, set global level, create/replace/remove scoped overrides, reopen the file sink, and reload logging configuration. Scoped operations require authorization for the target tenant.

Every mutation emits a non-suppressible audit record containing actor, action, scope, previous/new value, expiry, timestamp, request trace/correlation ID, and outcome. Signal reloads identify the process and signal as actor. Invalid reloads leave the prior valid configuration active.

Steady-state reads use a safe atomic snapshot or equivalent low-contention design. Expiration workers stop on shutdown; partial initialization cleans up every timer and goroutine. Race tests concurrently log, resolve levels, mutate and expire overrides, reload, reopen, and shut down.

Initial control state is process-local. Multi-instance deployments apply changes per instance or through deployment configuration, and the API reports instance identity so incomplete rollout is visible.

## Sensitive Data and Full-Turn Inspection

Routine telemetry is metadata-only. Full-turn inspection remains in the authenticated, tenant-isolated replay API and SDK. Grafana, Tempo, and logs correlate back through tenant, loop, and event-sequence metadata; documentation must not create unauthenticated replay links.

This change does not implement sensitive payload capture. Any future mode must require stricter authorization, explicit enablement, mandatory short expiry, tenant-and-loop scope, immutable audit records, visible marking, field redaction, per-record and total-size limits, and tenant-isolated buffers/export/storage. Service-wide capture is rejected and captured values never enter Prometheus labels. Debug level alone never grants payload access.

## Configuration

Composition configuration covers independent signal enablement; `/metrics` bind/access policy; OTLP endpoint/protocol/TLS/headers/batching/queues/timeouts/sampling; stdout or file logging; file path and drain timeout; default level and maximum scoped TTL; histogram buckets; tenant/model/tool cardinality budgets, pinned values, catalog snapshots, hash version and salt; and metric label declarations.

Invalid explicitly enabled adapters fail startup. Disabled adapters require no configuration or backend. Logging reload validates a complete new configuration before publishing it. Metric buckets, exporter topology, and other SDK-reconstruction settings require restart.

## Security and Privacy

- Authentication and tenant resolution precede trace/debug metadata processing.
- Tenant identity comes only from authenticated Fuse context.
- Remote trace headers cannot alter tenant, loop, or node identity; baggage is dropped unless explicitly allowlisted.
- Metrics carry only budget-admitted stable opaque tenant IDs on explicitly tenant-relevant families; human-readable tenant names and all loop/node/event identifiers are forbidden.
- Trace/log backends are tenant-sensitive because they contain tenant, loop, and node metadata.
- `/metrics` obeys existing binding/access policy and must not become unintentionally public.
- Debug controls use existing administrative authentication and tenant authorization.
- Control bodies, authorization failures, and OTLP credentials are never logged.
- File creation uses restrictive permissions and established secure path-opening behavior.
- Operators own backend access control, retention, deletion, and backup.

## Reference Docker Compose Environment

The source-controlled local evaluation environment contains Fuse, Prometheus, Grafana, OpenTelemetry Collector, and Grafana Tempo. It provisions Prometheus scraping, Fuse-to-Collector OTLP, Collector-to-Tempo export, Grafana Prometheus and Tempo data sources, the Fuse dashboard, and safe trace-to-log/replay link templates.

Tempo is the sole tested reference trace backend. Standard OTLP remains backend-neutral. Loki is absent: documentation may show external stdout/file collection into Loki, but Fuse neither configures nor validates Loki.

The reference environment is explicitly non-production. Fuse owns its configuration and dashboard artifacts, not production hosting, availability, upgrades, storage, retention, or lifecycle.

## Grafana Dashboard

The deterministic source-controlled dashboard covers loop throughput/outcomes/latency/active count; model rate, latency, retries, timeouts, and failures; tool rate, latency, and failures; spawn rate/failures; projection/export errors and drops; and active log overrides. Tenant-relevant panels support grouping and filtering by opaque `tenant_id`; model and tool panels expose their declared dimensions. Relevant panels or exemplars link to Tempo when trace correlation is present.

Grafana may map tenant IDs to display names separately, but queries, recording rules, and alerts continue to key on `tenant_id`. A cardinality-health section shows budgets, admitted counts, overflow rates by dimension/metric, and whether `__overflow__` contributes to operational panels. Per-tenant reliability, quota, and misuse alert examples group by `tenant_id` and exclude overflow from tenant-specific notifications; a companion alert fires whenever tenant overflow makes individual attribution incomplete.

Queries tolerate restarts and absent optional signals. Variables never enumerate loop, node, event, trace, span, or arbitrary uncontrolled identifiers. Dashboard documentation includes the worst-case series estimate for every family using tenant, model, or tool labels, including the replica multiplier.

## Testing

### Unit and Contract Tests

- Every canonical event kind affecting metrics or logs.
- Exact curated label sets: tenant metrics include `tenant_id`, model metrics include `model`, tool metrics include `tool`, and unrelated metrics acquire none of them.
- Stable opaque tenant IDs are preserved for admitted values; display names never appear in labels.
- Independent budget enforcement, deterministic admission across restart/replicas, request-order independence, pinned-value precedence, and invalid over-budget pin rejection.
- Unknown and non-admitted values collapse to `__overflow__`; diagnostic counters expose each overflowed dimension without leaking its raw value.
- Catalog reconciliation causes no request-path eviction or series churn and obeys its bounded replacement rule.
- Multi-dimension series stay inside their documented product bound.
- Label-policy tests reject loop/node/sequence/trace/span IDs, payloads, error strings, URLs/paths, and undeclared labels.
- Payload exclusion from routine logs and traces.
- Duration/outcome mapping and tenant-safe deduplication.
- Override precedence, authorization inputs, expiry, reload, and effective state.
- Malformed remote context and trace attribute filtering.
- Span closure for success, error, timeout, cancellation, retry, and early returns.
- File reopen success/failure and partial-initialization cleanup.

### Projection Parity

Feed the projector through its real committed-event source and replay through its real durable source; compare identity, ordering, and payload-free summaries. Do not validate both sides through one shared helper. Replay/live tests exercise subscribe-first watermark deduplication.

### Integration Tests

- SDK parent span becomes parent of API and loop spans.
- Context reaches model/retries, tools, spawn, store, and pub/sub.
- Delayed replay uses span links.
- Collector receives and forwards traces to Tempo.
- Prometheus scrapes `/metrics`; Grafana provisions sources/dashboard.
- A metric anomaly correlates to trace, log, and replay identity.
- Exporter outage cannot fail or indefinitely block loop execution.
- Scoped controls cannot inspect or mutate another tenant.
- `/metrics` honors configured access control.
- Dashboard queries and alert rules handle `__overflow__`, including per-tenant quota or misuse examples and a tenant-overflow alert.

### Race and Lifecycle Tests

Focused `go test -race` coverage includes concurrent logging/reload/reopen/shutdown, scoped expiration during lookup, subscriber removal during fanout, and adapter construction failure after resource allocation. Synchronization tests do not rely solely on sleeps.

### Compose Smoke Test

Start the reference stack, execute a loop with model/tool/spawn activity, confirm Prometheus metrics and a nested Tempo trace, confirm correlated JSON logs, retrieve the full sequence through authenticated replay, and prove payloads are absent from routine telemetry.

## Documentation

Documentation provides a runnable local setup; complete adapter configuration; `/metrics` security; OTLP/W3C and SDK parent-span examples; metric and attribute catalogs; JSON log schema; stdout collection and safe file rotation; live global/tenant/loop controls; expiry/audit semantics; multi-instance limitations; privacy/retention responsibilities; Loki compatibility without direct shipping; and troubleshooting. Cardinality documentation includes budget and pin examples, catalog reconciliation and versioned hashing, restart/overflow semantics, capacity calculations with replicas, tenant-ID display-name mapping, and guidance for sizing budgets and investigating overflow.

The demonstrated operator journey is Grafana anomaly → Tempo trace → correlated structured logs → authenticated replay for the full turn.

## Rollout

1. Provider-neutral interfaces and no-op implementations, disabled by default.
2. Committed-event projector and payload-free structured logging.
3. Prometheus catalog and `/metrics`.
4. OpenTelemetry tracing and propagation.
5. Live log controls, auditing, and file reopen.
6. Reference Compose stack, dashboard, smoke test, and documentation.

Each stage preserves no-op operation when disabled and does not require an observability backend for loop execution.

## Acceptance Criteria

- Core runtime/event packages contain no vendor observability imports.
- Tenant, model, and tool labels are enabled by default on their curated metric families with deterministic finite budgets, stable opaque tenant IDs, observable overflow, and documented capacity estimates.
- Metrics expose no loop/node/event/trace/span identifiers, tenant display names, payloads, or uncontrolled labels.
- Per-tenant reliability and quota/misuse dashboards and alert examples work and explicitly handle overflow.
- All model and child calls have bounded, correctly ended attempt spans.
- SDK and cross-process trace propagation is verified.
- Routine telemetry contains no full prompts, responses, deltas, tool arguments, or results.
- Logs correlate through tenant, loop, node, sequence, trace, and span where applicable.
- Stdout works by default; file writes and reopen pass concurrency and race tests.
- Authenticated live global/tenant/loop levels work without restart; scoped overrides expire and are audited.
- Prometheus, Grafana, Collector, and Tempo work in the reference Compose environment.
- Tempo is the only tested trace backend; Loki is neither bundled nor directly shipped to.
- The Grafana-to-Tempo-to-logs-to-replay journey is reproducible.
- Exporter/projector failure cannot create unbounded stalls or make loops unavailable.
- All resources shut down on normal and partial-initialization paths.
- Relevant tests pass under `go test -race`.

## Out of Scope

- Production operation of Prometheus, Grafana, Tempo, Loki, or Collector.
- Direct Loki shipping or bundling.
- Jaeger-specific compatibility, configuration, or testing; track it as a separate backlog stub.
- A Fuse telemetry query API.
- Prometheus segmentation by loop, node, event, trace, span, or arbitrary uncontrolled dimensions.
- Historical Prometheus reconstruction.
- Persistent or cluster-wide live log overrides.
- Routine or sensitive payload capture into telemetry.
- Backend retention, deletion, backup, tenancy, or access-control lifecycle.
- Live changes requiring metric/exporter SDK reconstruction.

## Assumptions

- The canonical envelope can carry repository-owned trace carrier metadata without OTEL types in runtime/storage packages.
- Committed-event notification occurs only after durable append success and is available in process.
- Existing events expose enough lifecycle, bounded classification, outcome, attempt, and timing data; only absent timings need hooks.
- Existing administration provides authenticated actor identity, tenant authorization, and registration conventions.
- Existing binding access policy can protect `/metrics` without a second auth system.
- Stable opaque tenant IDs and versioned snapshots of the tenant, model, and tool catalogs are available to every metrics-producing replica.
- Catalog snapshots are sufficiently consistent for deterministic admission; temporary skew is operationally visible.
- `__overflow__` is reserved and cannot be a valid tenant ID, model name, or tool name.
- All three dimensions are enabled by default with finite positive deployment-configured budgets; pinned values guarantee visibility for alert-critical tenants when catalogs exceed their budgets.
- Process-local metrics and controls are acceptable initially.
- Tenant/loop/node IDs are acceptable in protected traces/logs as tenant-sensitive metadata.
- Raw errors may contain sensitive content and remain excluded absent reviewed redaction.
- Authenticated replay can retrieve events by tenant, loop, and sequence.
- Local Compose may use clearly marked development-only ports and credentials.
- W3C Trace Context is the interoperability format.
- Sensitive capture is not implemented by this change; its stated constraints bind any future design.
