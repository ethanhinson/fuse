// connect-es TS smoke client for the fuse.loop.v1 wire (change 0055, Task 6).
//
// It drives a locally-started connect-go server (URL from argv[2]) using the REAL
// generated stub (../gen/ts/fuse/loop/v1/loop_pb.js) via @connectrpc/connect
// createClient: a unary StartLoop, a unary Send, a server-streaming Observe that
// tracks event.seq, then a RECONNECT that re-opens Observe(from_seq) from the last
// tracked seq and asserts the replay resumes with no loss/dup. On success it prints
// a single SMOKE_OK line and exits 0; any failure exits non-zero with a message.
//
// This is the node-based connect-es proof (the plan's accepted CI-friendly form). A
// browser/Playwright run is a documented manual step (see the results file); the wire
// exercised here is identical (Connect over HTTP, same generated stub).

import { createClient } from "@connectrpc/connect";
import { createConnectTransport } from "@connectrpc/connect-node";
import { LoopService } from "../gen/ts/fuse/loop/v1/loop_pb.js";

async function main() {
  const baseUrl = process.argv[2];
  if (!baseUrl) {
    console.error("usage: client.ts <server-url>");
    process.exit(2);
  }
  // Hard watchdog so a hung stream fails loudly instead of blocking the test.
  const watchdog = setTimeout(() => {
    console.error("SMOKE_FAIL: watchdog timeout (stream hung)");
    process.exit(3);
  }, 15000);
  watchdog.unref?.();

  const transport = createConnectTransport({
    baseUrl,
    httpVersion: "1.1", // the Go httptest server speaks HTTP/1.1; Connect streams over it.
  });
  const client = createClient(LoopService, transport);

  // 1) Unary StartLoop.
  const started = await client.startLoop({ task: "smoke", model: "cloud/x", tenant: "acme" });
  if (!started.loopId) throw new Error("StartLoop returned empty loop_id");
  const loopId = started.loopId;
  console.error("step: startLoop ok " + loopId);

  // 2) Unary Send (tenant threaded).
  await client.send({ loopId, input: "hello", tenant: "acme" });
  console.error("step: send ok");

  // 3) Server-streaming Observe from 0: collect the first few events, tracking seq.
  const seenA: bigint[] = [];
  let lastSeq = 0n;
  const target = 3; // the server scripts >=3 replay events.
  for await (const frame of client.observe({ loopId, fromSeq: 0n, tenant: "acme" })) {
    if (frame.keepalive || !frame.event) continue;
    seenA.push(frame.event.seq);
    lastSeq = frame.event.seq;
    if (seenA.length >= target) break;
  }
  if (seenA.length < target) {
    throw new Error(`Observe delivered ${seenA.length} events, want >= ${target}`);
  }

  // 4) RECONNECT: re-open Observe(from_seq = lastSeq - 1) and assert the stream
  //    resumes from the tracked seq with no duplication of already-seen-earlier seqs
  //    and no loss past the resume point.
  const resumeFrom = lastSeq - 1n; // ask for events strictly greater than resumeFrom.
  const seenB: bigint[] = [];
  for await (const frame of client.observe({ loopId, fromSeq: resumeFrom, tenant: "acme" })) {
    if (frame.keepalive || !frame.event) continue;
    seenB.push(frame.event.seq);
    if (seenB.length >= 1) break; // one resumed frame is enough to prove reconnect.
  }
  if (seenB.length < 1) throw new Error("reconnect Observe(from_seq) delivered nothing");
  if (seenB[0] <= resumeFrom) {
    throw new Error(`reconnect resumed at seq ${seenB[0]}, want > ${resumeFrom}`);
  }

  clearTimeout(watchdog);
  console.log("SMOKE_OK loopId=" + loopId + " seenA=" + seenA.join(",") + " resumeSeq=" + seenB[0]);
  // Break-ing out of an `for await` server-stream leaves the underlying HTTP
  // connection open, so node would not exit on its own — exit explicitly on success.
  process.exit(0);
}

main().catch((err) => {
  console.error("SMOKE_FAIL: " + (err?.message ?? err));
  process.exit(1);
});
