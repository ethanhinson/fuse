<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0047 — Durable / distributed event store — survives restart and is shared across instances](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0047-durable-distributed-event-store.md)**
<!-- docket:backlink:end -->

# Results — 0047 durable / distributed event store

**Change:** #47 — durable-distributed-event-store
**Branch:** feat/durable-distributed-event-store
**Date:** 2026-08-10
**ADR:** ADR-0031 (new, Accepted); ADR-0030 amended; ADR-0024/0025 preserved.

## What shipped

A backend-agnostic durable EventStore + loop-registry seam so a loop's history **and its very
existence** survive a process restart and are reachable from any instance — fixing the
`runtime: loop not found` cross-process reattach gap that live verification of 0046 exposed.

- `internal/event` — new `DurableStore` (context+tenant-scoped `Append`/`Subscribe`/`Replay` keyed by
  `StreamKey{Tenant,Loop}`) and `LoopRegistry` (`Register`/`SetLive`/`Resolve`/`List`); `TenantID`/
  `LoopID`/`DefaultTenant`/`NormalizeTenant`/`ErrLoopUnknown`. Legacy `EventStore`/`NoopStore` kept
  byte-compatible. One shared conformance suite in `internal/event/eventtest`.
- `internal/event/fsstore` — subdirectory-per-tenant partitioning
  (`<baseDir>/<tenant>/<loop>/events.jsonl`) + a filesystem-backed loop registry sidecar
  (`loop.json`). `*FSDurableStore` satisfies both seams. Per-loop Seq recovered from disk on reopen.
- `internal/event/pgstore` (**build-tagged `//go:build pgstore`**) — Postgres `DurableStore` +
  `LoopRegistry` with `LISTEN`/`NOTIFY` shared pub/sub. Per-loop Seq via transaction-scoped per-loop
  advisory lock (per-loop total order, not a global sequence). `Append` = synchronous durable INSERT
  then a **decoupled bounded async publisher** for the `NOTIFY` (never blocks the loop).
- `internal/runtime` — `Observe`/`Attach`/`Send` carry `context.Context` + `event.TenantID`;
  `LoopConfig.Tenant`; `Deps.DurableStore`/`Deps.Registry`. The in-memory `r.loops` map is demoted to
  a per-instance **cache/projection** over the durable registry (source of truth for existence +
  liveness/ownership); a cold process resolves + replays + live-tails a loop it never started.
- `internal/loopserver` — `tenant` on `observeParams`/`sendParams`; the existing subscribe-before-
  replay + dedup-at-watermark wire discipline preserved unchanged in shape.
- `cmd/fuse` — build-tagged `selectDurableBackend`: fsstore by default (untagged binary imports **no**
  pgx/testcontainers/OTEL); pgstore selected under `-tags pgstore` when a Postgres DSN is set
  (`FUSE_PG_DSN`, else `DATABASE_URL`).
- **D5 observability hooks only** — a bare `context.Context` threads every durable op + the pub/sub
  hop; `(tenant_id, loop_id, node_id)` exposed consumer-readable; **no** OTEL import/exporter/metrics
  surface (full observability is change 0051).

## Verification

- **`go test ./...` (bare — NO build tags, NO Postgres, NO Docker): PASS.** Non-negotiable constraint
  met — the pgstore backend + its tests are behind `//go:build pgstore` and never compile untagged.
- **`go test -race ./...`: PASS** (independently re-confirmed by the implementer at close).
- **`go test -tags pgstore -race ./internal/event/pgstore/...`: PASS against a real ephemeral
  Postgres** (`postgres:16-alpine` via testcontainers/Docker) — cross-instance pub/sub, from_seq
  reattach dedup, non-blocking-Append-under-Postgres, and tenant isolation all exercised. The suite
  `t.Skip`s cleanly if no container runtime is present, so even `-tags pgstore` never hard-fails on a
  Docker-less box.
- **No live-model verification turns were needed** — the load-bearing checks here are
  storage/concurrency tests, not model-quality tests. (Had one been needed, project policy mandates a
  non-Anthropic gateway model via `LLM_GATEWAY_URL`.)

## Manual checks for the merge gate

The bare CI suite does **not** exercise the Postgres backend (by design). To validate the deployable
path locally before/at merge, with Docker running:

```
cd <repo> && go test -tags pgstore -race ./internal/event/pgstore/...
```

Expect it to start a testcontainers Postgres and run the conformance + cross-instance + non-blocking +
tenant-isolation suites. (Consider wiring a `-tags pgstore` job into CI as a follow-up so the
deployable backend is gated on every PR, not just locally.)

## Findings & notable decisions

- **ADR-0030 reconciled by AMEND, not supersede** (recorded as ADR-0031, change:47). ADR-0030's
  value-threading + policy-free-seam decisions all still hold; 0047 only moves the *source of truth*
  for loop existence from the in-memory map to the durable registry. Supersession would have wrongly
  signalled a reversal. ADR-0024 (plaintext birth) and ADR-0025 (sole per-loop Seq allocator,
  non-blocking Append) are **preserved** and explicitly honored by the new backends; ADR-0025 got a
  change-47 `## Update` noting the per-loop-advisory-lock allocator now also holds for Postgres.
- **A real concurrency bug was caught and fixed under `-race` during the build**: `pgstore.deliver`
  originally snapshotted `*subscriber` pointers under the lock then sent outside it, racing a
  concurrent unsubscribe/`Close` (send-on-closed-channel, ~1-in-5 under `-race`). Fixed by
  snapshotting subscriber **ids** and re-validating membership under the re-acquired lock before each
  non-blocking send. Stress-run 10× green.
- **Review-driven tenant-correctness fixes** (would have bitten only the cross-instance/finished-loop
  send path under a real tenant): `Runtime.Send` now threads `tenant`; the `r.loops` cache asserts
  the cached loop's tenant matches the request on every hit (falls through to the tenant-scoped
  durable registry on mismatch) rather than returning the wrong tenant's store. New regression tests
  `TestSendResolvesUnderRequestedTenant` and `TestCacheHitDoesNotCrossTenants`.

## Plan deviations

- The durable fsstore type is `*FSDurableStore` (not `*FSEventStore`) — the plan's shared name is
  impossible in Go since the legacy no-arg `EventStore` methods and the durable context+key methods
  collide (no overloading). The store is its own registry (`return s, s`); there is no separate
  `NewFSLoopRegistry`.
- Postgres DSN is read from the environment (`FUSE_PG_DSN`/`DATABASE_URL`) because `config.Config` has
  no DSN field and extending the config schema was out of 0047's scope.
- `startParams` was not given a `tenant` field (only `observeParams`/`sendParams`) — `loop.start`
  currently starts under the default tenant; a per-start tenant is a small follow-up when auth/tenant
  identity (change 0049) lands.

## Follow-ups (not filed as stubs — auto-capture disabled this run; reported here per policy)

- Wire a `-tags pgstore` CI job so the deployable backend is gated on every PR.
- `loop.start` per-start tenant parameter (naturally folds into change 0049 auth/multi-tenancy).
- Postgres retention/rotation policy for the `events` table (append-only today, matching fsstore).
