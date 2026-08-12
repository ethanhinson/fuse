// Node test (node:test) for @fuse/sdk transient-vs-terminal error classification
// (change 0056, Task 2, D2).
//
// A TERMINAL Connect code (unauthenticated / permission_denied / not_found /
// failed_precondition) must STOP the reconnect loop and surface a typed FuseTerminalError
// out of the async iterator, firing onState('closed') — instead of the pre-0056 infinite
// hot-loop that swallowed every error. A TRANSIENT drop (clean stream end / network error)
// still reconnects (the #50 behavior, also covered by state.test.ts's reopen).
//
// The hot-loop guard: statesrv MODE=terminal returns the terminal error on EVERY Observe,
// so if the SDK reconnected it would spin forever. We assert the iterator throws promptly
// (bounded by node:test's timeout) and that onState observed connecting → closed with NO
// intervening reconnecting — proof the loop stopped on the first terminal code.

import { test } from "node:test";
import assert from "node:assert/strict";
import { createConnectTransport } from "@connectrpc/connect-node";

import { createClient, FuseTerminalError, type ConnState } from "../src/index.js";
import { spawnGoServer } from "./helpers.js";

function mkClient(url: string) {
  return createClient({
    baseUrl: url,
    credentials: { token: "any", tenant: "acme" },
    transport: createConnectTransport({ baseUrl: url, httpVersion: "1.1" }),
  });
}

const terminalCases: Array<{ code: string; label: string }> = [
  { code: "unauthenticated", label: "unauthenticated" },
  { code: "permission_denied", label: "permission_denied" },
  { code: "not_found", label: "not_found" },
  { code: "failed_precondition", label: "failed_precondition" },
];

for (const tc of terminalCases) {
  test(`terminal Connect code '${tc.label}' throws FuseTerminalError, fires closed, does not hot-loop`, async () => {
    const server = await spawnGoServer("./test/statesrv", { MODE: "terminal", TERMINAL_CODE: tc.code });
    const stop = server.stop;
    try {
      const client = mkClient(server.url);
      const { loopId } = await client.startLoop({ task: "err", model: "cloud/x", interactive: true });

      const states: ConnState[] = [];
      let thrown: unknown;
      let delivered = 0;
      try {
        for await (const _ev of client.observe(loopId, { onState: (s) => states.push(s) })) {
          delivered++;
          if (delivered > 5) break; // safety: a broken impl must not run forever here.
        }
      } catch (e) {
        thrown = e;
      }

      assert.equal(delivered, 0, "a terminal error delivered events (should throw before any)");
      assert.ok(thrown instanceof FuseTerminalError, `threw ${String(thrown)}, want FuseTerminalError`);
      // The typed error must carry the terminal code for the app to branch on.
      assert.ok((thrown as FuseTerminalError).code, "FuseTerminalError has no code");
      // No reconnect was attempted: connecting then closed, never reconnecting.
      assert.ok(states.includes("closed"), `states=${states.join(",")} (no closed)`);
      assert.ok(!states.includes("reconnecting"), `hot-looped: states=${states.join(",")}`);
    } finally {
      stop();
    }
  });
}

test("a transient stream end reconnects (not treated as terminal)", async () => {
  // MODE=reopen: first Observe closes the live channel (a clean end); the SDK must
  // reconnect and complete — proving a transient drop is NOT misclassified as terminal.
  const server = await spawnGoServer("./test/statesrv", { MODE: "transient-then-done" });
  const stop = server.stop;
  try {
    const client = mkClient(server.url);
    const { loopId } = await client.startLoop({ task: "err", model: "cloud/x", interactive: true });

    const seqs: bigint[] = [];
    const states: ConnState[] = [];
    for await (const ev of client.observe(loopId, { onState: (s) => states.push(s) })) {
      seqs.push(ev.seq);
      if (ev.kind === "loop.parked") break;
    }
    assert.deepEqual(seqs, [1n, 2n, 3n], `got ${seqs.join(",")}`);
    assert.ok(states.includes("reconnecting"), `expected a reconnect: ${states.join(",")}`);
  } finally {
    stop();
  }
});
