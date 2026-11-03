<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0117 — Cancel RPC on the networked loop server](../changes/active/0117-cancel-rpc-on-the-networked-loop-server.md)**
<!-- docket:backlink:end -->

# Cancel RPC on the networked loop server — results

**Human action:** Assessment pending; the review fix pass is in progress.

## Outcome

The branch adds `CancelLoop` (scope `loop:cancel`, also satisfied by `loop:admin`), per-loop abort with a terminal `loop.cancelled` event, sealing against Send/Resume, cross-replica relay through the durable stream, `fuse_loops_cancelled_total`, the TS SDK `client.cancel`, docs, and ADR-0078. The build gate (`make test`) passed at 8a5bdc8 after rebasing onto `origin/main` (which now includes change 0118's proto change; protos regenerated).

## Verification performed

- Full suite `make test` green at 8a5bdc83786a32f4c0295665d97085f68c617150.
- Deep review (docket-review-deep) returned 7 findings; fix pass pending:
  1. blocker — `cancelLocal` re-seals a cached finished loop without checking the durable stream (double terminal, wrong outcome across replicas).
  2. important — `LoopTeardown` can append `sandbox.reap` after `loop.cancelled`.
  3. important — relayed request never sealed when the owner finishes before honouring it; CANCELLING may never resolve.
  4. important — counter counts duplicate terminals.
  5. minor — `cancelPending` entries leak on timeout.
  6. minor — process-wide `cancelMu` held across store I/O.
  7. minor — Send to a cancelled uncached loop at tenant cap returns RESOURCE_EXHAUSTED.
