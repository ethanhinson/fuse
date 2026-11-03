<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0126 — Fenced loop ownership and cross-replica activity in the loop registry](../../changes/active/0126-fenced-loop-ownership-and-cross-replica-activity-in-the-loop.md)**
<!-- docket:backlink:end -->

# Fenced loop ownership and cross-replica activity in the loop registry - Implementation Plan

> Executed by `docket-build`: one task at a time. Each task is TDD (failing test, minimal code, passing test) and ends in one commit. Every test uses fakes: the `fsstore` durable store in a temp dir, in-memory registry doubles, scripted/blocking completers. pgstore runs only under `-tags pgstore` against its existing container harness, which skips cleanly without Docker. There is no live model traffic and no Anthropic traffic.

> The plan writer authored this plan inline. `superpowers:writing-plans` is not available in this environment (missing-skill rule).

**Goal:** Loop ownership becomes a fenced lease. A heartbeat or liveness flip from a node that is not the recorded owner of a live, fresh-leased record is refused with `event.ErrNotOwner`. Resume takes an expired or finished loop with one atomic `Claim`. A runtime whose heartbeat is refused stops driving that loop immediately: it seals the sink, drops the loop from its cache and cancels the run. Activity from any replica (Observe, Send, Steer) is recorded on the registry (`Touch` -> `LastActivity`), and the owner's idle reaper counts it.

**Architecture:**
- `internal/event/registry.go`: `ErrNotOwner`; `LoopRecord.LastActivity`; `LoopRecord.HeldByOther(nodeID, now)` (the one shared fence predicate); `LoopRegistry.Claim` and `LoopRegistry.Touch`; fence semantics documented on `Register`/`SetLive`/`Heartbeat`.
- `internal/event/event.go`: `ErrorPayload.Reason` (`json:"reason,omitempty"`, additive JSON, no proto change) and `ErrorReasonOwnershipLost = "ownership_lost"`.
- `internal/event/fsstore/registry.go`: fence checks under `s.mu`, `Claim`, `Touch`, and a `last_activity` field in the sidecar.
- `internal/event/pgstore`: the fence as a SQL predicate on each `UPDATE`/upsert, `Claim` as one conditional `UPDATE … RETURNING`, `Touch`, and an idempotent `last_activity` column migration in `schema.sql`.
- `internal/event/eventtest/conformance.go`: six new cases that both backends pass.
- `internal/runtime/inproc.go`: a per-runtime node id (`Deps.NodeID`, minted in `New` when empty). Resume uses `Claim`. The lease renewer fences the loop on `ErrNotOwner`. Observe/Send/Steer call `Touch` on a loop that is live elsewhere. The idle reaper consults `LastActivity` before reaping.
- Two-runtime tests in `internal/runtime/fence_twoinstance_test.go`.

**Tech stack:** Go, pgx v5 (tagged), fsstore JSON sidecars.

**Verified anchors (feature tree at `c3cfb60`):**
- `registry.go:43-64` interface.
- fsstore `registry.go:90-183` (`Register`/`SetLive`/`Heartbeat`).
- pgstore `store.go:490-556` (`Register`/`SetLive`/`Heartbeat`), `559-641` (`Resolve`/`List`), `schema.sql` (`loops` table plus `ALTER … ADD COLUMN IF NOT EXISTS` migrations).
- `inproc.go:286-308` `startLeaseRenewer`, `317-356` `startIdleReaper`, `531-541` `failLaunch`, `636-659` register/resume, `851-877` renewer/relay/reaper start, `967-972` completion `SetLive(false)`, `1081-1096` `Observe`, `1141-1218` `Resume`, `1245-1322` `Send`/`Steer`/`liveLoop`, `1475-1484` `reapIfAbandoned`.
- `cancel.go:704-748` `sealableSink`.
- `relay.go:60-76` `relayRequest`.

**Facts the spec did not state, found while planning (they drive the decisions below):**
1. **Today every replica records the LOOP id as `OwnerNodeID`.** `inproc.go` passes `rootID` to `Register`, `Heartbeat`, `SetLive` and the renewer. Two replicas driving one loop therefore present the same "node id", and an owner-equality fence could never tell them apart. A real per-runtime node id is a prerequisite (Task 3).
2. **Existing two-replica tests skew only replica B's runtime clock** (`relay_test.go` `TestSendToAnAbandonedLoopReportsFinished`, `TestObserveDoesNotRevivePhantomLease`; `cancel_relay_unanswered_test.go` `TestRelayedCancelOwnerDiesWhileWaiting`). The fsstore backend still runs on the wall clock. In those tests B reaps A's "expired" lease with `SetLive(false, rec.OwnerNodeID)`. If B passed its own id, a backend fence on the wall clock would refuse the reap and those suites would change outcome (acceptance 5).
3. **Several registries implement `LoopRegistry` and must gain `Claim`/`Touch`:** fsstore, pgstore, `internal/runtime/lease_test.go` `fakeRegistry`, `internal/runtime/session_cancel_leak_test.go` `sessionCapturingStore`, and `internal/loopconnect/handler_authz_test.go` `fakeRegistry`. `cmd/fuse/loop_serve_net_limits_test.go` `listOnlyRegistry` embeds the interface and needs nothing.
4. **The edge's `recordOwner` (`internal/loopconnect/handler.go:426`) does Resolve-then-Register on the node that just started the loop.** The re-Register therefore carries the current owner and passes the fence. `Register` must preserve `LastActivity`.

**Decisions made in this plan (put them in the PR body):**
1. **Fence predicate, one definition.** A record is *held by another node* when `Live && !LeaseExpiry.IsZero() && !now.After(LeaseExpiry) && OwnerNodeID != nodeID`. A zero lease is "no lease held", so it never fences. That keeps the existing lease-free conformance/registry tests (`RegistryRoundTripLivenessOwnership`, `TestFSLoopRegistryRoundTrip` flip `SetLive(false, "nodeB")` on a lease-free nodeA record) unchanged. `Register`, `SetLive`, `Heartbeat` and `Claim` return `ErrNotOwner` exactly when the record is held by another node. `Claim` by the current holder succeeds (a re-claim).
2. **The fence compares against the backend's clock.** fsstore uses `nowUTC()`. pgstore passes Go `time.Now().UTC()` as a parameter, the same instant it already writes into `updated_at`, rather than SQL `now()`. Both backends then share one notion of "now", and the conformance cases can use wall-clock-relative leases on both. This is a deliberate re-map of the spec's `lease_expiry < now()`. The decision is unchanged.
3. **Per-runtime node id.** `Deps.NodeID` (optional). `New` mints `node-<16 hex>` from `crypto/rand` when it is empty, so every composition root that calls `runtime.New` gets distinct ids with no wiring (learning `security-knob-inert-at-composition-root`). The node id replaces `rootID` everywhere it is used as `OwnerNodeID`.
4. **Reap is a compare-and-set on the owner it saw expire.** `reapIfAbandoned` keeps `SetLive(false, rec.OwnerNodeID)`, which flips the record only if it is still owned by the node whose lease this replica judged expired. If another node claimed it in between, the backend refuses with `ErrNotOwner`. The reap then re-resolves and returns the current record (no error). This deviates from the spec's "with the node's own id" for the reap path only: passing the observed owner is the CAS, and it keeps fact 2's suites unchanged. The safety property holds because Claim plus the renewer fence still stop a stale owner. The completion path and `failLaunch` use the runtime's own node id and ignore `ErrNotOwner`. `Shutdown` reaches `SetLive` only through the completion path, so it inherits this behavior.
5. **`Heartbeat` on a not-live record is allowed** (spec rule "OR the record is not live"). It renews the lease and owner but leaves `Live` false. `Claim` on a not-live record always succeeds, so a stray heartbeat between a reap and a claim cannot block the claim.
6. **Losing ownership (`loseOwnership`).** When the renewer's `Heartbeat` returns `ErrNotOwner`:
   - **(a) seal and conditional event.** Inside one `lp.sink.sealWith` it re-resolves the record and appends one `error{err:"ownership lost", reason:"ownership_lost"}` straight to the store, but only when the record still names this node as live owner. Otherwise the event is dropped. Either way the sink is sealed.
   - **(b) drop from cache.** It deletes the loop from `r.loops` (only if the entry is still this `*loop`).
   - **(c) end the run.** It fires `lp.abort(errOwnershipLost)` and, for an interactive loop, `lp.cancel()`. The session cancel also stops the relay pump and the idle reaper.

   Then the renewer goroutine returns. The run's completion path still runs (`SetLive(false, nodeID)` gets `ErrNotOwner` and is ignored; the store closes; `LoopTeardown` runs). In practice the conditional append drops the event, because a refused heartbeat means another node owns the record. Acceptance 3 allows exactly one such event or none.
7. **`errOwnershipLost`** is an unexported cause: `fmt.Errorf("%w: ownership lost to another replica", ErrLoopNotFound)`. It is never a `cancelState`, so no `loop.cancelled` is written and the loop stays resumable by its new owner.
8. **Non-owner activity is best-effort.** `Observe` (only when the resolved record is live), `Send` and `Steer` (on `errLiveElsewhere`, before relaying) call `r.touchRemote(ctx, key)`. Errors are ignored, following the runtime's existing `_ =` convention for best-effort registry writes; the runtime has no logger. A cache hit keeps the existing local `lp.touch()` only.
9. **The idle reaper reads `LastActivity` when its timer fires, not on a poll.** On fire, with a registry present, it calls `Resolve`. If `LastActivity + IdleTTL` is still in the future, it re-arms the timer for the remaining time and continues. Otherwise it reaps. This is exactly `max(localTouch, LastActivity)` and costs one registry read per idle window, not one per tick. A `Resolve` error falls back to local activity only (reap), which is the pre-change behavior. The reaper does its activity math on the wall clock (`time.Now`), matching both its real timer and the backend's `LastActivity` stamp. `Deps.Now` keeps governing only lease math, as its doc says.
10. **No wire change.** `ErrorPayload.Reason` is an additive `omitempty` JSON field inside an event payload. No proto or generated code changes, so `make proto` is not needed.

**Learnings applied:**
- `parity-test-feeds-each-side-its-own-production-source`: the six cases live in the shared `eventtest` suite. Each backend feeds its own real constructor (`TestFSStoreConformance`, `TestPGConformance`).
- `per-instance-resource-needs-teardown-on-every-early-return`: a refused `Claim` on resume is a new early-return path. It must go through `failLaunch()`, which releases the session context. Task 3 extends `TestResume_SessionCtxCanceledOnRegistryErrors` to cover it.
- `race-invisible-to-race-detector-without-concurrent-test`: the fence fires on the renewer goroutine while the run goroutine and relay pump are live. The Task 4 and Task 7 tests drive exactly that concurrency and must pass under `-race`.
- `mutex-test-double-concurrent-provider`: registry doubles and counting completers read by runtime goroutines and by the test are mutex- or atomic-guarded on both sides.
- `security-knob-inert-at-composition-root`: the fence only works if each process has its own node id. The default is minted inside `runtime.New` (not in `cmd/fuse`), and the Task 7 idle test runs with `NodeID` unset to exercise the default path across two runtimes.
- `reconcile-verify-claims-against-origin-not-working-tree`: the anchors above were checked on the feature tree at `c3cfb60`, which is `origin/main` 5eb0925 plus the spec commit.

**ADR:** the coordinator records it. No build task writes it.

---

## Task 1: Registry contract, fsstore implementation, conformance cases

**Tier:** premium (contract change every backend and double must honour)

**Files:**
- Modify: `internal/event/registry.go`
- Modify: `internal/event/event.go` (`ErrorPayload.Reason`, `ErrorReasonOwnershipLost`)
- Modify: `internal/event/fsstore/registry.go`
- Modify: `internal/event/eventtest/conformance.go`
- Modify (compile only): `internal/runtime/lease_test.go`, `internal/runtime/session_cancel_leak_test.go`, `internal/loopconnect/handler_authz_test.go`
- Test: `internal/event/event_test.go`, `internal/event/fsstore/registry_test.go` (existing `TestFSStoreConformance` runs the new cases)

**Step 1: Write the failing conformance cases** in `RunDurableStoreConformance`. Use `errors.Is` for every sentinel check, and truncate lease instants to the millisecond like the existing `HeartbeatAdvancesLeaseLeavesOwnerAndLive`. `fresh := time.Now().UTC().Add(time.Hour).Truncate(time.Millisecond)`, `expired := time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond)`.
- `ClaimExpiredLeaseSucceeds`:
  - Register `{nodeA, Owner:"alice", Live:true, LeaseExpiry:expired}`. `Claim(key, "nodeB", fresh)` returns nil, and the returned and resolved records both have `Live`, `OwnerNodeID=="nodeB"`, `LeaseExpiry.Equal(fresh)`, `Owner=="alice"`.
  - A second key registered `Live:false` with a fresh lease by nodeA is also claimable by nodeB (not live ⇒ claimable).
- `ClaimFreshLeaseErrNotOwner`:
  - Register `{nodeA, Live:true, LeaseExpiry:fresh}`. `Claim(key,"nodeB",fresh)` returns `ErrNotOwner`, and the returned record has `OwnerNodeID=="nodeA"`. Resolve is unchanged.
  - `Claim(key,"nodeA",fresh.Add(time.Minute))` (the holder re-claims) succeeds.
  - `Claim` on an unregistered key returns `ErrLoopUnknown`.
- `HeartbeatFromNonOwnerErrNotOwner`:
  - nodeA live with a fresh lease. `Heartbeat(key,"nodeB",fresh2)` returns `ErrNotOwner`, and Resolve still shows nodeA and the original lease.
  - On a second key where nodeA's lease is `expired`, `Heartbeat(key,"nodeB",fresh)` succeeds and records nodeB.
- `SetLiveFromNonOwnerErrNotOwner`: nodeA live with a fresh lease. `SetLive(key,false,"nodeB")` returns `ErrNotOwner` and the record is still live. `SetLive(key,false,"nodeA")` returns nil and the record is not live.
- `RegisterDoesNotStealFreshLease`:
  - nodeA live with a fresh lease. `Register({key,nodeB,Live:true,LeaseExpiry:fresh})` returns `ErrNotOwner`, and Resolve shows nodeA.
  - `Register` of a brand-new key by nodeB returns nil.
  - Re-`Register` by nodeA with `Owner:"bob"` returns nil, with `Owner=="bob"` and `CreatedAt` preserved.
- `TouchAdvancesLastActivityOnly`:
  - nodeA live with a fresh lease, `Owner:"alice"`. Take `before := Resolve`, sleep 2ms, then `Touch(key)` returns nil.
  - `after.LastActivity` is non-zero and `!after.LastActivity.Before(before.UpdatedAt)`. `after.UpdatedAt` has not regressed. `OwnerNodeID`, `Owner`, `Live` and `LeaseExpiry` are unchanged.
  - A following `Heartbeat(key,"nodeA",fresh)` preserves `LastActivity`.
  - `Touch` on an unregistered key returns `ErrLoopUnknown`.

Also add to `internal/event/event_test.go`: `TestErrorPayloadReasonJSON`. `ErrorPayload{Err:"x",Turn:1}` marshals to `{"err":"x","turn":1}` (no `reason` key). With `Reason: ErrorReasonOwnershipLost` it contains `"reason":"ownership_lost"`.

**Step 2:** `go test ./internal/event/... -count=1` fails to compile (undefined `Claim`, `Touch`, `ErrNotOwner`, `LastActivity`).

**Step 3: Implement the contract** in `internal/event/registry.go`:
```go
// ErrNotOwner is returned by a LoopRegistry write that would take or renew ownership
// of a loop another node holds: the record is live, carries a non-zero lease that has
// not expired, and names a different OwnerNodeID. It fences a stale owner (a replica
// that stalled past its lease while another resumed the loop) out of the record.
var ErrNotOwner = errors.New("event: loop is owned by another node")
```
Add `LastActivity time.Time` to `LoopRecord`, with this doc: the last activity any replica recorded through `Touch` (Observe/Send/Steer relayed from a non-owner). The zero value means none. The owner's idle reaper counts it. Then add the shared predicate:
```go
// HeldByOther reports whether a node other than nodeID holds r's lease at now: r is
// live, has a non-zero lease that has not expired, and names a different owner. A
// zero lease is no lease, so it never fences. Every backend applies this one rule.
func (r LoopRecord) HeldByOther(nodeID string, now time.Time) bool {
	return r.Live && !r.LeaseExpiry.IsZero() && !now.After(r.LeaseExpiry) && r.OwnerNodeID != nodeID
}
```
Update the interface docs:
- `Register`: an upsert that returns `ErrNotOwner` when the existing record is `HeldByOther(rec.OwnerNodeID, now)`, and preserves `CreatedAt` and `LastActivity`.
- `SetLive` and `Heartbeat`: return `ErrNotOwner` under the same rule for `ownerNodeID`, and preserve `LastActivity`.

Then add:
```go
// Claim atomically takes ownership of a loop: it sets Live=true, OwnerNodeID and
// LeaseExpiry when the record is not held by another node (not live, no lease, an
// expired lease, or already owned by ownerNodeID) and returns the record as written.
// When another node holds a fresh lease it returns ErrNotOwner with the current record;
// an unregistered key is ErrLoopUnknown. Owner, CreatedAt and LastActivity are preserved.
Claim(ctx context.Context, key StreamKey, ownerNodeID string, expiry time.Time) (LoopRecord, error)

// Touch records activity on a loop from any replica: it advances LastActivity and
// UpdatedAt to now and changes nothing else (owner, liveness and lease are untouched).
// It returns ErrLoopUnknown for an unregistered key.
Touch(ctx context.Context, key StreamKey) error
```
In `event.go`, add `Reason string \`json:"reason,omitempty"\`` to `ErrorPayload` and `const ErrorReasonOwnershipLost = "ownership_lost"`. The doc comment says it is set on the error event a runtime may write when its lease renewal is refused.

**Step 4: Implement fsstore** (`internal/event/fsstore/registry.go`):
- Add `LastActivity time.Time \`json:"last_activity"\`` to `loopSidecar`. Old sidecars decode to zero.
- Add a helper `func (sc loopSidecar) record(k event.StreamKey) event.LoopRecord` and use it in `Resolve`, `List` and `Claim`, so `LastActivity` is mapped once.
- `Register`: under `s.mu`, if a previous sidecar exists and `prev.record(key).HeldByOther(rec.OwnerNodeID, now)`, return `event.ErrNotOwner`. Preserve `prev.CreatedAt` and `prev.LastActivity`.
- `SetLive` / `Heartbeat`: after the unknown check, refuse with `ErrNotOwner` when `HeldByOther(ownerNodeID, nowUTC())`.
- `Claim`: under `s.mu`, read the sidecar (absent: `ErrLoopUnknown`). If held by another node, return `(prev.record(key), ErrNotOwner)`. Otherwise set `Live=true`, `OwnerNodeID`, `LeaseExpiry=expiry` and `UpdatedAt=now`, write with `writeSidecar` (tmp+rename), and return the record.
- `Touch`: under `s.mu`, read the sidecar (absent: `ErrLoopUnknown`), set `LastActivity = UpdatedAt = nowUTC()` and write it.

**Step 5: Update the doubles so the module compiles:**
- `lease_test.go` `fakeRegistry`: add `Claim` (permissive: sets Live/owner/lease, counts `claims`, returns `claimErr` when set; the field is guarded by `f.mu`) and `Touch` (sets `LastActivity=time.Now()`, counts `touches`; `ErrLoopUnknown` for a missing key). Add `claimCount()` and `touchCount()` accessors under `f.mu`.
- `session_cancel_leak_test.go` `sessionCapturingStore`: add `Claim` honoring a new `claimErr` field, and a permissive `Touch`.
- `loopconnect/handler_authz_test.go` `fakeRegistry`: add permissive `Claim` and `Touch`.

These doubles stay non-fencing. The fence is the backends' job, and the conformance suite proves it.

**Step 6:** `go test ./internal/event/... ./internal/runtime/... ./internal/loopconnect/... -count=1` passes, and `go build ./...` passes. `go vet -tags pgstore ./internal/event/pgstore/` is expected to fail until Task 2 (the package is tag-gated, so the default build and suite stay green).

**Step 7: Commit** `feat(event): fenced loop registry contract - ErrNotOwner, Claim, Touch, LastActivity (fsstore)`.

---

## Task 2: pgstore fence, Claim, Touch, `last_activity` migration

**Tier:** standard

**Files:**
- Modify: `internal/event/pgstore/store.go`
- Modify: `internal/event/pgstore/schema.sql`
- Test: `internal/event/pgstore/store_test.go` (existing `TestPGConformance` runs the six new cases)

**Step 1:** `go vet -tags pgstore ./internal/event/pgstore/` fails (`*PGStore` does not implement `LoopRegistry`). That is the red state. The behavioral red/green is the conformance suite under `-tags pgstore`.

**Step 2: Schema.** Add `last_activity timestamptz` to the `CREATE TABLE loops` column list, and append an idempotent migration block in the existing style:
```sql
-- Idempotent migration for change 0126 (fenced ownership): last cross-replica activity
-- recorded by Touch; NULL means none. The owner's idle reaper counts it.
ALTER TABLE loops ADD COLUMN IF NOT EXISTS last_activity timestamptz;
```

**Step 3: Implement** (`$now` is always `time.Now().UTC()` computed once per call; see decision 2). The fence clause, written once as a Go string constant and reused:
```go
// fenceOpen is the SQL form of event.LoopRecord.HeldByOther negated: the write may
// proceed when the caller already owns the record, it is not live, it holds no lease,
// or its lease has expired. $owner and $now are the call's parameters.
```
- `Register`: `INSERT … ON CONFLICT (tenant_id, loop_id) DO UPDATE SET owner_node_id=…, owner=…, live=…, lease_expiry=…, updated_at=… WHERE loops.owner_node_id = EXCLUDED.owner_node_id OR NOT loops.live OR loops.lease_expiry IS NULL OR loops.lease_expiry < $7`. A conflicting row that fails the `WHERE` affects 0 rows, so `RowsAffected()==0` returns `event.ErrNotOwner`. `last_activity` and `created_at` are not in the `SET` list, so they are preserved.
- `SetLive` / `Heartbeat`: append `AND (owner_node_id=$4 OR NOT live OR lease_expiry IS NULL OR lease_expiry < $5)` to the existing `WHERE`. On 0 rows, call a helper `s.missOrNotOwner(ctx, nk)`, which runs `SELECT EXISTS(SELECT 1 FROM loops WHERE tenant_id=$1 AND loop_id=$2)` and returns `ErrLoopUnknown` when the row is absent, else `ErrNotOwner`.
- `Claim`: `UPDATE loops SET live=true, owner_node_id=$3, lease_expiry=$4, updated_at=$5 WHERE tenant_id=$1 AND loop_id=$2 AND (owner_node_id=$3 OR NOT live OR lease_expiry IS NULL OR lease_expiry < $5) RETURNING owner_node_id, owner, live, lease_expiry, created_at, updated_at, last_activity`. On `pgx.ErrNoRows`, `Resolve` the key and return `(rec, ErrNotOwner)`. If `Resolve` returns `ErrLoopUnknown`, return that instead.
- `Touch`: `UPDATE loops SET last_activity=$3, updated_at=$3 WHERE tenant_id=$1 AND loop_id=$2`. 0 rows returns `ErrLoopUnknown`.
- `Resolve` / `List`: select `last_activity` as `*time.Time` and map it with `timeFromNullable` into `LastActivity`. Factor a `scanLoop`-style helper if it removes the duplication between `Resolve`, `List` and `Claim`.
- Return the sentinels unwrapped (as `ErrLoopUnknown` is today), so `errors.Is` and `==` both work.

**Step 4:**
- `go vet -tags pgstore ./...` passes.
- `go test -tags pgstore ./internal/event/pgstore/ -count=1` passes, or skips cleanly when no container is available. If it skips, say so in the task report; do not claim it ran.
- `go test ./internal/event/... -count=1` still passes.

**Step 5: Commit** `feat(pgstore): fenced ownership, Claim, Touch and last_activity column`.

---

## Task 3: Runtime node identity; resume claims; reap is a CAS on the observed owner

**Tier:** premium

**Files:**
- Modify: `internal/runtime/inproc.go`
- Modify: `internal/runtime/session_cancel_leak_test.go` (resume registry-error table)
- Create: `internal/runtime/fence_test.go`

**Step 1: Write failing tests** in `internal/runtime/fence_test.go`:
- `TestRuntimesMintDistinctNodeIDs`: `New(Deps{})` twice. The two `.(*inProcRuntime).nodeID` values are non-empty and different. `New(Deps{NodeID:"node-x"})` yields `"node-x"`.
- `TestRegistryRecordsRuntimeNodeIDNotLoopID`:
  - Use an fsstore durable store (`fsstore.NewDurableFSStore(t.TempDir())` as both `DurableStore` and `Registry`), `NodeID:"node-a"`, an interactive loop, and a scripted completer as in `relay_test.go`'s `twoReplicasWith`.
  - Once the loop parks (`eventually` on a `loop.parked` in `streamOf`), Resolve shows `OwnerNodeID=="node-a"` (not the loop id) and `Live`.
- `TestResumeClaimRefusedIsLiveElsewhere`:
  - Wrap the fsstore in `claimRefusingRegistry{*fsstore.FSDurableStore}`, whose `Claim` returns `(rec, event.ErrNotOwner)`. This simulates another node winning the race between Resume's Resolve and its Claim.
  - Seed a resumable not-live loop: start it on a first runtime with the plain store, wait for park, then `Shutdown` that runtime.
  - Resume on a second runtime built with the wrapper. `errors.Is(err, ErrLoopNotFound)` holds, the message contains "live on another instance", and the loop is not in that runtime's cache.
- `TestReapCASLosesToConcurrentClaim`:
  - `fakeRegistry` seeded with a live record owned by "dead" whose lease expired under the runtime's frozen clock. Wrap it so `SetLive` returns `event.ErrNotOwner` and `Resolve` returns, on its second call, a record owned by "winner", live, with a fresh lease. Both are guarded by a mutex.
  - `rt.resolveDurable` returns no error and the "winner" record (`Live`).
- Extend `TestResume_SessionCtxCanceledOnRegistryErrors`:
  - Replace the `heartbeat` and `setlive` cases (those resume calls no longer exist) with `claim` (`s.claimErr = errors.New("claim boom")`, path "resume claim error") and `claim-not-owner` (`s.claimErr = event.ErrNotOwner`, path "resume claim not owner").
  - In both, the session ctx is canceled. For `claim-not-owner`, also assert `errors.Is(err, ErrLoopNotFound)`.
  - Update the fixture comment at the top of `sessionCapturingStore` to match.

**Step 2:** `go test ./internal/runtime -run 'NodeID|ClaimRefused|ReapCAS|SessionCtxCanceledOnRegistryErrors' -count=1` fails.

**Step 3: Implement** in `internal/runtime/inproc.go`:
- Add the `Deps.NodeID string` field. Doc: the identity this runtime records as `OwnerNodeID` on every loop it runs, unique per process. Empty means `New` mints one. A binding sets it only to pin a stable id (tests).
- Add a `nodeID string` field on `inProcRuntime`, set in `New`. Add `func newNodeID() string`: 8 bytes from `crypto/rand`, hex-encoded with prefix `node-`. If `rand.Read` fails, fall back to `fmt.Sprintf("node-%d-%d", os.Getpid(), time.Now().UnixNano())`.
- Replace every `rootID` used as an owner id with `r.nodeID`:
  - the `Register` `LoopRecord.OwnerNodeID` (line ~654);
  - `liveOwner` (~658);
  - the renewer argument (~854);
  - the completion `SetLive(false, …)` (~971).

  The completion call keeps its `_ =`. Add a comment there: `ErrNotOwner` means another replica took the loop over, so leaving its record alone is correct.
- Resume branch (~641-653): replace `Heartbeat`+`SetLive` with
  ```go
  if _, err := r.deps.Registry.Claim(ctx, key, r.nodeID, leaseExpiry); err != nil {
  	failLaunch()
  	if errors.Is(err, event.ErrNotOwner) {
  		return nil, fmt.Errorf("%w: %q (live on another instance)", ErrLoopNotFound, rootID)
  	}
  	return nil, fmt.Errorf("runtime: resume claim: %w", err)
  }
  ```
  Rewrite the comment: Claim is the one atomic take-over. It refuses when another node holds a fresh lease, which closes the window between Resume's Resolve and the launch.
- `reapIfAbandoned`: keep `SetLive(ctx, key, false, rec.OwnerNodeID)`. On `errors.Is(err, event.ErrNotOwner)`, re-`Resolve` and return that record with nil error (fall back to `rec` if the re-resolve fails). Update the doc comment with decision 4.

**Step 4:** `go test ./internal/runtime -count=1` passes, the whole package, including the relay, cancel, resume and lease suites, unchanged. `go test ./internal/loopconnect/... ./cmd/fuse -run 'TwoInstance|Durable|Resume' -count=1` passes.

**Step 5: Commit** `feat(runtime): per-runtime node id as loop owner; resume claims the lease atomically`.

---

## Task 4: The lease renewer fences a loop whose heartbeat is refused

**Tier:** premium (concurrency on a live run)

**Files:**
- Modify: `internal/runtime/inproc.go` (`startLeaseRenewer`, launch wiring)
- Create: `internal/runtime/fence_ownership.go` (`errOwnershipLost`, `loseOwnership`)
- Modify: `internal/runtime/fence_test.go`

**Step 1: Write failing tests** in `fence_test.go`. Every test here must also pass under `-race`. Add a helper `countErrorReason(evs, reason) int` that decodes `KindError` payloads into `event.ErrorPayload`.
- `TestRefusedHeartbeatFencesInteractiveLoop`:
  - Build a runtime over an fsstore durable store with `NodeID:"node-a"`, `LeaseTTL: time.Second`, `HeartbeatInterval: 10*time.Millisecond`, `IdleTTL: time.Hour` and a scripted completer. Start an interactive loop and wait for park.
  - Grab `lp := rti.loopCache(event.DefaultTenant, id)` (non-nil).
  - Steal the record the way a resuming replica would: `store.SetLive(ctx,key,false,"node-a")`, then `store.Claim(ctx,key,"thief",time.Now().Add(time.Hour))`.
  - Then assert:
    - `eventually` `rti.loopCache(...) == nil`.
    - `h.Wait()` returns within 3s; run it in a goroutine and fail on timeout.
    - Resolve shows `OwnerNodeID=="thief"` and `Live` (the stale owner's completion did not flip it).
    - `lp.sink.Append(event.Event{Kind: event.KindTurnStart})` returns an error wrapping `ErrLoopCancelled` (the sink is sealed).
    - `countErrorReason(streamOf(…), "ownership_lost") <= 1`.
    - `rt.Send(ctx, tenant, id, "x")` on this runtime returns nil, because it now relays: the record is live elsewhere. Afterwards the stream holds a `steer.request`.
- `TestRefusedHeartbeatFencesOneShotLoopMidCall`:
  - Same store; a non-interactive `StartLoop` whose completer is `cancelAwareCompleter` (it blocks until ctx is done).
  - After the first heartbeat, steal as above.
  - `h.Wait()` returns within 3s with an error (the abort reached the model call), and the loop is not in the cache.
- `TestOwnershipLostEventWrittenOnlyWhileStillOwner`:
  - Wrap the fsstore in `refusingHeartbeatRegistry`, which embeds `*fsstore.FSDurableStore` and whose `Heartbeat` returns `event.ErrNotOwner` once an atomic flag is set. `Resolve` is untouched, so the record still names node-a as live owner.
  - Start an interactive loop, park, set the flag, and `eventually` the loop leaves the cache.
  - The stream holds exactly one `error` event with `Reason=="ownership_lost"`, and it is the highest seq the loop wrote.

**Step 2:** `go test ./internal/runtime -run 'Fences|OwnershipLost' -count=1` fails.

**Step 3: Implement.**
- `startLeaseRenewer(ctx, key, ownerNodeID, stop, onLost func())`: in the tick branch,
  ```go
  err := r.deps.Registry.Heartbeat(context.Background(), key, ownerNodeID, r.now().Add(ttl))
  if errors.Is(err, event.ErrNotOwner) {
  	// Another replica owns this loop now: stop driving it, and stop renewing.
  	if onLost != nil {
  		onLost()
  	}
  	return
  }
  ```
  Other errors stay best-effort, as today.
- In `launchLoop` (~854): `r.startLeaseRenewer(sessionCtx, key, r.nodeID, stopRenew, func() { r.loseOwnership(lp) })`. The renewer already starts after `lp` exists.
- `fence_ownership.go`:
  ```go
  // errOwnershipLost is the cause a loop's run is cancelled with when its lease
  // renewal is refused: another replica claimed the loop, so this one must stop
  // appending and accepting input. It is not a Cancel, so no loop.cancelled is
  // written and the new owner keeps running the loop.
  var errOwnershipLost = fmt.Errorf("%w: ownership lost to another replica", ErrLoopNotFound)

  // loseOwnership fences a loop this runtime no longer owns (decision 6): seal the
  // sink (writing one ownership_lost error event only if the record still names this
  // node as live owner), drop the loop from the cache, then end its run.
  func (r *inProcRuntime) loseOwnership(lp *loop) { ... }
  ```
  - **Seal.** `lp.sink.sealWith(func() bool { …; return true })`. Inside it, `rec, err := r.deps.Registry.Resolve(context.Background(), lp.key)`. If `err == nil && rec.Live && rec.OwnerNodeID == r.nodeID`, build `event.Event{NodeID: lp.id, Kind: event.KindError, Payload: marshal(event.ErrorPayload{Err: "ownership lost", Reason: event.ErrorReasonOwnershipLost})}`. Append it with `r.deps.DurableStore.Append(context.Background(), lp.key, ev)` when a durable store is set, else with `lp.sink.EventStore.Append(ev)`. Never append through `lp.sink` itself.
  - **Drop from cache.** Under `r.mu`, `if r.loops[lp.id] == lp { delete(r.loops, lp.id) }`.
  - **End the run.** Call `lp.abort(errOwnershipLost)`, then `lp.cancel()` if it is non-nil.
- Check `cancel.go`'s callers of the cache (`isCancelled`, the local cancel) still behave for a loop that has left the cache: a Cancel then takes the not-in-cache path and relays to the new owner. That path is already correct; no edit is expected.

**Step 4:** `go test ./internal/runtime -count=1` and `go test -race ./internal/runtime -run 'Fence|Lease|Relay|Resume|Cancel' -count=1` pass.

**Step 5: Commit** `feat(runtime): a refused lease renewal fences the loop - seal, uncache, cancel`.

---

## Task 5: Non-owner Observe, Send and Steer record activity with `Touch`

**Tier:** standard

**Files:**
- Modify: `internal/runtime/inproc.go` (`Observe`, `Send`, `Steer`, new `touchRemote`)
- Modify: `internal/runtime/fence_test.go`

**Step 1: Write failing tests:**
- `TestNonOwnerActivityTouchesRegistry`:
  - Use `twoReplicas(t, f, nil)` from `relay_test.go`. A starts an interactive loop; wait for park. Read `rec0 := store.Resolve` (`LastActivity` is zero).
  - On B: `Observe` (unsubscribe right away). `eventually` `LastActivity` is non-zero. Record it as `t1`.
  - Sleep 5ms, then `Send` on B. `LastActivity` is after `t1`.
  - Sleep 5ms, then `Steer` on B. `LastActivity` advances again.
  - `OwnerNodeID`, `Live` and `LeaseExpiry` never come from B: owner is still A's id.
- `TestOwnerLocalActivityDoesNotTouchRegistry`: use `fakeRegistry` with `BaseDir: t.TempDir()`, start a loop, then Send and Observe on the owner. `touchCount()==0` (a cache hit keeps the local touch only).
- `TestObserveFinishedLoopDoesNotTouch`: a loop that finished (`Shutdown` on its runtime), Observed on another runtime. `LastActivity` stays zero.

**Step 2:** `go test ./internal/runtime -run 'Touch' -count=1` fails.

**Step 3: Implement:**
```go
// touchRemote records activity on a loop another replica runs, so that owner's idle
// reaper counts it (decision 8). Best-effort: a failed Touch only narrows the idle
// window, it never fails the caller's Observe/Send/Steer.
func (r *inProcRuntime) touchRemote(ctx context.Context, key event.StreamKey) {
	if r.deps.Registry == nil {
		return
	}
	_ = r.deps.Registry.Touch(ctx, key)
}
```
- `Observe`, durable branch: `rec, key, err := r.resolveDurable(...)`. After the error check, `if rec.Live { r.touchRemote(ctx, key) }`.
- `Send` / `Steer`: in the `errLiveElsewhere` branch, build `key := event.StreamKey{Tenant: event.NormalizeTenant(tenant), Loop: event.LoopID(loopID)}`, call `r.touchRemote(ctx, key)`, then relay.

**Step 4:** `go test ./internal/runtime -count=1` passes.

**Step 5: Commit** `feat(runtime): record cross-replica Observe/Send/Steer activity on the registry`.

---

## Task 6: The owner's idle reaper counts registry `LastActivity`

**Tier:** standard

**Files:**
- Modify: `internal/runtime/inproc.go` (`startIdleReaper` and its call site)
- Modify: `internal/runtime/fence_test.go`

**Step 1: Write failing tests.** Use an fsstore durable store, `IdleTTL: 300*time.Millisecond`, an interactive loop, and wait for park. Run `h.Wait()` in a goroutine that closes `done`.
- `TestIdleReaperHonoursRegistryActivity`:
  - A test goroutine calls `store.Touch(ctx, key)` every 60ms for 900ms (3× IdleTTL).
  - During that window `done` stays open; check at 900ms with a non-blocking select. The record is still `Live`.
  - Then stop touching and note `quiet := time.Now()`. `done` closes within `IdleTTL + 2s`, `time.Since(quiet) >= IdleTTL - 60ms` (it was not reaped early), and the record is not live.
- `TestIdleReaperReapsWithoutActivity` (control): no touches. `done` closes within `IdleTTL + 2s`, as before this change.
- `TestIdleReaperRegistryErrorFallsBackToLocal`: use `fakeRegistry` wrapped so `Resolve` returns an error. The loop is still reaped at about IdleTTL. Use `BaseDir: t.TempDir()`, since the registry is a fake.

**Step 2:** `go test ./internal/runtime -run 'IdleReaper' -count=1` fails (the first test reaps early).

**Step 3: Implement:**
- Change the signature to `startIdleReaper(sessionCtx, sessionCancel, stop, key event.StreamKey)` and pass `key` at the call site (~876).
- In the `<-timer.C` branch:
  ```go
  if wait := r.remoteActivityWait(key, ttl); wait > 0 {
  	timer.Reset(wait) // another replica was active more recently than the local window
  	continue
  }
  sessionCancel()
  return
  ```
  The `for { select { … } }` loop already continues, so `continue` re-enters the select.
- Add:
  ```go
  // remoteActivityWait returns how much of the idle window is left measured from the
  // registry's LastActivity (activity other replicas recorded with Touch), or 0 when
  // there is none, it is older than ttl, or the registry cannot be read (decision 9:
  // fall back to local activity only). Wall clock on purpose: LastActivity is stamped
  // by the backend's clock and the reaper's timer is real time; Deps.Now governs lease
  // math only.
  func (r *inProcRuntime) remoteActivityWait(key event.StreamKey, ttl time.Duration) time.Duration
  ```
  Return 0 when `r.deps.Registry == nil`. Otherwise call `Resolve(context.Background(), key)`; on error return 0; if `rec.LastActivity.IsZero()` return 0; else return `time.Until(rec.LastActivity.Add(ttl))`, floored at 0.
- Update the `startIdleReaper` and `Deps.IdleTTL` doc comments: activity now includes registry-visible activity from other replicas.

**Step 4:** `go test ./internal/runtime -count=1` passes (existing short-IdleTTL suites such as `resume_test.go`, `attach_finished_test.go` and `interactive_test.go` are unchanged in outcome). `go test -race ./internal/runtime -run 'IdleReaper|Interactive|Resume' -count=1` passes.

**Step 5: Commit** `feat(runtime): idle reaper counts activity other replicas record on the registry`.

---

## Task 7: Two-runtime acceptance tests

**Tier:** standard

**Files:**
- Create: `internal/runtime/fence_twoinstance_test.go`

Model these on `relay_test.go` (`twoReplicasWith`: two `New(Deps{…})` runtimes over ONE `fsstore.NewDurableFSStore(t.TempDir())`, used as both `DurableStore` and `Registry`) and on `cmd/fuse/loop_serve_net_twoinstance_test.go`, which builds two fully independent runtime graphs over one store. Each runtime gets its OWN completer, so the test can attribute model calls to a replica. Add a `countingCompleter` with an `atomic.Int64` call count that returns `model.CompletionResp{Content: "ok"}`. Build each agent with `agent.New(c, execAll{reg}, nopRenderer{}, modelID, "", 1, 0)`.

**Step 1: Write the tests:**
- `TestStaleOwnerIsFencedAfterAnotherReplicaResumes` (acceptance 3).
  - **Setup.**
    - A: `NodeID:"node-a"`, real clock, `LeaseTTL: time.Second`, `HeartbeatInterval: 20*time.Millisecond`, `IdleTTL: time.Hour`, `RelayPoll: 20*time.Millisecond`.
    - B: `NodeID:"node-b"`, `Now: func() time.Time { return time.Now().Add(time.Duration(skew.Load())) }`, the same TTLs.
  - **A starts.** A starts an interactive loop. `eventually` it parks, and `compA` has at least 1 call.
  - **A stalls past its lease.** `skew.Store(int64(time.Hour))`: from B's clock, A's lease has expired. This is the stalled-owner case: B reaps with a CAS on node-a, then claims.
  - **B resumes.** `rtB.Resume(ctx, tenant, id)` returns nil error. Resolve shows `OwnerNodeID=="node-b"` and `Live`. Note `claimSeq` = the highest seq in the stream right after Resume returns.
  - **A is fenced.**
    - `eventually`: `rtA.(*inProcRuntime).loopCache(tenant,id) == nil`.
    - A's handle `Wait()` returns within 3s.
    - Resolve is still node-b and `Live` (A's completion did not clobber it).
  - **Only B drives the loop.**
    - Note `nA := compA.n.Load()`.
    - Call `rtA.Send(ctx, tenant, id, "via A")`. It returns nil: A relays because the loop is live on B.
    - `eventually` `compB` sees a request whose last user turn contains "via A".
    - Sleep 300ms. `compA.n.Load() == nA` (A made no further model call).
  - **Stream.** Over `streamOf`:
    - `countErrorReason(evs, "ownership_lost") <= 1`;
    - exactly one `steer.request` and exactly one `user.input` whose `ReqSeqs` names it (the request was delivered once, so it was not driven by two owners);
    - every `model.call.start` with seq > `claimSeq` falls in the window where `compA` was frozen. Assert it by counting: the `model.call.start` events after `claimSeq` equal `compB`'s call count.
- `TestCrossReplicaActivityKeepsOwnerLoopAlive` (acceptance 4). Both runtimes leave `NodeID` unset, which exercises the minted default (learning `security-knob-inert-at-composition-root`). A: `IdleTTL: 400*time.Millisecond`. B: `IdleTTL: time.Hour`. Both use the real clock.
  - A starts an interactive loop and parks. Run A's `Wait()` in a goroutine that closes `done`.
  - For 1.2s (3× IdleTTL), every 100ms: `rtB.Observe` then unsubscribe right away. Every 4th iteration also `rtB.Send(ctx, tenant, id, "ping")`. Every call returns nil.
  - At the end of the window, `done` is still open and Resolve shows `Live`.
  - Stop all activity and note `quiet`. `done` closes within `400ms + 3s`, `time.Since(quiet) >= 400ms - 100ms`, and Resolve shows not live.
- `TestCrossReplicaIdleControlStillReaps`: the same setup with no B activity. `done` closes within `400ms + 3s`.

**Step 2:** Run `go test ./internal/runtime -run 'StaleOwner|CrossReplica' -count=1 -v`. If Tasks 3-6 are in, these pass on first run. That is expected for an acceptance task: confirm each assertion fails when its mechanism is disabled. For example, temporarily make the renewer ignore `ErrNotOwner`, watch the stale-owner test fail, then revert. Report that check; do not commit the temporary edit.

**Step 3:** `go test -race ./internal/runtime -run 'StaleOwner|CrossReplica' -count=3` passes (repeat runs to shake out timing flakes; widen timeouts, never weaken assertions).

**Step 4: Commit** `test(runtime): two-runtime fencing and cross-replica idle acceptance`.

---

## Task 8: Final verification

**Tier:** economy

1. `go build ./...` and `go vet ./...`.
2. `go vet -tags pgstore ./...`.
3. `make test` (the full gate).
4. `go test -race ./internal/runtime ./internal/event/... ./internal/loopconnect/... -count=1`.
5. `go test ./cmd/fuse -run 'TwoInstance' -count=1 -v`: `TestTwoInstanceDifferentInstanceReconnect` passes, unchanged (acceptance 5).
6. `go test -tags pgstore ./internal/event/pgstore/ -count=1`: passes, or report the skip explicitly.
7. Check that the fence is reachable in the shipped binary, not only in tests.
   - `grep -n 'Claim(\|ErrNotOwner\|Touch(' internal/runtime/inproc.go internal/runtime/fence_ownership.go` shows the resume claim, the renewer fence and `touchRemote`.
   - `grep -rn 'NodeID' cmd/fuse/*.go | grep -v _test` confirms no composition root pins a shared `Deps.NodeID`; each process relies on the minted default.
8. Scan the diff (code, comments, test names, commit messages) for any host-application name; there must be none ("callers" only).

No commit unless a step needed a fix (then `fix: …` describing it).

---

## Acceptance coverage map

| Criterion | Where |
| --- | --- |
| 1 six conformance cases, both backends | Task 1 (fsstore via `TestFSStoreConformance`), Task 2 (`TestPGConformance`, `-tags pgstore`) |
| 2 Heartbeat/SetLive non-owner refused; Claim expired ok / fresh refused | Task 1 cases `HeartbeatFromNonOwnerErrNotOwner`, `SetLiveFromNonOwnerErrNotOwner`, `ClaimExpiredLeaseSucceeds`, `ClaimFreshLeaseErrNotOwner`; Task 2 |
| 3 stale owner fenced; one or no `ownership_lost`; never two owners | Task 4 unit tests; Task 7 `TestStaleOwnerIsFencedAfterAnotherReplicaResumes` |
| 4 cross-replica activity keeps the loop alive; quiet loop still reaped | Task 5, Task 6; Task 7 `TestCrossReplicaActivityKeepsOwnerLoopAlive`, `TestCrossReplicaIdleControlStillReaps` |
| 5 two-instance test and resume/cancel suites unchanged | Tasks 3-6 Step 4 (full `internal/runtime` package); Task 8 steps 3-5 |
| 6 ADR | Recorded by the coordinator (no build task) |
