#!/usr/bin/env node
// Tiny zero-dependency launcher for the Wander concierge demo (change 0056).
//
// Two jobs, NO WebSocket relay — the SDK speaks Connect DIRECTLY over connect-web; this
// server only keeps the browser same-origin. (The retired concierge demo's
// server.js hand-rolled an RFC 6455 relay plus a `GET /loops/{id}/events` replay proxy for
// the pre-#55 wire; change 0060 folded that demo's UI into this one and deleted the relay.
// Nothing here re-implements the protocol.):
//
//   1. Serves the static web app (index.html / app.js / styles.css / vendor/fuse-sdk.js).
//   2. Reverse-proxies the Connect service path `/fuse.loop.v1.*` straight through to the
//      `fuse loop-serve-net` backend. This is a TRANSPARENT HTTP forward (not a protocol
//      re-implementation): the browser POSTs Connect requests to the same origin it loaded
//      from, so there is no CORS preflight and no cross-origin config, while the SDK still
//      drives the real Connect wire. Server-streaming (Observe) responses are piped
//      un-buffered so the live tail streams frame-by-frame.
//
// The upstream fuse address is FUSE_NET_ADDR (default 127.0.0.1:8787); the static port is
// PORT (default 5173). The Authorization bearer header the SDK sets is forwarded verbatim.
//
// Change 0060 (task 8) added a THIRD, demo-only job: `GET /demo-users.json` publishes the
// `loop_server.auth` directory of a DEMO fuse config so the page's user picker can offer
// those principals. See DEMO_CONFIG below — it publishes bearer tokens by design, and the
// only file it is ever allowed to point at is a demo one.

const http = require("http");
const fs = require("fs");
const path = require("path");

const PORT = Number(process.env.PORT || 5173);
const UPSTREAM = process.env.FUSE_NET_ADDR || "127.0.0.1:8787";
const [UP_HOST, UP_PORT] = UPSTREAM.split(":");

// DEMO_CONFIG is the fuse config whose `loop_server.auth` directory the picker offers.
//
// ⚠ THIS ENDPOINT HANDS EVERY BEARER TOKEN IN THAT FILE TO ANY BROWSER THAT ASKS. That is
// deliberate and safe for the default — examples/wander/fuse.demo.yml holds only
// obviously-fake, checked-in demo tokens that grant access to nothing real. NEVER point
// FUSE_DEMO_CONFIG at ~/.fuse/config.yml or any file with a credential you care about; the
// override exists so a test (or a demo on ephemeral ports) can supply its own demo file.
const DEMO_CONFIG = process.env.FUSE_DEMO_CONFIG || path.join(__dirname, "fuse.demo.yml");
const DEMO_CONFIG_OVERRIDDEN = Boolean(process.env.FUSE_DEMO_CONFIG);

// readDemoUsers extracts the `loop_server.auth` entries from a fuse config. It is a
// deliberately tiny, indentation-aware reader for exactly that one block rather than a
// YAML dependency (this launcher is zero-dependency by design). Anything it cannot parse
// yields an empty directory — the picker then offers only the built-in dev credential.
function readDemoUsers(file) {
  let text;
  try {
    text = fs.readFileSync(file, "utf8");
  } catch {
    return [];
  }
  const users = [];
  let inLoopServer = false;
  let authIndent = -1;
  let cur = null;
  const flush = () => {
    if (cur && cur.token) users.push(cur);
    cur = null;
  };
  for (const raw of text.split(/\r?\n/)) {
    if (!raw.trim() || /^\s*#/.test(raw)) continue; // blank + whole-line comments
    const indent = raw.search(/\S/);
    const line = raw.trim();
    if (indent === 0) {
      // A new top-level key ends any auth block we were inside.
      flush();
      inLoopServer = /^loop_server\s*:/.test(line);
      authIndent = -1;
      continue;
    }
    if (!inLoopServer) continue;
    if (authIndent >= 0 && indent <= authIndent) {
      // Dedented back to a sibling of `auth:` (e.g. `lease_ttl:`) — the list is over.
      flush();
      authIndent = -1;
    }
    if (authIndent < 0) {
      if (/^auth\s*:/.test(line)) authIndent = indent;
      continue;
    }
    if (line.startsWith("-")) {
      flush();
      cur = {};
    }
    const m = line.replace(/^-\s*/, "").match(/^([A-Za-z_][A-Za-z0-9_]*)\s*:\s*(.+)$/);
    if (m && cur) {
      cur[m[1]] = m[2]
        .replace(/\s+#.*$/, "") // trailing inline comment
        .trim()
        .replace(/^["']|["']$/g, "");
    }
  }
  flush();
  return users
    .filter((u) => u.token)
    .map((u) => ({
      token: u.token,
      tenant: u.tenant || "_default",
      subject: u.subject || "user",
    }));
}

const STATIC_DIR = __dirname;
const MIME = {
  ".html": "text/html; charset=utf-8",
  ".js": "text/javascript; charset=utf-8",
  ".css": "text/css; charset=utf-8",
  ".svg": "image/svg+xml",
  ".ico": "image/x-icon",
  ".png": "image/png",
};

// active tracks in-flight proxied Connect sockets (upstream request + client response) so
// the /__cut test control can forcibly sever an OPEN server-stream (Observe) mid-session —
// a deterministic "network kill" for the headless-browser reconnect CI lane. It has no
// effect in normal use (nothing calls /__cut).
const active = new Set();

const server = http.createServer((req, res) => {
  const url = new URL(req.url, "http://localhost");

  // Test control: forcibly drop every in-flight proxied Connect stream (a deterministic
  // mid-stream network kill). The SDK must classify this as TRANSIENT and reconnect.
  if (url.pathname === "/__cut") {
    let cut = 0;
    for (const entry of active) {
      try {
        entry.up.destroy();
      } catch {}
      try {
        entry.res.destroy();
      } catch {}
      cut++;
    }
    active.clear();
    res.writeHead(200, { "content-type": "application/json" });
    res.end(JSON.stringify({ cut }));
    return;
  }

  // Demo-only: the user picker's directory. Read per request so editing the demo config
  // does not need a restart. See DEMO_CONFIG's warning — this publishes those tokens.
  if (url.pathname === "/demo-users.json") {
    res.writeHead(200, { "content-type": "application/json", "cache-control": "no-store" });
    res.end(JSON.stringify(readDemoUsers(DEMO_CONFIG)));
    return;
  }

  // Reverse-proxy the Connect service path to the fuse backend (same-origin, no CORS).
  if (url.pathname.startsWith("/fuse.loop.v1.")) {
    const headers = { ...req.headers, host: UPSTREAM };
    const up = http.request(
      { host: UP_HOST, port: Number(UP_PORT), path: req.url, method: req.method, headers },
      (ur) => {
        res.writeHead(ur.statusCode || 502, ur.headers);
        ur.pipe(res); // un-buffered: server-streaming (Observe) frames flow through live.
      },
    );
    const entry = { up, res };
    active.add(entry);
    const done = () => active.delete(entry);
    res.on("close", done);
    up.on("close", done);
    up.on("error", (e) => {
      done();
      if (!res.headersSent) {
        res.writeHead(502, { "content-type": "application/json" });
        res.end(JSON.stringify({ error: "upstream unreachable: " + e.message }));
      } else {
        res.destroy();
      }
    });
    req.pipe(up); // stream the request body (unary POST) through.
    return;
  }

  // Static files.
  let p = url.pathname === "/" ? "/index.html" : url.pathname;
  const file = path.join(STATIC_DIR, path.normalize(p));
  // Path-traversal guard. Require the resolved path to be STATIC_DIR itself or strictly
  // BELOW it (prefix + separator) — a bare startsWith(STATIC_DIR) would also admit a sibling
  // whose name is a prefix of STATIC_DIR (e.g. `/app/wander-evil` for STATIC_DIR `/app/wander`).
  if (file !== STATIC_DIR && !file.startsWith(STATIC_DIR + path.sep)) {
    res.writeHead(403);
    res.end("forbidden");
    return;
  }
  fs.readFile(file, (err, data) => {
    if (err) {
      res.writeHead(404);
      res.end("not found");
      return;
    }
    res.writeHead(200, { "content-type": MIME[path.extname(file)] || "application/octet-stream" });
    res.end(data);
  });
});

server.listen(PORT, () => {
  console.log(`\n  Wander demo  ->  http://localhost:${PORT}`);
  console.log(`  proxying Connect /fuse.loop.v1.* to  http://${UPSTREAM}  (no CORS, no WS relay)`);
  const demoUsers = readDemoUsers(DEMO_CONFIG);
  console.log(`  demo user picker: ${demoUsers.length} principal(s) from ${DEMO_CONFIG}`);
  if (DEMO_CONFIG_OVERRIDDEN) {
    console.warn(
      `  ⚠ FUSE_DEMO_CONFIG is set: GET /demo-users.json will publish every bearer token in\n` +
        `    ${DEMO_CONFIG} to any browser that asks. Point it ONLY at a demo config.`,
    );
  }
  console.log("");
});
