<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0047 — Durable / distributed event store — survives restart and is shared across instances](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0047-durable-distributed-event-store.md)**
<!-- docket:backlink:end -->

# Durable / Distributed Event Store Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make a loop's event history *and its very existence* survive a process restart and be reachable from any instance, via a backend-agnostic durable EventStore + loop-registry seam with two implementations (fsstore, Postgres), tenant isolation, and a shared pub/sub live cross-instance tail.

**Architecture:** Generalize the existing `event.EventStore` seam into a durable, tenant-scoped, context-carrying `DurableStore` + `LoopRegistry` contract. Keep `fsstore` as the local/dev backend (extended for tenant subdir partitioning + durable registry via a filesystem index). Add a `pgstore` backend (behind `//go:build pgstore`) that is the deployable one and the home of Postgres `LISTEN`/`NOTIFY` pub/sub. Thread `tenant_id` and `context.Context` through the runtime seam (`Runtime.Observe`/`Attach`/`StartLoop`) so `inProcRuntime` resolves `(tenant_id, loop_id)` from the durable registry, with the in-memory `r.loops` map demoted to a per-instance cache/projection.

**Tech Stack:** Go 1.26.5; existing `internal/event`, `internal/event/fsstore`, `internal/runtime`, `internal/loopserver`, `cmd/fuse`; new `internal/event/pgstore` (build-tagged); `github.com/jackc/pgx/v5` (pg driver, tagged import only); `github.com/testcontainers/testcontainers-go` + its `postgres` module (test-only, tagged).

## Global Constraints

- **Bare `go test ./...` (no build tags, no Postgres, no Docker) MUST pass** — non-negotiable. All Postgres production code and its tests live behind `//go:build pgstore`; untagged builds never import pgx/testcontainers.
- **`go test -race ./...` MUST stay green** — the load-bearing gate; cross-instance liveness + pub/sub introduce new concurrency.
- **ADR-0024 preserved:** events born **plaintext**, independent of the segment store; a Postgres `payload` column holds the plaintext JSON body.
- **ADR-0025 preserved:** the store is the **sole Seq allocator**; a **per-loop** total order (callers pass `Seq == 0`); `Replay(from)` is a clean `seq > from` cursor scoped to `(tenant_id, loop_id)`.
- **ADR-0025/ADR-0016 preserved:** `Append` **never blocks** on a slow/absent subscriber or on `NOTIFY` delivery — non-blocking drop-newest-with-gap fan-out; it must never wedge an agent's scheduler slot.
- **No `Event` envelope / Kind change** (`internal/event/event.go`); no new wire format.
- **No OTEL import, exporter, or `/metrics` surface** — D5 is hooks-only (a bare `context.Context` + the `(tenant_id, loop_id, node_id)` labeling triple).
- **Any live-model verification uses a NON-Anthropic cheap gateway model via `LLM_GATEWAY_URL`** — never Claude/Anthropic/Fable/Opus/Sonnet/Haiku. Load-bearing verifications here are storage/concurrency tests; live-model turns minimal.
- **`internal/runtime` imports no `cmd/fuse`** (policy-free-seam invariant, ADR-0030); the Postgres backend is selected at the composition root (`cmd/fuse`), threaded in as a value — never a construction-time shared global (`deglobalize-holder-also-per-instance-the-shared-graph`).

---

## File Structure

- `internal/event/store.go` — MODIFY: add the durable seam types. Introduce `TenantID`/`LoopID` string aliases, a `StreamKey{Tenant, Loop}`, a `DurableStore` interface (`context`-carrying, tenant-scoped `Append`/`Subscribe`/`Replay`), and a `LoopRegistry` interface (`Register`/`SetLive`/`Resolve`/`List`, liveness/ownership). Keep the legacy `EventStore` + `NoopStore` intact (byte-compatible) and provide an adapter so existing callers compile.
- `internal/event/fsstore/store.go` — MODIFY: tenant-subdir partitioning (`<baseDir>/<tenant>/<loop>/events.jsonl`), a `context.Context` first parameter on the durable ops, and a filesystem-backed `LoopRegistry` (a per-tenant registry index file the store maintains + reads).
- `internal/event/registry.go` — CREATE: the shared behavioral registry contract + the in-memory/default helpers both backends reuse (types only; no backend logic).
- `internal/event/store_conformance_test.go` — CREATE: the ONE shared behavioral suite (a `func testDurableStore(t, factory)`), run against fsstore always; imported by the pg test under the tag.
- `internal/event/pgstore/store.go` — CREATE (`//go:build pgstore`): Postgres `DurableStore` + `LoopRegistry` (schema, per-loop advisory-lock Seq, `LISTEN`/`NOTIFY` pub/sub, decoupled async publisher).
- `internal/event/pgstore/store_test.go` — CREATE (`//go:build pgstore`): testcontainers Postgres harness + the shared conformance suite + pg-specific pub/sub tests; `t.Skip` if the container runtime is unavailable.
- `internal/runtime/runtime.go` — MODIFY: thread `context.Context` + `tenant_id` onto `Observe`/`Attach` (and `LoopConfig`); add a `Registry`/`DurableStore` seam field to `Deps`.
- `internal/runtime/inproc.go` — MODIFY: resolve `(tenant, loop)` via the durable registry when present, demote `r.loops` to a cache; register loop existence/liveness durably at `StartLoop` and on completion.
- `internal/loopserver/server.go` — MODIFY: carry `tenant_id` on `startParams`/`observeParams`/`sendParams`; pass it + `ctx` through to the runtime; preserve subscribe-before-replay + dedup-at-watermark over the wire (already present).
- `cmd/fuse/loop_server.go` — MODIFY: select the durable backend (fsstore by default; pgstore when configured, via a tagged constructor seam), threaded as a value into `runtime.Deps`.
- `cmd/fuse/durable_backend.go` + `cmd/fuse/durable_backend_pg.go` — CREATE: a build-tagged selector so the untagged binary only ever wires fsstore (no pgx import without the tag).

---

## Task 1: Durable seam types (tenant + context) — interface only

**Files:**
- Modify: `internal/event/store.go`
- Create: `internal/event/registry.go`
- Test: `internal/event/store_test.go` (extend)

**Interfaces:**
- Produces:
  - `type TenantID string`, `type LoopID string`
  - `type StreamKey struct { Tenant TenantID; Loop LoopID }`
  - `type DurableStore interface { Append(ctx context.Context, key StreamKey, e Event) error; Subscribe(ctx context.Context, key StreamKey) (<-chan Event, func(), error); Replay(ctx context.Context, key StreamKey, from Seq) ([]Event, error) }`
  - `type LoopRecord struct { Key StreamKey; OwnerNodeID string; Live bool; CreatedAt, UpdatedAt time.Time }`
  - `type LoopRegistry interface { Register(ctx context.Context, rec LoopRecord) error; SetLive(ctx context.Context, key StreamKey, live bool, ownerNodeID string) error; Resolve(ctx context.Context, key StreamKey) (LoopRecord, error); List(ctx context.Context, tenant TenantID) ([]LoopRecord, error) }`
  - `var ErrLoopUnknown = errors.New("event: loop not found in registry")` (distinguishable, cross-process)
  - `const DefaultTenant TenantID = "_default"` (empty tenant maps here so single-tenant local behavior is preserved)

- [ ] **Step 1: Write the failing test** — assert the new types exist and `DefaultTenant` normalization helper works.

```go
// internal/event/store_test.go
func TestNormalizeTenantDefaults(t *testing.T) {
	if got := event.NormalizeTenant(""); got != event.DefaultTenant {
		t.Fatalf("NormalizeTenant(\"\") = %q, want %q", got, event.DefaultTenant)
	}
	if got := event.NormalizeTenant("acme"); got != event.TenantID("acme") {
		t.Fatalf("NormalizeTenant(acme) = %q, want acme", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails** — `go test ./internal/event/ -run TestNormalizeTenant` → FAIL (undefined).

- [ ] **Step 3: Write minimal implementation** — add the types above to `store.go`, `registry.go`, and:

```go
func NormalizeTenant(t TenantID) TenantID {
	if t == "" {
		return DefaultTenant
	}
	return t
}
```

Keep the legacy `EventStore` + `NoopStore` unchanged. Do NOT yet wire any backend.

- [ ] **Step 4: Run test to verify it passes** — `go test ./internal/event/ -run TestNormalizeTenant` → PASS.

- [ ] **Step 5: Run full package + vet** — `go build ./... && go test ./internal/event/...` → PASS (nothing else references the new types yet).

- [ ] **Step 6: Commit** — `git commit -m "feat(event): #47 durable-store + loop-registry seam types (tenant, context)"`

---

## Task 2: Shared behavioral conformance suite

**Files:**
- Create: `internal/event/store_conformance_test.go`

**Interfaces:**
- Produces: `func RunDurableStoreConformance(t *testing.T, newStore func(t *testing.T) (DurableStore, LoopRegistry))` — the ONE suite both backends pass (`parity-test-feeds-each-side-its-own-production-source`: each backend is fed via its own real constructor).

- [ ] **Step 1: Write the suite** — cases: (a) Append allocates contiguous per-loop Seq from 1; (b) `Replay(from)` returns `seq>from` in order, tenant-scoped; (c) two loops in one tenant have independent Seq streams; (d) **cross-tenant isolation**: a `Resolve`/`Replay` for `(tenantY, loopOfX)` returns `ErrLoopUnknown`/empty, never X's stream; (e) subscribe-before-replay + dedup-at-watermark: subscribe, append during the window, replay, assert each Seq exactly once (no gap/no dup); (f) non-blocking: fill a subscriber buffer, burst appends, assert prompt return + a recorded gap (never blocks); (g) registry `Register`→`Resolve`→`List` round-trips liveness/ownership. Put the exported entry point in a NON-`_test.go`? No — it is a test helper, keep it in `store_conformance_test.go` in package `event` and call it from backend test packages via a thin re-export, OR place it in an internal `eventtest` helper package. **Decision:** create `internal/event/eventtest/conformance.go` (a normal package, not `_test.go`) exporting `RunDurableStoreConformance`, so both `fsstore` and `pgstore` test packages import it.

- [ ] **Step 2: Move the helper** — create `internal/event/eventtest/conformance.go` with `RunDurableStoreConformance`. It imports only `internal/event` + `testing`.

- [ ] **Step 3: Run** — `go build ./internal/event/...` → PASS (no backend consumes it yet; a build-only check).

- [ ] **Step 4: Commit** — `git commit -m "test(event): #47 one shared durable-store conformance suite (eventtest)"`

---

## Task 3: fsstore — tenant partitioning + context ops

**Files:**
- Modify: `internal/event/fsstore/store.go`
- Test: `internal/event/fsstore/store_test.go` (extend)

**Interfaces:**
- Consumes: Task 1 types.
- Produces: `*FSEventStore` satisfies `event.DurableStore`. New constructor `NewDurableFSStore(baseDir string) *FSEventStore` that keys by `StreamKey` internally (path `<baseDir>/<tenant>/<loop>/events.jsonl` via `NormalizeTenant`). Legacy `NewFSEventStore(baseDir, sessionID)` retained (maps to `DefaultTenant`, `loop=sessionID`) so existing callers compile byte-identically.

- [ ] **Step 1: Write the failing test** — append under tenant `acme`/loop `L1` and tenant `beta`/loop `L1`; assert distinct files, distinct Seq streams, and `Replay(ctx, {beta,L1}, 0)` never returns acme's events.

```go
func TestFSStoreTenantPartitioning(t *testing.T) {
	s := fsstore.NewDurableFSStore(t.TempDir())
	ctx := context.Background()
	_ = s.Append(ctx, event.StreamKey{Tenant: "acme", Loop: "L1"}, event.Event{Kind: event.KindTurnStart})
	_ = s.Append(ctx, event.StreamKey{Tenant: "beta", Loop: "L1"}, event.Event{Kind: event.KindTurnEnd})
	acme, _ := s.Replay(ctx, event.StreamKey{Tenant: "acme", Loop: "L1"}, 0)
	beta, _ := s.Replay(ctx, event.StreamKey{Tenant: "beta", Loop: "L1"}, 0)
	if len(acme) != 1 || acme[0].Kind != event.KindTurnStart { t.Fatalf("acme leak: %+v", acme) }
	if len(beta) != 1 || beta[0].Kind != event.KindTurnEnd { t.Fatalf("beta leak: %+v", beta) }
	if acme[0].Seq != 1 || beta[0].Seq != 1 { t.Fatalf("per-loop Seq must start at 1") }
}
```

- [ ] **Step 2: Run to verify it fails** — `go test ./internal/event/fsstore/ -run TenantPartitioning` → FAIL.

- [ ] **Step 3: Implement** — internally hold a `map[StreamKey]*stream` (each `stream` = the existing per-file writer/subscriber/seq state), lazily opened; path via `filepath.Join(baseDir, string(NormalizeTenant(k.Tenant)), string(k.Loop), "events.jsonl")`. Wire the `context.Context` param through (honor `ctx.Err()` at entry; no cancellation mid-write needed for the fs path). Preserve non-blocking fan-out + `Dropped()`.

- [ ] **Step 4: Run to verify it passes** + keep the legacy `NewFSEventStore` tests green — `go test ./internal/event/fsstore/...` → PASS.

- [ ] **Step 5: Run the shared conformance suite against fsstore** — add `TestFSStoreConformance` calling `eventtest.RunDurableStoreConformance(t, fsFactory)`.

- [ ] **Step 6: `-race`** — `go test -race ./internal/event/...` → PASS.

- [ ] **Step 7: Commit** — `git commit -m "feat(event/fsstore): #47 tenant subdir partitioning + context durable ops"`

---

## Task 4: fsstore — filesystem-backed loop registry

**Files:**
- Modify: `internal/event/fsstore/store.go` (or add `internal/event/fsstore/registry.go`)
- Test: `internal/event/fsstore/registry_test.go` (create)

**Interfaces:**
- Consumes: Task 1 `LoopRegistry`.
- Produces: `*FSEventStore` (or a sibling `*FSLoopRegistry` sharing the baseDir) satisfies `event.LoopRegistry`. Registry state persists as a small JSON sidecar `<baseDir>/<tenant>/<loop>/loop.json` (`{owner_node_id, live, created_at, updated_at}`); `List(tenant)` enumerates `<baseDir>/<tenant>/*/loop.json`; `Resolve` returns `ErrLoopUnknown` when absent.

- [ ] **Step 1: Failing test** — `Register` then `Resolve` returns the record; `SetLive(false)` flips liveness; `Resolve` of an unregistered loop returns `ErrLoopUnknown`; `List` returns only that tenant's loops. Guard the `dirent-isdir-skips-symlinks` learning: use `os.Stat` fallback when enumerating.

- [ ] **Step 2: Run → FAIL.**

- [ ] **Step 3: Implement** the sidecar read/write under the store mutex (atomic write via temp+rename).

- [ ] **Step 4: Run → PASS**, then `-race` on the package.

- [ ] **Step 5: Commit** — `git commit -m "feat(event/fsstore): #47 filesystem-backed durable loop registry"`

---

## Task 5: Runtime seam — thread tenant + context, durable registry-backed resolution

**Files:**
- Modify: `internal/runtime/runtime.go`, `internal/runtime/inproc.go`
- Test: `internal/runtime/inproc_test.go`, `internal/runtime/multiloop_test.go` (extend), `internal/runtime/durable_reattach_test.go` (create)

**Interfaces:**
- Consumes: Tasks 1/3/4.
- Produces:
  - `LoopConfig` gains `Tenant event.TenantID` (empty ⇒ `DefaultTenant`).
  - `Runtime.Observe(ctx context.Context, tenant event.TenantID, loopID string) (<-chan event.Event, func(), error)`
  - `Runtime.Attach(ctx context.Context, tenant event.TenantID, loopID string, from event.Seq) ([]event.Event, error)`
  - `Deps` gains `DurableStore event.DurableStore` and `Registry event.LoopRegistry` (both optional; when nil, fall back to the current per-loop fsstore + in-memory-only path so CLI single-loop bindings stay byte-identical).
  - `inProcRuntime.lookup` resolves against the durable registry first (for existence/liveness) and treats `r.loops` as a **cache**: a hit in the map returns the live `*loop`; a miss but a durable `Resolve` success returns a **replay-only handle** (Attach works; Observe subscribes to the durable store's cross-instance channel; Send returns `ErrLoopNotFound`/`ErrLoopFinished` per liveness). A durable `ErrLoopUnknown` ⇒ `ErrLoopNotFound`.

- [ ] **Step 1: Failing test (the load-bearing one)** — `durable_reattach_test.go`: build runtime A with a shared `DurableStore`+`Registry` (fsstore under a temp dir); `StartLoop` a loop, append events, let it finish. Build a **fresh** runtime B over the **same** store/registry with an **empty `r.loops`**. Assert `B.Attach(ctx, tenant, loopID, 0)` returns the full history (reproducing the `runtime: loop not found` bug and asserting it now SUCCEEDS).

```go
func TestColdRuntimeAttachesToPriorProcessLoop(t *testing.T) {
	dir := t.TempDir()
	store := fsstore.NewDurableFSStore(dir)
	reg := fsstore.NewFSLoopRegistry(dir) // or the store itself
	depsA := runtime.Deps{DurableStore: store, Registry: reg, /* BuildAgent stub */}
	// ... start loop on A, drive a few events, finish ...
	// Fresh B, empty in-memory map, same durable store+registry:
	depsB := runtime.Deps{DurableStore: store, Registry: reg, /* same stub */}
	rB := runtime.New(depsB)
	hist, err := rB.Attach(context.Background(), event.DefaultTenant, loopID, 0)
	if err != nil { t.Fatalf("cold attach failed (the 0047 bug): %v", err) }
	if len(hist) == 0 { t.Fatalf("cold attach returned no history") }
}
```

- [ ] **Step 2: Run → FAIL** (signature + resolution not yet durable).

- [ ] **Step 3: Implement** the signature changes + registry-backed `lookup`; register loop existence at `StartLoop` (`Register` + `SetLive(true, rootNodeID)`) and `SetLive(false, ...)` in the run-completion goroutine. Demote `r.loops` to a cache. Guard `deglobalize-holder-also-per-instance-the-shared-graph`: B derives liveness from the durable registry, never from a shared in-memory object; each `StartLoop` opens its own stream via the injected store (no construction-time shared per-loop object).

- [ ] **Step 4: Update all call sites** — `Observe`/`Attach` callers in `internal/loopserver` + tests (Task 6 handles loopserver; here fix runtime's own tests and any compile breaks). Enumerate by `go build ./...`.

- [ ] **Step 5: Run → PASS**; keep `multiloop_test.go` green (per-loop Seq from 1, no cross-loop bleed). Then `go test -race ./internal/runtime/...`.

- [ ] **Step 6: Commit** — `git commit -m "feat(runtime): #47 tenant+context seam; durable-registry-backed loop resolution (r.loops as cache)"`

---

## Task 6: loopserver — tenant param, wire through, preserve wire dedup

**Files:**
- Modify: `internal/loopserver/server.go`
- Test: `internal/loopserver/server_test.go`, `internal/loopserver/reattach_test.go` (extend)

**Interfaces:**
- Consumes: Task 5 signatures.
- Produces: `startParams`/`sendParams`/`observeParams` gain `Tenant string \`json:"tenant,omitempty"\`` (empty ⇒ default). `serveObserve` passes `ctx` + tenant to `rt.Observe`/`rt.Attach`; the existing subscribe-before-replay + dedup-at-watermark + gap-flagging path is UNCHANGED in shape (only the two calls gain params).

- [ ] **Step 1: Failing test** — a `tenant`-scoped `loop.observe` reaches the runtime with the right tenant; the `storeBackedRuntime` double gains the new signatures. Existing `TestReattachReplaysThenResumesExactlyOnce` must still pass after the signature update.

- [ ] **Step 2: Run → FAIL** (double signatures mismatch).

- [ ] **Step 3: Implement** — update `storeBackedRuntime` methods + the three param structs + the two runtime calls; thread `ctx` from `Serve`/`serveObserve`.

- [ ] **Step 4: Run → PASS**; `-race` on loopserver.

- [ ] **Step 5: Commit** — `git commit -m "feat(loopserver): #47 tenant param threaded; wire dedup preserved"`

---

## Task 7: cmd/fuse — build-tagged durable backend selector (fsstore default)

**Files:**
- Modify: `cmd/fuse/loop_server.go`
- Create: `cmd/fuse/durable_backend.go` (no tag — fsstore selector), `cmd/fuse/durable_backend_pg.go` (`//go:build pgstore` — pg selector)
- Test: `cmd/fuse/loop_server_test.go`, `cmd/fuse/loop_server_multiloop_test.go` (keep green)

**Interfaces:**
- Consumes: Tasks 3/4/5; pgstore (Task 8) only under the tag.
- Produces: `func selectDurableBackend(cfg config.Config) (event.DurableStore, event.LoopRegistry, error)` with two implementations selected by build tag. The untagged file returns the fsstore durable store+registry under `session.DefaultLogDir()`; the tagged file returns pgstore when `cfg` names a Postgres DSN, else falls through to fsstore. `buildLoopServerRuntimeDeps` sets `Deps.DurableStore`/`Deps.Registry` from the selector, threaded as VALUES.

- [ ] **Step 1: Failing test** — assert `runLoopServer` (untagged) wires a durable fsstore backend and that `selectDurableBackend` returns non-nil store+registry with no Postgres configured. Multi-loop test still asserts per-loop isolation.

- [ ] **Step 2: Run → FAIL.**

- [ ] **Step 3: Implement** the untagged selector + `Deps` wiring; keep `BaseDir` for the legacy per-loop fsstore path OR route the store through the new durable seam (prefer the durable seam so cold reattach works for `fuse loop-server`).

- [ ] **Step 4: Run → PASS**; `go build ./...` (untagged) has NO pgx import; `go test -race ./cmd/fuse/...`.

- [ ] **Step 5: Commit** — `git commit -m "feat(cmd/fuse): #47 build-tagged durable backend selector (fsstore default, no pg import untagged)"`

---

## Task 8: pgstore — Postgres DurableStore + LoopRegistry (build-tagged)

**Files:**
- Create: `internal/event/pgstore/store.go` (`//go:build pgstore`), `internal/event/pgstore/schema.sql` (embedded), `go.mod`/`go.sum` (add pgx + testcontainers)

**Interfaces:**
- Consumes: Task 1 types, Task 2 conformance suite.
- Produces: `func Open(ctx context.Context, dsn string) (*PGStore, error)`; `*PGStore` satisfies `event.DurableStore` + `event.LoopRegistry`. Schema per the reconcile decision (events PK `(tenant_id, loop_id, seq)`; per-loop `pg_advisory_xact_lock` Seq; `loops` registry table). `Append` = synchronous durable INSERT, then `NOTIFY` handed to a decoupled bounded async publisher goroutine. `Subscribe` = a `LISTEN` connection feeding a non-blocking drop-newest-with-gap channel.

- [ ] **Step 1: Add tagged deps** — `go get github.com/jackc/pgx/v5 github.com/testcontainers/testcontainers-go github.com/testcontainers/testcontainers-go/modules/postgres`; confirm `go build ./...` (untagged) is unaffected (no import without the tag) and `go build -tags pgstore ./...` compiles.

- [ ] **Step 2: Write the schema + Open** — embed `schema.sql` via `//go:embed`; `Open` connects (pgxpool) and applies the schema idempotently (`CREATE TABLE IF NOT EXISTS`).

- [ ] **Step 3: Implement Append** — one transaction: `pg_advisory_xact_lock(hashtextextended(tenant||'/'||loop, 0))`; `INSERT ... (seq) VALUES ((SELECT COALESCE(MAX(seq),0)+1 FROM events WHERE tenant_id=$1 AND loop_id=$2), ...)`; set `e.Seq` from the returned seq; commit; then enqueue a `NOTIFY <channel>, <seq>` on the async publisher (bounded; drop-with-gap on overflow — Append never blocks).

- [ ] **Step 4: Implement Replay** — `SELECT ... WHERE tenant_id=$1 AND loop_id=$2 AND seq>$3 ORDER BY seq`.

- [ ] **Step 5: Implement Subscribe** — a dedicated `LISTEN` conn; on NOTIFY, fetch the row (or carry the row in the payload) and non-blocking-send to the subscriber channel (drop-newest-with-gap). Idempotent unsubscribe.

- [ ] **Step 6: Implement the LoopRegistry** — `loops` table upserts for `Register`/`SetLive`; `Resolve`/`List` with tenant-scoped WHERE; `ErrLoopUnknown` on no row.

- [ ] **Step 7: Commit** — `git commit -m "feat(event/pgstore): #47 Postgres durable store + registry + LISTEN/NOTIFY (build-tagged)"`

---

## Task 9: pgstore tests — testcontainers + shared conformance + pub/sub, skip-clean

**Files:**
- Create: `internal/event/pgstore/store_test.go` (`//go:build pgstore`)

**Interfaces:**
- Consumes: Task 2 `eventtest.RunDurableStoreConformance`, Task 8 `Open`.

- [ ] **Step 1: Container harness** — a `newPG(t)` helper that starts a testcontainers Postgres; if the container runtime is unavailable (`testcontainers` returns a provider/Docker error), `t.Skip("no container runtime")` — so even `-tags pgstore` never hard-fails on a Docker-less box.

- [ ] **Step 2: Run the shared conformance suite** against pgstore (`TestPGConformance` → `eventtest.RunDurableStoreConformance`).

- [ ] **Step 3: Cross-instance pub/sub test** — TWO `Open` handles over the SAME container (instance A appends, instance B `Subscribe`s): B receives A's events with **no gap and no dup**, and a `from_seq` reattach (subscribe-before-replay + dedup-at-watermark over pgstore) resumes correctly, with a concurrent append forced into the handoff gap.

- [ ] **Step 4: Non-blocking-Append-under-Postgres test** — fill/park a subscriber, burst appends, assert Append returns promptly (never back-pressured by NOTIFY delivery) and a gap is recorded.

- [ ] **Step 5: Tenant isolation test** — cross-tenant `Resolve`/`Replay` fails (`ErrLoopUnknown`/empty), never returns another tenant's stream.

- [ ] **Step 6: Run tagged** — `go test -tags pgstore -race ./internal/event/pgstore/...` (locally, Docker present) → PASS. Confirm untagged bare `go test ./...` still green.

- [ ] **Step 7: Commit** — `git commit -m "test(event/pgstore): #47 testcontainers conformance + cross-instance pub/sub + non-blocking + tenant isolation (skip-clean)"`

---

## Task 10: D5 observability hooks — assert presence, no emission

**Files:**
- Create/Modify: `internal/event/store_hooks_test.go`; docstrings on the durable ops.

**Interfaces:**
- Consumes: Tasks 1/3/8.

- [ ] **Step 1: Structural test** — assert (by construction / signature) that every durable-store op and the registry ops take a `context.Context`, and that the `(tenant_id, loop_id, node_id)` triple is available on telemetry-relevant ops in a consumer-readable form (e.g. `StreamKey` + `Event.NodeID`). Assert NO OTEL import in `internal/event` (`go list -deps` grep for `go.opentelemetry.io` returns nothing).

```go
func TestNoOTELDependency(t *testing.T) {
	out, _ := exec.Command("go", "list", "-deps", "./internal/event/...").Output()
	if bytes.Contains(out, []byte("go.opentelemetry.io")) {
		t.Fatal("internal/event must not import OTEL in 0047 (D5 is hooks-only)")
	}
}
```

- [ ] **Step 2: Run → PASS** (context already threaded; no OTEL present).

- [ ] **Step 3: Commit** — `git commit -m "test(event): #47 assert D5 hooks present (context+labels) and no OTEL emission"`

---

## Task 11: Full-suite gate + local/dev no-Postgres verification

- [ ] **Step 1: Bare suite** — `go test ./...` (NO tags, NO Postgres, NO Docker needed) → PASS. **Non-negotiable.**
- [ ] **Step 2: Race** — `go test -race ./...` → PASS.
- [ ] **Step 3: Tagged suite (local, Docker present)** — `go test -tags pgstore -race ./internal/event/pgstore/...` → PASS (or skip-clean if no Docker).
- [ ] **Step 4: Local-path smoke** — `fuse loop-server` starts against fsstore with no Postgres; cross-process reattach works via the durable fsstore registry (a fresh process resolves a prior loop_id). If a live-model turn is needed, use a NON-Anthropic model via `LLM_GATEWAY_URL` — keep it minimal (storage/concurrency are the load-bearing checks).
- [ ] **Step 5: Commit any fixes** — `git commit -m "chore: #47 full-suite + race + tagged gates green"`

---

## Self-Review

**Spec coverage:** D1 (Tasks 1-4, 8), D2 tenant (Tasks 1,3,4,6,7,9), D3 pub/sub (Tasks 8,9), D4 durable registry reconciling ADR-0030 (Tasks 4,5; ADR at Step 6), D5 hooks (Tasks 1,10). Verification section: cross-process replay (Task 5), cross-instance tail (Task 9), tenant isolation (Tasks 2,3,9), two-impl one-suite (Tasks 2,3,9), no-Postgres local (Tasks 7,11), -race (every task + Task 11), non-blocking under PG (Task 9), D5 hooks no emission (Task 10), ADR (Step 6 docket-adr).

**Placeholder scan:** backend method bodies are described by exact SQL/paths; the pgstore SQL specifics (advisory-lock keying) are given. No "TBD"/"handle edge cases".

**Type consistency:** `DurableStore`/`LoopRegistry`/`StreamKey`/`TenantID`/`LoopID`/`LoopRecord`/`ErrLoopUnknown`/`DefaultTenant`/`NormalizeTenant` are used consistently across Tasks 1→11; `Runtime.Observe`/`Attach` new signatures match between Tasks 5 and 6.
