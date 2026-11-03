<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0126 — Fenced loop ownership and cross-replica activity in the loop registry](../../changes/active/0126-fenced-loop-ownership-and-cross-replica-activity-in-the-loop.md)**
<!-- docket:backlink:end -->

# Fenced loop ownership and cross-replica activity in the loop registry

- Type: fix
- Priority: high

## Why

`event.LoopRegistry` (`internal/event/registry.go:43-64`) records `OwnerNodeID` and a `LeaseExpiry`, and `inProcRuntime` re-owns a loop whose lease has expired (`internal/runtime/inproc.go:1169`, `:641-652`, `reapIfAbandoned` `:1475-1487`). But neither backend fences the owner: `pgstore.Heartbeat` and `SetLive` are `UPDATE loops SET … WHERE tenant_id=$1 AND loop_id=$2` with no owner predicate (`internal/event/pgstore/store.go:515-556`), `Register` is an upsert that overwrites `owner_node_id` (`:490-513`), and fsstore mirrors that (`internal/event/fsstore/registry.go:90-192`). A replica paused past its lease (GC, network partition, a slow model call) wakes up, its renewer heartbeats, and the record now points back at it while another replica is already running the loop from a resume: two agents append to one stream and both accept Sends.

Separately, the idle-TTL reaper (`inproc.go:310-360`) is reset only by `touch()` on the owning process. `Observe` on a non-owner replica subscribes to the durable store and never touches (`inproc.go:1081-1095`); `Send`/`Steer` on a non-owner relay a `steer.request` (`relay.go:33-47`) and never touch either. A loop callers keep watching through a load balancer can therefore be idle-reaped.

## What changes

1. Registry contract (`internal/event/registry.go`):
   - `Heartbeat(ctx, key, ownerNodeID, expiry)` and `SetLive(ctx, key, live, ownerNodeID)` succeed only when the record's `OwnerNodeID == ownerNodeID` OR the record is not live OR its lease has expired. Otherwise they return the new sentinel `event.ErrNotOwner`.
   - New `Claim(ctx, key, ownerNodeID string, expiry time.Time) (LoopRecord, error)`: atomically sets `live=true, owner_node_id, lease_expiry` when the record is not live or its lease has expired; returns `ErrNotOwner` with the current record when another node holds a fresh lease. pgstore: one `UPDATE … WHERE … AND (NOT live OR lease_expiry < now()) RETURNING …`; fsstore: under the store mutex with the sidecar's tmp+rename write.
   - `Register` keeps upserting for a NEW key but must not overwrite the owner of a record that is live with a fresh lease; it returns `ErrNotOwner` in that case.
   - New `Touch(ctx, key) error` that advances `UpdatedAt` (and a new `LastActivity` field on `LoopRecord`, mapped to a new nullable `last_activity` column in pgstore via a migration, and a sidecar field in fsstore) without touching owner or lease.
2. Runtime (`internal/runtime/inproc.go`):
   - Resume uses `Claim` instead of `Heartbeat`+`SetLive` (`:641-652`); `ErrNotOwner` maps to the existing "live on another instance" `ErrLoopNotFound` path.
   - The lease renewer (`startLeaseRenewer`, `:286-308`) treats `ErrNotOwner` as fatal for that loop: it cancels the loop's session context, seals the sink so no further events are appended, drops the loop from the cache, and emits one `error` event with reason `ownership_lost` *before* sealing (the event is accepted only if the node still owns the record; otherwise it is dropped). `reapIfAbandoned` and `Shutdown` call `SetLive(false, ownerNodeID)` with the node's own id and ignore `ErrNotOwner`.
   - Non-owner activity: `Observe`, `Send`, `Steer` on a loop that is live elsewhere call `Registry.Touch` (best-effort, errors logged). The owner's idle reaper reads `LastActivity` from the registry on each tick (or at least every `IdleTTL/3`) and treats `max(localTouch, LastActivity)` as the activity time.
3. Conformance (`internal/event/eventtest/conformance.go`): new cases `ClaimExpiredLeaseSucceeds`, `ClaimFreshLeaseErrNotOwner`, `HeartbeatFromNonOwnerErrNotOwner`, `SetLiveFromNonOwnerErrNotOwner`, `RegisterDoesNotStealFreshLease`, `TouchAdvancesLastActivityOnly`. Both backends pass.
4. Two-runtime tests (`internal/runtime`): with a frozen `Deps.Now`, runtime A starts an interactive loop; the clock jumps past the lease; runtime B `Resume`s and takes ownership; A's renewer ticks → A seals its loop, A's `Send` returns `ErrLoopNotFound`/relays, and only B's agent appends to the stream (assert no `model.call.start` with A's run after B's claim). A second test: a loop live on A, Observe/Send only through B for longer than `IdleTTL`; the loop is NOT reaped.

## Out of scope

Deliberate live migration; wire changes; adjusting lease/idle defaults; the cancel races tracked by 0120.

## Acceptance criteria

1. `eventtest.RunDurableStoreConformance` contains the six new cases and passes for fsstore (default build) and pgstore (`-tags pgstore`).
2. `Heartbeat`/`SetLive` from a non-owner on a live, fresh-lease record return `event.ErrNotOwner` on both backends; `Claim` on an expired lease succeeds and on a fresh lease returns `ErrNotOwner`.
3. The two-runtime stale-owner test passes: after B claims, A appends nothing further and A's cache no longer holds the loop; the stream shows exactly one `error{reason:"ownership_lost"}` or none, never two live owners.
4. The two-runtime idle test passes: cross-replica Observe/Send keeps the loop alive past `IdleTTL`; with no activity it is still reaped at `IdleTTL`.
5. `cmd/fuse/loop_serve_net_twoinstance_test.go` and the existing resume/cancel suites are unchanged in outcome.
6. An ADR records: ownership is a fenced lease with CAS on the registry; a refused heartbeat ends the loop locally; activity is registry-visible.
