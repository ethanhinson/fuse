// Node test (node:test): startLoop with a harness spec over the REAL Connect wire
// (change 0085). The Go helper (./test/server) validates a carried spec with the Go
// validator: an accepted spec is acknowledged in the loop id with its content digest, a
// refused one comes back as InvalidArgument whose detail lists every violation.

import { test } from "node:test";
import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { create, toBinary } from "@bufbuild/protobuf";
import { createConnectTransport } from "@connectrpc/connect-node";

import { createClient, FuseHarnessError, harness } from "../src/index.js";
import { spawnGoServer } from "./helpers.js";

test("startLoop carries a harness spec and surfaces its violations", async () => {
  const { url, stop } = await spawnGoServer("./test/server");
  try {
    const client = createClient({
      baseUrl: url,
      credentials: { token: "any", tenant: "acme" },
      transport: createConnectTransport({ baseUrl: url, httpVersion: "1.1" }),
    });

    // Accepted: the spec crossed the wire intact (same digest the server records).
    const spec = { maxTurns: 1, skills: [{ name: "research" }] };
    const { loopId } = await client.startLoop({ task: "t", model: "", harness: spec });
    const digest = createHash("sha256").update(toBinary(harness.HarnessSpecSchema, create(harness.HarnessSpecSchema, spec))).digest("hex");
    assert.equal(loopId, `loop-ts sha256:${digest}`);

    // Refused: every violation, with its field path and rule id.
    await assert.rejects(
      client.startLoop({ task: "t", model: "", harness: { context: { summarization: { threshold: 2 } }, maxTokens: 0 } }),
      (err: unknown) => {
        assert.ok(err instanceof FuseHarnessError, `got ${String(err)}`);
        assert.deepEqual(
          err.violations.map((v) => [v.fieldPath, v.ruleId]),
          [["context.summarization.threshold", "double.gte_lte"]],
        );
        assert.match(err.message, /context\.summarization\.threshold/);
        return true;
      },
    );
  } finally {
    stop();
  }
});
