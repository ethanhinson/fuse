// Cancel surface of @fuse/sdk, driven against an in-memory Connect router so the exact
// request fields that reach the wire are asserted without a server process.

import { test } from "node:test";
import assert from "node:assert/strict";
import { Code, ConnectError, createRouterTransport } from "@connectrpc/connect";

import { CancelOutcome, LoopService } from "../../../proto/gen/ts/fuse/loop/v1/loop_pb.js";
import { createClient, isCompletion, KIND_LOOP_CANCELLED, type Event } from "../src/index.js";

function harness(outcome: CancelOutcome = CancelOutcome.CANCELLED, fail?: ConnectError) {
  const cancels: Array<{ loopId: string; reason: string; tenant: string }> = [];
  const transport = createRouterTransport(({ service }) => {
    service(LoopService, {
      cancelLoop(req) {
        cancels.push({ loopId: req.loopId, reason: req.reason, tenant: req.tenant });
        if (fail) throw fail;
        return { outcome, wasLive: true, terminalSeq: 9n, reason: req.reason };
      },
    });
  });
  const client = createClient({
    baseUrl: "http://unused",
    credentials: { token: "t", tenant: "acme" },
    transport,
  });
  return { client, cancels };
}

test("cancel sends loop, reason, tenant and maps the result", async () => {
  const { client, cancels } = harness();
  const res = await client.cancel("loop-1", "operator stop");
  assert.deepEqual(cancels, [{ loopId: "loop-1", reason: "operator stop", tenant: "acme" }]);
  assert.deepEqual(res, { outcome: "cancelled", wasLive: true, terminalSeq: 9n, reason: "operator stop" });
});

test("cancel without a reason sends an empty one", async () => {
  const { client, cancels } = harness();
  await client.cancel("loop-1");
  assert.equal(cancels[0].reason, "");
});

test("cancel maps every outcome; unspecified throws", async () => {
  assert.equal((await harness(CancelOutcome.ALREADY_CANCELLED).client.cancel("l")).outcome, "already_cancelled");
  assert.equal((await harness(CancelOutcome.CANCELLING).client.cancel("l")).outcome, "cancelling");
  await assert.rejects(harness(CancelOutcome.UNSPECIFIED).client.cancel("l"));
});

test("cancel surfaces a router error unchanged", async () => {
  const { client } = harness(CancelOutcome.CANCELLED, new ConnectError("nope", Code.PermissionDenied));
  await assert.rejects(client.cancel("l"), (err: unknown) => {
    assert.ok(err instanceof ConnectError);
    assert.equal(err.code, Code.PermissionDenied);
    return true;
  });
});

test("isCompletion covers loop.cancelled", () => {
  assert.equal(KIND_LOOP_CANCELLED, "loop.cancelled");
  const ev = (kind: string) => ({ seq: 1n, kind }) as unknown as Event;
  assert.equal(isCompletion(ev("loop.cancelled")), true);
  assert.equal(isCompletion(ev("loop.parked")), true);
  assert.equal(isCompletion(ev("model.call.end")), false);
});
