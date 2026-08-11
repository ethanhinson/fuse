// @fuse/sdk — Runtime-parity, remote-only TypeScript/JS client for the fuse.loop.v1
// wire (change 0050). It presents the same StartLoop / Send / Observe surface the Go
// SDK does, over Connect: browser reach via @connectrpc/connect-web (the default
// transport), with a @connectrpc/connect-node transport injectable for the node test.
//
// Credential seam (#49): `credentials = { token, tenant }`. The token is sent as
// `Authorization: Bearer <token>` on every request (a client interceptor) and the
// tenant is threaded into every request message. Auth ENFORCEMENT is #49's business —
// this SDK only presents the seam.
//
// Observe is an async iterator over DOMAIN events: it skips keepalive frames
// (`frame.keepalive || !frame.event`), tracks `seq` (bigint), surfaces `gap` markers,
// and RECONNECTS transparently — when a stream ends or errors while the caller still
// wants events, it re-opens Observe(loopId, fromSeq = lastSeq). This is the browser
// analogue of treating every post-handshake read/close as a clean shutdown and
// resuming from the last-seen seq (no loss / no dup across the reconnect).

import {
  createClient as createConnectClient,
  type Interceptor,
  type Transport,
} from "@connectrpc/connect";
import { createConnectTransport } from "@connectrpc/connect-web";
import { timestampDate } from "@bufbuild/protobuf/wkt";

// The generated stub lives in the sibling `proto` workspace, which exposes no exports
// map, so it is imported by relative path (matching the smoke client's import_extension=js
// idiom). This keeps proto/package.json untouched.
import {
  LoopService,
  type Event as WireEvent,
} from "../../../proto/gen/ts/fuse/loop/v1/loop_pb.js";

/** Credentials presented on every request: a bearer token and the tenant field. */
export interface Credentials {
  token: string;
  tenant: string;
}

/** Options for {@link createClient}. */
export interface ClientOptions {
  /** Base URL of the fuse loop server (Connect over HTTP). */
  baseUrl: string;
  /** Bearer token + tenant threaded onto every request. */
  credentials: Credentials;
  /**
   * Optional transport override. Defaults to a browser `@connectrpc/connect-web`
   * transport at `baseUrl`. The node test injects a `@connectrpc/connect-node`
   * transport (which speaks HTTP/1.1 to the Go httptest server).
   */
  transport?: Transport;
}

/** Arguments for {@link Client.startLoop}. */
export interface StartLoopArgs {
  task: string;
  model: string;
  interactive?: boolean;
}

/**
 * A domain event as surfaced by {@link Client.observe}. Mirrors the wire `Event`, but
 * ergonomic: `payload` is the raw JSON bytes (per the proto contract), `gap` is lifted
 * from the enclosing frame so a reconnect hint travels with the event it precedes.
 */
export interface Event {
  seq: bigint;
  ts?: Date;
  nodeId: string;
  parentId: string;
  depth: number;
  turn: number;
  kind: string;
  payload: Uint8Array;
  /** true when a sequence gap preceded this event (re-observe hint). */
  gap: boolean;
}

/** The completion event kind for an interactive loop (see D5 in the plan). */
export const KIND_LOOP_PARKED = "loop.parked";

/**
 * isCompletion reports whether `event` is the loop's completion signal — an interactive
 * loop parking at a terminal turn boundary. Keyed on the real event kind
 * (`internal/event.KindLoopParked = "loop.parked"`), never inferred from stream shape.
 */
export function isCompletion(event: Event): boolean {
  return event.kind === KIND_LOOP_PARKED;
}

/** The remote-only fuse client surface. */
export interface Client {
  /** StartLoop constructs and drives one loop, returning its id. */
  startLoop(args: StartLoopArgs): Promise<{ loopId: string }>;
  /** Send injects human input at the loop's next turn boundary. */
  send(loopId: string, input: string): Promise<void>;
  /**
   * observe streams domain events from `fromSeq` (default 0), skipping keepalives and
   * reconnecting transparently from the last-seen seq on stream end/error. Break the
   * `for await` loop to stop observing (which closes the underlying stream).
   */
  observe(loopId: string, fromSeq?: bigint): AsyncIterable<Event>;
}

function bearerInterceptor(token: string): Interceptor {
  return (next) => (req) => {
    if (token) {
      req.header.set("Authorization", `Bearer ${token}`);
    }
    return next(req);
  };
}

// mergeAuthHeader returns a HeadersInit that carries the caller's headers plus the bearer
// token. Used to wrap an INJECTED transport (whose interceptors we cannot alter after
// construction) so the credential seam still sets Authorization on every request.
function mergeAuthHeader(token: string, header: HeadersInit | undefined): HeadersInit {
  const h = new Headers(header);
  if (token) {
    h.set("Authorization", `Bearer ${token}`);
  }
  return h;
}

// withBearer wraps an injected Transport so every unary/stream call carries the bearer
// header. The default transport instead takes the interceptor at construction.
function withBearer(transport: Transport, token: string): Transport {
  return {
    unary(method, signal, timeoutMs, header, input, contextValues) {
      return transport.unary(method, signal, timeoutMs, mergeAuthHeader(token, header), input, contextValues);
    },
    stream(method, signal, timeoutMs, header, input, contextValues) {
      return transport.stream(method, signal, timeoutMs, mergeAuthHeader(token, header), input, contextValues);
    },
  };
}

function toDomainEvent(wire: WireEvent, gap: boolean): Event {
  return {
    seq: wire.seq,
    ts: wire.ts ? timestampDate(wire.ts) : undefined,
    nodeId: wire.nodeId,
    parentId: wire.parentId,
    depth: wire.depth,
    turn: wire.turn,
    kind: wire.kind,
    payload: wire.payload,
    gap,
  };
}

/**
 * createClient builds a remote-only fuse client. It sets the bearer header (interceptor)
 * and threads the tenant onto every request. The default transport is browser
 * `connect-web`; the node test injects a `connect-node` transport.
 */
export function createClient(options: ClientOptions): Client {
  const { baseUrl, credentials, transport } = options;
  // Default transport = browser connect-web with the bearer interceptor baked in. When a
  // transport is injected (e.g. the node test's connect-node transport), wrap it so the
  // bearer header is still set on every request — the credential seam holds either way.
  const tport = transport
    ? withBearer(transport, credentials.token)
    : createConnectTransport({
        baseUrl,
        interceptors: [bearerInterceptor(credentials.token)],
      });
  const wire = createConnectClient(LoopService, tport);
  const tenant = credentials.tenant;

  return {
    async startLoop(args: StartLoopArgs): Promise<{ loopId: string }> {
      const resp = await wire.startLoop({
        task: args.task,
        model: args.model,
        interactive: args.interactive ?? false,
        tenant,
      });
      return { loopId: resp.loopId };
    },

    async send(loopId: string, input: string): Promise<void> {
      await wire.send({ loopId, input, tenant });
    },

    observe(loopId: string, fromSeq: bigint = 0n): AsyncIterable<Event> {
      return {
        async *[Symbol.asyncIterator](): AsyncGenerator<Event> {
          // last is the reconnect cursor: the highest seq delivered so far. On a stream
          // end/error we re-open Observe(fromSeq = last) so no event is lost or repeated.
          let last = fromSeq;
          // Reconnect loop. Each pass opens one server-stream; when it ends we resume
          // from `last`. A hard error that is not a clean end still triggers a resume —
          // the browser analogue of a websocket read error being a reconnect, not a fault.
          for (;;) {
            let streamEnded = false;
            try {
              for await (const frame of wire.observe({
                loopId,
                fromSeq: last,
                tenant,
              })) {
                if (frame.keepalive || !frame.event) {
                  continue; // idle heartbeat: no seq, ignore.
                }
                const ev = frame.event;
                if (ev.seq <= last) {
                  continue; // defensive dedup-at-watermark: never re-yield a seen seq.
                }
                last = ev.seq;
                yield toDomainEvent(ev, frame.gap);
              }
              streamEnded = true;
            } catch {
              // A stream error (post-handshake read/close) is a clean reconnect signal,
              // not a fault: fall through to re-open Observe(last).
              streamEnded = true;
            }
            if (streamEnded) {
              // Re-open from the last-seen seq. The server replays history since `last`
              // then live-tails; the watermark dedup above drops any overlap frame.
              continue;
            }
          }
        },
      };
    },
  };
}
