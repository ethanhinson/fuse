<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0117 — Cancel RPC on the networked loop server](../changes/archive/2026-11-03-0117-cancel-rpc-on-the-networked-loop-server.md)**
<!-- docket:backlink:end -->

# Cancel RPC on the networked loop server — results

**Human action:** No action is required before merge. One optional check of a cross-replica cancel against a real Postgres-backed deployment is listed below, because the automated tests use the file-backed durable store only.

## Outcome

Callers can now stop a loop they own with `CancelLoop`. It needs the new `loop:cancel` scope, and `loop:admin` also satisfies it. The loop's in-flight model call, tools, sandbox processes and child agents unwind through context cancellation, and its pools and MCP manager are released. A terminal `loop.cancelled` event with the caller's reason becomes the last event of the stream. The registry shows the loop as not live before the call returns `CANCELLED`, and Send and Resume refuse the loop afterwards with "loop cancelled". Cancel is idempotent and tenant-isolated, relays to another replica through the durable stream, and is counted by `fuse_loops_cancelled_total{tenant_id, was_live}`. The TS SDK gains `client.cancel`, the README, Helm values and observability docs are updated, and ADR-0078 records the design. The branch was rebased onto `origin/main` after change 0118 (carried skills) landed, and the protos were regenerated.

## Human actions and testing

- **Optional — cross-replica cancel on Postgres.** Why: the two-replica tests run on the file-backed durable store; the Postgres store implements the same interfaces but was not exercised for this feature. Prerequisites: two `fuse loop-serve-net` replicas sharing one Postgres durable store and registry, a token with `loop:start loop:observe loop:cancel`, and the gateway models `glm`/`kimi` on `localhost:4000` (never an Anthropic model). Steps: start a loop on replica A whose model call takes a while; call `CancelLoop` on replica B with `reason: "check"`. Expected: B returns `CANCEL_OUTCOME_CANCELLED` with `was_live=true`; an Observe on either replica ends with `loop.cancelled` carrying `reason: "check"`; a later Send returns FailedPrecondition containing "cancelled"; `/metrics` on A shows `fuse_loops_cancelled_total{...,was_live="true"} 1`. Cleanup: none. The cancelled loop stays sealed and needs no cleanup.

## Verification performed

- Full suite `make test` passed through the gate driver after the fix pass, at the final head.
- Focused `-race` runs for `internal/runtime`, `internal/loopconnect`, `internal/observe/...` and `cmd/fuse`, `make sdk-ts-test`, `make observability-validate`, and a check that `make proto` reproduces the committed output with no diff.
- End-to-end tests over the real `loop-serve-net` command use a scripted HTTP gateway only. They cover cancel during a model call, tenant isolation, freeing the `max_live_loops` slot, the `/metrics` counter and the log projection.
- Deep review (docket-review-deep) returned 7 findings (1 blocker, 3 important, 3 minor). All 7 were fixed in-branch, and the suite passed after the fixes.

## Known issues and follow-ups

- **A replica that still caches a finished loop answers Send with "loop finished" after another replica cancels it.** When: the loop finished on replica A, and replica B later cancelled it. What happens: a Send that reaches A says "loop finished" rather than "loop cancelled". Resume on A returns the old handle and never relaunches it, so the loop stays dead. Impact: a misleading error message only. Confirmed by design (plan decision 11). Workaround: none needed. Next action: route the cached-finished branch of Send through the stream check that `IsCancelled` now provides. Backlog: **Related to #108** (Send surfaces a resume refusal instead of "loop finished"). Folding this in would widen #108 from the Resume error path to the cached path, so this needs a new change a human captures, listing #108 under `related:`.
- **Two `loop.cancelled` events can still appear in rare races, and the counter can over-count then.** When: two replicas seal the same not-live loop at the same moment, or an owner and a requester race to answer one relayed request. What happens: the stream gets two terminal events; readers take the first. Neither writer knows about the other, so the second is not flagged `superseded`, and `fuse_loops_cancelled_total` counts both. Impact: rare over-count; the seal holds either way. Suspected rare; not reproduced outside targeted races. Next action: check the stream at append time under a store-level conditional append. Backlog: **No existing change fits (checked 13).**
- **Go SDK has no cancel method.** The spec limited SDK work to TypeScript; `sdk/fuse` is unchanged. Next action: add `Cancel` to the Go SDK if Go callers need it. Backlog: **No existing change fits (checked 13).**
- **A relay sent from a cached finished loop does not seal that copy's sink.** When `cancelFinished` relays because another replica resumed the loop, this replica's leftover in-memory loop is not sealed once the relay settles. Impact: none observed, because a finished loop no longer appends events. In-scope limitation, recorded for the reviewer.
