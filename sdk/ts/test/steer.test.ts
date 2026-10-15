// Steering surface of @fuse/sdk, driven against an in-memory Connect router so the exact
// request fields that reach the wire are asserted without a server process.

import { test } from "node:test";
import assert from "node:assert/strict";
import { createRouterTransport } from "@connectrpc/connect";

import { LoopService } from "../../../proto/gen/ts/fuse/loop/v1/loop_pb.js";
import { createClient } from "../src/index.js";

function harness() {
  const sends: Array<{ loopId: string; input: string; interrupt: boolean; tenant: string; source: string }> = [];
  const starts: Array<{ task: string; orchestrateLoopId: string; interactive: boolean }> = [];
  const transport = createRouterTransport(({ service }) => {
    service(LoopService, {
      startLoop(req) {
        starts.push({
          task: req.task,
          orchestrateLoopId: req.orchestrateLoopId,
          interactive: req.interactive,
        });
        return { loopId: "orch-1" };
      },
      send(req) {
        sends.push({ loopId: req.loopId, input: req.input, interrupt: req.interrupt, tenant: req.tenant, source: req.source });
        return {};
      },
    });
  });
  const client = createClient({
    baseUrl: "http://unused",
    credentials: { token: "t", tenant: "acme" },
    transport,
  });
  return { client, sends, starts };
}

test("send does not interrupt; interrupt does", async () => {
  const { client, sends } = harness();
  await client.send("loop-1", "hello");
  await client.interrupt("loop-1", "stop now");
  assert.deepEqual(sends, [
    { loopId: "loop-1", input: "hello", interrupt: false, tenant: "acme", source: "" },
    { loopId: "loop-1", input: "stop now", interrupt: true, tenant: "acme", source: "" },
  ]);
});

test("startLoop threads the orchestrate target", async () => {
  const { client, starts } = harness();
  const { loopId } = await client.startLoop({ task: "watch it", model: "m", orchestrate: "target-9" });
  assert.equal(loopId, "orch-1");
  assert.equal(starts[0].orchestrateLoopId, "target-9");

  await client.startLoop({ task: "plain", model: "m" });
  assert.equal(starts[1].orchestrateLoopId, "");
});

test("sendWith labels the source", async () => {
  const { client, sends } = harness();
  await client.sendWith("loop-1", "try the cache", { source: "orchestrator", interrupt: true });
  assert.deepEqual(sends, [
    { loopId: "loop-1", input: "try the cache", interrupt: true, tenant: "acme", source: "orchestrator" },
  ]);
});
