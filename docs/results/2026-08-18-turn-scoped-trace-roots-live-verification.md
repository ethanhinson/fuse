<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0071 — Turn-scoped trace roots for interactive loops — end loop.run at first park, per-turn root spans](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0071-turn-scoped-trace-roots-interactive-loops.md)**
<!-- docket:backlink:end -->

# Change 0071 — live verification of acceptance criterion 1

Plan Task 6. Evidence record for the change results file.

**Path taken: 1 (real live observation).** No deviation. The plan's fallback (an
in-memory/stdout `sdktrace.SpanExporter` substitute) was **not** needed: the local
observability stack — `otel/opentelemetry-collector-contrib` on `:4317/:4318`,
`grafana/tempo` on `:3200`, Grafana on `:3000` (`deploy/observability/docker-compose.yml`)
— was reachable, so the criterion was observed in Tempo against real exported spans from a
real interactive session driven by a real model.

## Setup

- Binary built from this branch (`go build ./cmd/fuse`), run as `fuse loop-serve-net` with a
  throwaway `HOME` whose `~/.fuse/config.yml` enabled OTLP/gRPC traces to `localhost:4317`
  (`sample_ratio: 1`, `batch_size: 1`, `service.instance.id: turnspan-live-0071`) and a
  `loop_server.auth` entry for tenant `tenant-live`.
- **Model: `cloud/qwen3-8b` via the local LiteLLM gateway (`LLM_GATEWAY_URL`).** A cheap
  gateway model, per the standing project rule. **No Anthropic/Claude model was used.**
- Drive: Connect `StartLoop{interactive: true}` → park → `Send` → park → `Send` → park.
  Three real model turns. Loop id `001a012a49cd46ccb64da34fd`.

## Observed in Tempo, while the session was still parked and alive

Liveness was confirmed *after* the trace reads by a further `Send` to the same loop id
returning HTTP 200 — the spans below were therefore exported from a session that had not
terminated.

**First-turn trace `0c4bb991de308252ddb411808fb9f5fe`** — rooted at
`fuse.api.request.start_loop`, containing `fuse.loop.run` (span `bc734cc65f17df74`,
`tenant=tenant-live`) **already ended** with `fuse.outcome=success` and a finite duration,
plus that turn's `fuse.model.attempt.complete` and `fuse.store.append` children. The whole
first-turn trace was complete and queryable while the session was still parked.

**One complete `fuse.loop.turn`-rooted trace per completed later turn:**

| trace id | root span | parent | `loop_id` | `tenant` | `fuse.turn.index` | link → |
| --- | --- | --- | --- | --- | --- | --- |
| `c5898ab40ecd6af3c5e589902be21b16` | `fuse.loop.turn` (`4835236f640653e5`) | none (new root) | `001a012a49cd46ccb64da34fd` | `tenant-live` | `2` | `0c4bb991de308252ddb411808fb9f5fe` / `bc734cc65f17df74` |
| `bda2a12317b23357796618197f1b754d` | `fuse.loop.turn` (`daf1d1c8a144b885`) | none (new root) | `001a012a49cd46ccb64da34fd` | `tenant-live` | `3` | `0c4bb991de308252ddb411808fb9f5fe` / `bc734cc65f17df74` |

Each link target is exactly the session root's trace id and the `fuse.loop.run` span id, so
the causal edge back to the session root is present and correct. Both turn roots ended with
`fuse.outcome=success` at the following park.

**Beyond the criterion:** each turn trace also contains that turn's
`fuse.model.attempt.complete` as a **child of the turn root**, not of the session. Task 4's
plan text allowed a fallback in which children still parent to the session; live traffic
shows the stronger outcome — per-turn child parenting works end to end.

## Result

Acceptance criterion 1 is **verified live**. Focused suite
`go test -race -count=1 ./internal/observe/... ./internal/runtime/...` green.
