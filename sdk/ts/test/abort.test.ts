// Node test (node:test) for @fuse/sdk idempotent AbortSignal teardown
// (change 0056, Task 3, D3).
//
// A real browser app needs an explicit, idempotent way to stop observing and release the
// stream on page-unload / component-teardown — it must not leak a stream per navigation.
// observe(loopId, { signal }) accepts an AbortSignal: aborting stops the reconnect loop,
// tears the underlying stream down, and fires onState('closed') EXACTLY ONCE. A second
// abort() (or abort-after-close) is a no-op.
//
// statesrv MODE=park delivers seq 1 then keeps the stream open, so the iterator blocks
// after one event — the point at which we abort.

import { test } from "node:test";
import assert from "node:assert/strict";
import { createConnectTransport } from "@connectrpc/connect-node";

import { createClient, type ConnState } from "../src/index.js";
import { spawnGoServer } from "./helpers.js";

function mkClient(url: string) {
  return createClient({
    baseUrl: url,
    credentials: { token: "any", tenant: "acme" },
    transport: createConnectTransport({ baseUrl: url, httpVersion: "1.1" }),
  });
}

test("abort() stops observe, fires closed exactly once, and is idempotent", async () => {
  const server = await spawnGoServer("./test/statesrv", { MODE: "park" });
  const stop = server.stop;
  try {
    const client = mkClient(server.url);
    const { loopId } = await client.startLoop({ task: "abort", model: "cloud/x", interactive: true });

    const ac = new AbortController();
    const states: ConnState[] = [];
    let closedCount = 0;
    const onState = (s: ConnState) => {
      states.push(s);
      if (s === "closed") closedCount++;
    };

    const seqs: bigint[] = [];
    // Drain the iterator in the background; abort after the first event arrives.
    const drain = (async () => {
      for await (const ev of client.observe(loopId, { signal: ac.signal, onState })) {
        seqs.push(ev.seq);
        // First event in hand: tear down. The stream is parked (open), so without the
        // abort this for-await would block forever.
        ac.abort();
      }
    })();

    // The iterator must RETURN (not hang) once aborted — bounded by the test timeout.
    await drain;

    assert.deepEqual(seqs, [1n], `got seqs ${seqs.join(",")}`);
    assert.equal(closedCount, 1, `closed fired ${closedCount} times, want exactly 1: ${states.join(",")}`);
    assert.equal(states[states.length - 1], "closed", `last state ${states.join(",")}`);

    // A second abort (and an abort-after-close) must be a no-op — no extra closed, no throw.
    ac.abort();
    assert.equal(closedCount, 1, "second abort re-fired closed (not idempotent)");
  } finally {
    stop();
  }
});

test("aborting an already-aborted signal before iteration closes immediately (no events)", async () => {
  const server = await spawnGoServer("./test/statesrv", { MODE: "park" });
  const stop = server.stop;
  try {
    const client = mkClient(server.url);
    const { loopId } = await client.startLoop({ task: "abort", model: "cloud/x", interactive: true });

    const ac = new AbortController();
    ac.abort(); // pre-aborted

    const states: ConnState[] = [];
    const seqs: bigint[] = [];
    for await (const ev of client.observe(loopId, { signal: ac.signal, onState: (s) => states.push(s) })) {
      seqs.push(ev.seq);
    }
    assert.deepEqual(seqs, [], "delivered events for a pre-aborted signal");
    assert.ok(states.includes("closed"), `no closed for pre-aborted signal: ${states.join(",")}`);
  } finally {
    stop();
  }
});
