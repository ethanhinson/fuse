<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0051 — Observability for the loop — OTEL traces + Prometheus metrics + Grafana + structured logs](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0051-loop-observability-otel-metrics.md)**
<!-- docket:backlink:end -->

# Observability for the loop — OTEL traces + Prometheus metrics + Grafana + structured logs — results

Change: #51 · Branch: feat/loop-observability-otel-metrics · PR: <set at PR open> · Plan: docs/superpowers/plans/2026-08-12-loop-observability-otel-metrics.md · ADRs: 0038, 0039, 0040, 0041

## Verify (human)

- [x] Drove a real loop through the authenticated Connect network entry and received `OBSERVABILITY-E2E-REPLY` from the scripted model gateway.
- [x] Drove a second real loop through the direct `fuse` CLI entry and received the same reply.
- [x] Inspected the provisioned Grafana dashboard with live throughput, outcome, latency, and model data. Receipt: [Grafana dashboard](screenshots/grafana-fuse-loop-observability.png).
- [x] Queried Tempo trace `58fe8a821c41c20a5512125873f4a983` in Grafana Explore and inspected the nested API, loop, store, and model spans. Receipts: [Tempo trace](screenshots/grafana-tempo-trace.png) and [Tempo span tree](screenshots/grafana-tempo-trace-spans.png).
- [x] Verified the exported trace resource identifies `service.name=fuse`, `service.version=0.1.0-dev`, and `service.instance.id=e2e-instance`.

## Findings

- The production composition now projects exact committed event envelopes through a bounded, non-blocking dispatcher; this is recorded by ADR-0038 and ADR-0039.
- Provider-neutral observation fans out through a composite observer, and process-global logging mutations require an operator capability; these decisions are recorded by ADR-0040 and ADR-0041.
- The first deterministic deep whole-branch review returned eleven findings. All were fixed before the acceptance run, including production entry wiring, trace-carrier persistence, exact committed identity, bounded dispatch, truthful metric outcomes, payload-free correlation fields, operator authorization, shutdown races, zero sampling, and cardinality budgeting.
- A second test-quality review was explicitly requested. The Superpowers code-review role was unavailable in this harness, so the configured `docket-review-deep` whole-branch reviewer was used as the documented fallback. Its six findings were fixed: PostgreSQL trace-carrier parity, real network/CLI acceptance coverage, cancellable dispatcher shutdown, auxiliary model-call coverage, bounded OTLP outage accounting, and semantic deploy-artifact validation.
- Live Compose acceptance found two deployment defects that unit tests had not exposed: OTLP receivers were bound to loopback inside their containers, and exported spans lacked an OTEL service resource. Both were fixed with focused regression tests before the final trace and screenshots were captured.

## Acceptance evidence

The repository's Compose topology was launched as the isolated `fuse-obs-e2e` project because the default development ports were already owned by unrelated local containers. The same source-controlled Collector, Tempo, Prometheus, and Grafana configuration was used with host ports remapped to `14318`, `13200`, `19091`, and `13000`. Fuse exported OTLP over HTTP/protobuf to the Collector.

- Prometheus reported the Fuse target healthy and returned live samples for `fuse_loop_operations_total`, `fuse_model_calls_total`, `fuse_event_projection_total`, operation durations, and cardinality-budget health.
- Structured JSON logs in `/tmp/fuse-obs-e2e/fuse.jsonl` contained sequence, tenant, loop, trace, and span correlation fields. A secret/payload scan found no task, reply, token, or authorization payloads.
- Tempo returned trace `58fe8a821c41c20a5512125873f4a983` with seven nested spans across `api.request.start_loop`, `loop.run`, `store.append`, and model-attempt work, carrying the corrected Fuse resource identity.
- Permanent automated gates now include `make observability-acceptance`, `make observability-race`, `make observability-compose-smoke`, and semantic validation of Collector routing, Grafana datasources/dashboard PromQL, Prometheus alerts, and OTEL resource attributes.

## Follow-ups

None were auto-captured. Autonomous capture is disabled, and all review and live-acceptance findings belonged to this branch and were fixed here.

## Notes / deviations

- The resolved Superpowers plan and finish roles were unavailable, so the docket auto fallbacks authored the cross-tree plan and will push/open the PR without merging. Build tasks still ran through the configured `docket-build` worker contracts, and both whole-branch reviews used the deterministic deep reviewer.
- One Task 5 worker report contained a nonexistent commit SHA. Under the explicitly authorized resume path, the actual branch commit `4d3b5ce9fc2db5254bc1eb55d3c078c62c039923` was inspected directly and its focused race suite passed; valid work was retained.
