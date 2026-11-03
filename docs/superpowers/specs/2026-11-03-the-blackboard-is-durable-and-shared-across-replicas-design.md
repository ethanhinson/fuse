<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0131 — The blackboard is durable and shared across replicas](../../changes/active/0131-the-blackboard-is-durable-and-shared-across-replicas.md)**
<!-- docket:backlink:end -->

# The blackboard is durable and shared across replicas

- Type: feat
- Priority: high

## Why

`agent.Blackboard` (`internal/agent/blackboard.go:34-39`) is `entries map[string]BlackboardEntry` + `waiters` behind a mutex, built per loop (`cmd/fuse/loop_server.go:404`); `Put` (`:54-69`) emits nothing; there is no `blackboard.*` event kind; `Wait` (`:138-180`) blocks on an in-process channel. `Resume` builds a fresh empty board. `pipeline.Run` consumes the concrete type (`internal/pipeline/engine.go:73`). Under (b) the board is lost; under (c) a child elsewhere cannot see it.

## What changes

1. Events (`internal/event/event.go`): `KindBlackboardPut = "blackboard.put"` with `BlackboardPutPayload{Key, Value json.RawMessage, Writer string, Turn int, Version uint64}`; `KindBlackboardDelete = "blackboard.delete"` with `{Key, Writer, Version}`. Values are capped at 64 KiB inline; larger values are written to the artifact store (0128, `Kind:"blackboard"`) and the payload carries `Ref` instead of `Value`.
2. Seam (`internal/agent/blackboard.go`):
   ```go
   type BlackboardBackend interface {
       Put(ctx, key string, e BlackboardEntry) (version uint64, err error)
       Delete(ctx, key string) error
       Get(ctx, key string) (BlackboardEntry, bool, error)
       Keys(ctx, pattern string) ([]string, error)
       Watch(ctx, key string) (<-chan BlackboardEntry, func(), error) // fires on put/delete of key; fresh value first if present
   }
   ```
   `MemBackend` (today's behaviour) and `StreamBackend{store event.EventStore (the loop's sealable sink), key StreamKey, cache}`: `Put` appends the event through the loop's store and applies it to the cache on the store-assigned seq (via `CommittedDurableStore.AppendCommitted` when available); `Watch` subscribes to the loop stream and filters. `Blackboard` keeps its public API and delegates; `Wait` uses `Watch` and still yields the scheduler slot.
3. Resume fold (extends 0127's `loopState`): replay `blackboard.put/delete` in seq order into the cache; `Resume` constructs the `StreamBackend` pre-seeded.
4. Conformance `internal/agent/blackboardtest.RunBackendConformance(t, newBackend)`: put/get/delete round trip; version monotonic per key; `Watch` fires for a put after subscribe and delivers the current value first; two backends over one stream (StreamBackend only) see each other's puts within one relay poll; large value via ref round-trips.
5. Wiring: `BuildAgent` uses `StreamBackend` whenever the loop has a durable store; `MemBackend` for the no-store paths; `pipeline.Run` takes the `Blackboard` interface surface it needs (`Put/Get/Wait/Keys`), not the struct.
6. Tests: two-runtime test — A's loop puts `plan` and parks; A is cut; B resumes and `blackboard_get("plan")` returns it; then a direct `StreamBackend` on B puts `result` while A's (not yet fenced) board is watching — A's `Wait("result")` wakes.

## Out of scope

Cross-loop or tenant-wide boards; schema changes to blackboard tools; TUI changes beyond reading the cache.

## Acceptance criteria

1. Conformance passes for `MemBackend` and `StreamBackend` over fsstore and pgstore (tagged).
2. Every `blackboard_put` by a hosted loop appears on its stream; a resumed loop reads every key put before the move (two-runtime test).
3. `blackboard_wait` on one runtime wakes on a put made through the stream by another process.
4. Values > 64 KiB are stored as artifacts and read back transparently.
5. Existing blackboard, pipeline, and workflow test suites pass unchanged in outcome.
6. ADR: the blackboard is event-sourced on the loop stream; the map is a cache.
