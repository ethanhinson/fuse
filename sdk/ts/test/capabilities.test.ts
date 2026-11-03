// Node test (node:test): specFeatures over the REAL Connect wire. The Go helper
// (./test/server) advertises the runtime's HarnessSpec features through
// GetCapabilities; with FUSE_TEST_PREDATING=1 it answers GetCapabilities with
// Unimplemented, as a server from before the call does.

import { test } from "node:test";
import assert from "node:assert/strict";
import { createConnectTransport } from "@connectrpc/connect-node";

import { createClient } from "../src/index.js";
import { spawnGoServer } from "./helpers.js";

test("specFeatures lists no_tools from a server that advertises it", async () => {
  const { url, stop } = await spawnGoServer("./test/server");
  try {
    const client = createClient({
      baseUrl: url,
      credentials: { token: "any", tenant: "acme" },
      transport: createConnectTransport({ baseUrl: url, httpVersion: "1.1" }),
    });
    assert.deepEqual(await client.specFeatures(), ["no_tools"]);
  } finally {
    stop();
  }
});

test("specFeatures is empty from a server that does not answer GetCapabilities", async () => {
  const { url, stop } = await spawnGoServer("./test/server", { FUSE_TEST_PREDATING: "1" });
  try {
    const client = createClient({
      baseUrl: url,
      credentials: { token: "any", tenant: "acme" },
      transport: createConnectTransport({ baseUrl: url, httpVersion: "1.1" }),
    });
    assert.deepEqual(await client.specFeatures(), []);
  } finally {
    stop();
  }
});
