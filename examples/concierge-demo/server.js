#!/usr/bin/env node
// Tiny zero-dependency launcher for the Concierge demo.
//
// It does two jobs:
//   1. Serves the static web app (index.html / app.js / styles.css).
//   2. Proxies to the fuse `loop-serve-net` binding (binding #3) so the browser
//      never needs CORS and never needs to know the backend's real address:
//        - GET  /api/health            -> is the fuse net binding reachable?
//        - WS   /ws                    -> piped to fuse ws://HOST/ws (full loop.* session)
//        - GET  /loops/{id}/events     -> piped to fuse HTTP replay endpoint
//
// The upstream fuse address is taken from FUSE_NET_ADDR (default 127.0.0.1:8787).
// This file is intentionally dependency-free: it hand-implements the small slice of
// the WebSocket protocol (RFC 6455) needed to relay text frames in both directions,
// so `node server.js` runs with nothing installed.

const http = require('http');
const net = require('net');
const crypto = require('crypto');
const fs = require('fs');
const path = require('path');

const PORT = Number(process.env.PORT || 5173);
const UPSTREAM = process.env.FUSE_NET_ADDR || '127.0.0.1:8787';
const [UP_HOST, UP_PORT] = UPSTREAM.split(':');

const STATIC_DIR = __dirname;
const MIME = {
  '.html': 'text/html; charset=utf-8',
  '.js': 'text/javascript; charset=utf-8',
  '.css': 'text/css; charset=utf-8',
  '.svg': 'image/svg+xml',
  '.ico': 'image/x-icon',
};

// ---------------------------------------------------------------- static + REST

const server = http.createServer((req, res) => {
  const url = new URL(req.url, `http://localhost`);

  if (url.pathname === '/api/health') {
    const probe = net.connect(Number(UP_PORT), UP_HOST, () => {
      probe.destroy();
      res.writeHead(200, { 'content-type': 'application/json' });
      res.end(JSON.stringify({ ok: true, upstream: UPSTREAM }));
    });
    probe.on('error', () => {
      res.writeHead(200, { 'content-type': 'application/json' });
      res.end(JSON.stringify({ ok: false, upstream: UPSTREAM }));
    });
    return;
  }

  // Proxy the HTTP replay endpoint straight through to fuse.
  if (url.pathname.startsWith('/loops/')) {
    const opts = { host: UP_HOST, port: Number(UP_PORT), path: req.url, method: req.method, headers: { host: UPSTREAM } };
    const up = http.request(opts, (ur) => {
      res.writeHead(ur.statusCode || 502, ur.headers);
      ur.pipe(res);
    });
    up.on('error', (e) => {
      res.writeHead(502, { 'content-type': 'application/json' });
      res.end(JSON.stringify({ error: 'upstream unreachable: ' + e.message }));
    });
    req.pipe(up);
    return;
  }

  // Static files.
  let p = url.pathname === '/' ? '/index.html' : url.pathname;
  const file = path.join(STATIC_DIR, path.normalize(p));
  if (!file.startsWith(STATIC_DIR)) { res.writeHead(403); res.end('forbidden'); return; }
  fs.readFile(file, (err, data) => {
    if (err) { res.writeHead(404); res.end('not found'); return; }
    res.writeHead(200, { 'content-type': MIME[path.extname(file)] || 'application/octet-stream' });
    res.end(data);
  });
});

// ------------------------------------------------------------- WS relay (RFC 6455)
//
// The browser opens ws://localhost:PORT/ws. We complete the handshake with the
// browser, dial a raw TCP socket to the fuse net binding, perform the client-side
// WebSocket handshake against it, then relay decoded text frames both ways. Only
// text frames + close + ping/pong are handled — that is the entire loop.* protocol.

const GUID = '258EAFA5-E914-47DA-95CA-C5AB0DC85B11';

server.on('upgrade', (req, browserSock, head) => {
  const url = new URL(req.url, 'http://localhost');
  if (url.pathname !== '/ws') { browserSock.destroy(); return; }

  // 1) Complete handshake with the browser.
  const key = req.headers['sec-websocket-key'];
  const accept = crypto.createHash('sha1').update(key + GUID).digest('base64');
  browserSock.write(
    'HTTP/1.1 101 Switching Protocols\r\n' +
    'Upgrade: websocket\r\n' +
    'Connection: Upgrade\r\n' +
    `Sec-WebSocket-Accept: ${accept}\r\n\r\n`
  );

  // 2) Dial fuse and perform the client-side handshake against ws://UPSTREAM/ws.
  const upstream = net.connect(Number(UP_PORT), UP_HOST, () => {
    const upKey = crypto.randomBytes(16).toString('base64');
    upstream.write(
      `GET /ws HTTP/1.1\r\n` +
      `Host: ${UPSTREAM}\r\n` +
      `Upgrade: websocket\r\n` +
      `Connection: Upgrade\r\n` +
      `Sec-WebSocket-Key: ${upKey}\r\n` +
      `Sec-WebSocket-Version: 13\r\n\r\n`
    );
  });

  let upstreamReady = false;
  let upstreamBuf = Buffer.alloc(0);
  const pending = []; // browser text frames waiting for the upstream handshake

  const relay = (from, to, maskOut) => {
    let buf = Buffer.alloc(0);
    from.on('data', (chunk) => {
      buf = Buffer.concat([buf, chunk]);
      let frame;
      while ((frame = decodeFrame(buf)) !== null) {
        buf = buf.slice(frame.total);
        if (frame.opcode === 0x8) { to.end(encodeFrame(Buffer.alloc(0), 0x8, maskOut)); from.end(); return; }
        if (frame.opcode === 0x9) { from.write(encodeFrame(frame.payload, 0xA, from === browserSock ? false : true)); continue; } // ping->pong on same sock
        if (frame.opcode === 0x1 || frame.opcode === 0x0) {
          to.write(encodeFrame(frame.payload, 0x1, maskOut));
        }
      }
    });
    from.on('close', () => to.end());
    from.on('error', () => to.destroy());
  };

  upstream.once('data', (chunk) => {
    upstreamBuf = Buffer.concat([upstreamBuf, chunk]);
    const idx = upstreamBuf.indexOf('\r\n\r\n');
    if (idx === -1) return;
    upstreamReady = true;
    const rest = upstreamBuf.slice(idx + 4);
    // Relay upstream(server, unmasked) -> browser(unmasked). Browser->upstream must be masked.
    relay(upstream, browserSock, false);
    relay(browserSock, upstream, true);
    // flush any buffered browser frames captured before the handshake finished
    for (const f of pending) upstream.write(encodeFrame(f, 0x1, true));
    pending.length = 0;
    if (rest.length) upstream.emit('data', rest);
  });

  // Buffer browser frames that arrive before the upstream handshake is done.
  let earlyBuf = Buffer.alloc(0);
  const earlyHandler = (chunk) => {
    if (upstreamReady) { browserSock.removeListener('data', earlyHandler); return; }
    earlyBuf = Buffer.concat([earlyBuf, chunk]);
    let frame;
    while ((frame = decodeFrame(earlyBuf)) !== null) {
      earlyBuf = earlyBuf.slice(frame.total);
      if (frame.opcode === 0x1 || frame.opcode === 0x0) pending.push(frame.payload);
    }
  };
  browserSock.on('data', earlyHandler);

  upstream.on('error', () => browserSock.destroy());
  browserSock.on('error', () => upstream.destroy());
});

// decodeFrame returns {opcode, payload, total} for one complete frame, or null if
// more bytes are needed. Handles client-masked and server-unmasked frames.
function decodeFrame(buf) {
  if (buf.length < 2) return null;
  const opcode = buf[0] & 0x0f;
  const masked = (buf[1] & 0x80) !== 0;
  let len = buf[1] & 0x7f;
  let off = 2;
  if (len === 126) { if (buf.length < off + 2) return null; len = buf.readUInt16BE(off); off += 2; }
  else if (len === 127) { if (buf.length < off + 8) return null; len = Number(buf.readBigUInt64BE(off)); off += 8; }
  let mask;
  if (masked) { if (buf.length < off + 4) return null; mask = buf.slice(off, off + 4); off += 4; }
  if (buf.length < off + len) return null;
  let payload = buf.slice(off, off + len);
  if (masked) { payload = Buffer.from(payload); for (let i = 0; i < payload.length; i++) payload[i] ^= mask[i & 3]; }
  return { opcode, payload, total: off + len };
}

// encodeFrame builds a single unfragmented frame. When mask=true (client->server),
// a random mask is applied as RFC 6455 requires for client frames.
function encodeFrame(payload, opcode, mask) {
  const len = payload.length;
  let header;
  if (len < 126) header = Buffer.from([0x80 | opcode, (mask ? 0x80 : 0) | len]);
  else if (len < 65536) { header = Buffer.alloc(4); header[0] = 0x80 | opcode; header[1] = (mask ? 0x80 : 0) | 126; header.writeUInt16BE(len, 2); }
  else { header = Buffer.alloc(10); header[0] = 0x80 | opcode; header[1] = (mask ? 0x80 : 0) | 127; header.writeBigUInt64BE(BigInt(len), 2); }
  if (!mask) return Buffer.concat([header, payload]);
  const m = crypto.randomBytes(4);
  const out = Buffer.from(payload);
  for (let i = 0; i < out.length; i++) out[i] ^= m[i & 3];
  return Buffer.concat([header, m, out]);
}

server.listen(PORT, () => {
  console.log(`\n  Concierge demo  →  http://localhost:${PORT}`);
  console.log(`  proxying fuse binding #3 at  ws://${UPSTREAM}/ws  (+ HTTP replay)\n`);
});
