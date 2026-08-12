#!/usr/bin/env node
// Tiny zero-dependency launcher for the Wander concierge demo (change 0056).
//
// Two jobs, NO WebSocket relay (unlike the older concierge-demo) — the SDK speaks Connect
// DIRECTLY over connect-web; this server only keeps the browser same-origin:
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

const http = require("http");
const fs = require("fs");
const path = require("path");

const PORT = Number(process.env.PORT || 5173);
const UPSTREAM = process.env.FUSE_NET_ADDR || "127.0.0.1:8787";
const [UP_HOST, UP_PORT] = UPSTREAM.split(":");

const STATIC_DIR = __dirname;
const MIME = {
  ".html": "text/html; charset=utf-8",
  ".js": "text/javascript; charset=utf-8",
  ".css": "text/css; charset=utf-8",
  ".svg": "image/svg+xml",
  ".ico": "image/x-icon",
};

const server = http.createServer((req, res) => {
  const url = new URL(req.url, "http://localhost");

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
    up.on("error", (e) => {
      res.writeHead(502, { "content-type": "application/json" });
      res.end(JSON.stringify({ error: "upstream unreachable: " + e.message }));
    });
    req.pipe(up); // stream the request body (unary POST) through.
    return;
  }

  // Static files.
  let p = url.pathname === "/" ? "/index.html" : url.pathname;
  const file = path.join(STATIC_DIR, path.normalize(p));
  if (!file.startsWith(STATIC_DIR)) {
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
  console.log(`  proxying Connect /fuse.loop.v1.* to  http://${UPSTREAM}  (no CORS, no WS relay)\n`);
});
