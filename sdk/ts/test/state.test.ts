// Node test (node:test) for @fuse/sdk connection-state surface (change 0056, Task 1, D1).
//
// It drives observe(loopId, { onState }) against the scripted statesrv in MODE=reopen: the
// first Observe delivers seqs 1,2 then closes its live channel (a clean stream end) forcing
// the SDK to reconnect; the reconnect replays [1,2] and delivers the terminal seq 3
// (loop.parked). It asserts the onState callback observes connecting → live on the first
// frame, then reconnecting → live across the re-open. It also asserts the POSITIONAL
// observe(loopId, 0n) form still yields events (back-compat with the #50 node test).

import { test } from "node:test";
import assert from "node:assert/strict";
import { createConnectTransport } from "@connectrpc/connect-node";

import { createClient, isCompletion, type ConnState, type Event } from "../src/index.js";
import { spawnGoServer } from "./helpers.js";

function mkClient(url: string) {
  return createClient({
    baseUrl: url,
    credentials: { token: "any", tenant: "acme" },
    transport: createConnectTransport({ baseUrl: url, httpVersion: "1.1" }),
  });
}

test("observe({ onState }) surfaces connecting→live then reconnecting→live across a re-open", async () => {
  const server = await spawnGoServer("./test/statesrv", { MODE: "reopen" });
  const stop = server.stop;
  try {
    const client = mkClient(server.url);
    const { loopId } = await client.startLoop({ task: "state", model: "cloud/x", interactive: true });

    const states: ConnState[] = [];
    const seqs: bigint[] = [];
    for await (const ev of client.observe(loopId, {
      fromSeq: 0n,
      onState: (s) => states.push(s),
    })) {
      seqs.push(ev.seq);
      if (isCompletion(ev)) break; // seq 3 = loop.parked
    }

    // Every seq exactly once, in order (no-loss/no-dup across the forced re-open).
    assert.deepEqual(seqs, [1n, 2n, 3n], `got seqs ${seqs.join(",")}`);

    // First transition is connecting; live must appear before the first re-open; a
    // reconnecting must appear (the forced stream end) followed by live again.
    assert.equal(states[0], "connecting", `states=${states.join(",")}`);
    const firstLive = states.indexOf("live");
    const reconnecting = states.indexOf("reconnecting");
    assert.ok(firstLive >= 0, `no live state seen: ${states.join(",")}`);
    assert.ok(reconnecting > firstLive, `no reconnecting after first live: ${states.join(",")}`);
    const liveAfterReconnect = states.indexOf("live", reconnecting);
    assert.ok(liveAfterReconnect > reconnecting, `no live after reconnecting: ${states.join(",")}`);
  } finally {
    stop();
  }
});

test("positional observe(loopId, 0n) still yields events (back-compat)", async () => {
  const server = await spawnGoServer("./test/statesrv", { MODE: "reopen" });
  const stop = server.stop;
  try {
    const client = mkClient(server.url);
    const { loopId } = await client.startLoop({ task: "state", model: "cloud/x", interactive: true });

    const seqs: bigint[] = [];
    let last: Event | undefined;
    for await (const ev of client.observe(loopId, 0n)) {
      seqs.push(ev.seq);
      last = ev;
      if (isCompletion(ev)) break;
    }
    assert.deepEqual(seqs, [1n, 2n, 3n], `got seqs ${seqs.join(",")}`);
    assert.ok(last && isCompletion(last), "positional observe did not reach completion");
  } finally {
    stop();
  }
});
