// Node test (node:test) driving @fuse/sdk against a REAL connect-go server.
//
// It spawns the Go helper (`go run ./test/server`, cwd = this sdk/ts dir), reads the
// `URL <url>` line the helper prints, and drives the SDK's createClient over a
// @connectrpc/connect-node transport (which speaks HTTP/1.1 to the Go httptest-style
// server — the browser default transport is @connectrpc/connect-web, injected here for
// node). It asserts, FROM TS:
//
//   1. startLoop -> send -> observe delivers events (the scripted history 1..4);
//   2. no-loss/no-dup across the server's subscribe->replay gap (the helper forces an
//      overlap event, seq 3, into the window so a naive replay double-delivers it) — the
//      client sees every seq EXACTLY once;
//   3. a reconnect observe(loopId, fromSeq = lastSeq) resumes with no loss / no dup —
//      no seq <= fromSeq reappears and no seq past it is lost.
//
// This is the REQUIRED acceptance for change 0050 (stronger than #55's one-frame check).
// The real-engine end-to-end for the identical wire is the Go sdk/fuse/acceptance_test.go
// real-loop test (the wire is language-agnostic); see README.md.

import { test } from "node:test";
import assert from "node:assert/strict";
import { spawn, type ChildProcess } from "node:child_process";
import { fileURLToPath } from "node:url";
import { dirname, resolve } from "node:path";
import { createConnectTransport } from "@connectrpc/connect-node";

import { createClient, isCompletion, type Event } from "../src/index.js";

const here = dirname(fileURLToPath(import.meta.url));
const sdkTsDir = resolve(here, "..");

// startServer spawns the Go helper and resolves with its base URL + a kill fn.
//
// The helper is launched `detached: true` so it becomes its own process-group leader:
// `go run` execs a compiled grandchild, and killing the `go run` pid alone would orphan
// that grandchild (leaving the port bound and node's test runner hung on open stdio).
// stop() kills the whole group (kill(-pid)) so the grandchild dies too — a clean teardown.
function startServer(): Promise<{ url: string; stop: () => void }> {
  return new Promise((resolvePromise, rejectPromise) => {
    const proc: ChildProcess = spawn("go", ["run", "./test/server"], {
      cwd: sdkTsDir,
      stdio: ["ignore", "pipe", "pipe"],
      detached: true,
    });
    const killGroup = () => {
      try {
        // Negative pid targets the process group (the detached leader + its grandchild).
        if (proc.pid !== undefined) process.kill(-proc.pid, "SIGKILL");
      } catch {
        // already gone
      }
      try {
        proc.kill("SIGKILL");
      } catch {
        // already gone
      }
    };
    let stdout = "";
    let settled = false;
    const timer = setTimeout(() => {
      if (!settled) {
        settled = true;
        killGroup();
        rejectPromise(new Error("timed out waiting for server URL; stdout=" + stdout));
      }
    }, 30_000);
    timer.unref?.();

    proc.stdout?.on("data", (chunk: Buffer) => {
      stdout += chunk.toString();
      const match = stdout.match(/^URL (\S+)$/m);
      if (match && !settled) {
        settled = true;
        clearTimeout(timer);
        resolvePromise({
          url: match[1],
          stop: killGroup,
        });
      }
    });
    let stderr = "";
    proc.stderr?.on("data", (chunk: Buffer) => {
      stderr += chunk.toString();
    });
    proc.on("exit", (code) => {
      if (!settled) {
        settled = true;
        clearTimeout(timer);
        rejectPromise(new Error(`server exited (code ${code}) before URL; stderr=${stderr}`));
      }
    });
  });
}

// collect drains an async-iterable of events until it has seen `through` (inclusive) or
// the count cap is hit, returning the ordered seqs and the last event.
async function collect(
  iter: AsyncIterable<Event>,
  through: bigint,
  cap = 20,
): Promise<{ seqs: bigint[]; last: Event | undefined }> {
  const seqs: bigint[] = [];
  let last: Event | undefined;
  for await (const ev of iter) {
    seqs.push(ev.seq);
    last = ev;
    if (ev.seq >= through || seqs.length >= cap) break;
  }
  return { seqs, last };
}

function assertNoLossNoDup(seqs: bigint[], afterExclusive: bigint, throughInclusive: bigint, label: string) {
  // Expect exactly the contiguous run (after, through], each once, in order.
  const want: bigint[] = [];
  for (let s = afterExclusive + 1n; s <= throughInclusive; s++) want.push(s);
  assert.deepEqual(
    seqs,
    want,
    `${label}: got ${seqs.join(",")}, want ${want.join(",")} (no-loss/no-dup violated)`,
  );
}

test("startLoop -> send -> observe -> reconnect: no-loss/no-dup from TS over the real wire", async () => {
  const { url, stop } = await startServer();
  try {
    const client = createClient({
      baseUrl: url,
      credentials: { token: "any", tenant: "acme" },
      transport: createConnectTransport({ baseUrl: url, httpVersion: "1.1" }),
    });

    // 1) Unary StartLoop.
    const { loopId } = await client.startLoop({ task: "smoke", model: "cloud/x", interactive: true });
    assert.ok(loopId, "startLoop returned empty loopId");

    // 2) Unary Send.
    await client.send(loopId, "hello");

    // 3) Observe from 0: collect through seq 4 (the terminal loop.parked event). The
    //    server forces the overlap event (seq 3) into the subscribe->replay window, so a
    //    naive replay would double-deliver it. Assert every seq exactly once, in order.
    const first = await collect(client.observe(loopId, 0n), 4n);
    assertNoLossNoDup(first.seqs, 0n, 4n, "initial observe (overlap gap)");
    assert.ok(first.last, "no terminal event");
    assert.ok(isCompletion(first.last!), `last event kind = ${first.last!.kind}, want loop.parked`);

    // 4) RECONNECT: re-open observe(fromSeq = lastSeq - 1). The server replays history
    //    with Seq > (lastSeq-1), i.e. just seq 4. Assert no seq <= resumeFrom reappears
    //    and seq 4 is delivered exactly once (no loss, no dup across the resume point).
    const lastSeq = first.last!.seq; // 4n
    const resumeFrom = lastSeq - 1n; // 3n
    const second = await collect(client.observe(loopId, resumeFrom), lastSeq);
    assertNoLossNoDup(second.seqs, resumeFrom, lastSeq, "reconnect observe(fromSeq)");
    assert.ok(
      second.seqs.every((s) => s > resumeFrom),
      `reconnect delivered a seq <= ${resumeFrom} (duplicate): ${second.seqs.join(",")}`,
    );
  } finally {
    stop();
  }
});
