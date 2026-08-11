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
  Code,
  ConnectError,
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

/**
 * ConnState is the observed connection lifecycle of an {@link Client.observe} stream
 * (change 0056, D1). It is surfaced via the `onState` callback so an app can render a
 * connection indicator without reaching into the transport:
 *
 *   - `connecting`   — opening (or re-opening) the underlying Observe stream.
 *   - `live`         — a stream is delivering frames (set on the first delivered frame).
 *   - `reconnecting` — a stream ended/dropped (transient); backing off before a re-open.
 *   - `closed`       — the observe is finished: consumer stopped, aborted, or a terminal
 *                      error stopped the reconnect loop. No further states follow.
 */
export type ConnState = "connecting" | "live" | "reconnecting" | "closed";

/**
 * ObserveOptions is the options-object form of {@link Client.observe} (change 0056, D1/D3).
 * It is ADDITIVE: the positional `observe(loopId, fromSeq?)` form still works, so the
 * existing node test is untouched.
 */
export interface ObserveOptions {
  /** Resume cursor: the highest seq already seen (default 0 = from the start). */
  fromSeq?: bigint;
  /** Lifecycle callback fired at each {@link ConnState} transition. */
  onState?: (state: ConnState) => void;
  /**
   * AbortSignal for idempotent teardown (D3): aborting stops the reconnect loop, tears
   * the underlying stream down, and fires `onState('closed')` exactly once. Abort-after-
   * close / double-abort is a no-op.
   */
  signal?: AbortSignal;
}

/**
 * FuseTerminalError is thrown out of {@link Client.observe}'s async iterator when the
 * stream fails with a TERMINAL Connect code (change 0056, D2) — one the reconnect loop
 * must NOT retry: `unauthenticated` / `permission_denied` (auth rejected), `not_found`
 * (unknown loop) or `failed_precondition` (finished loop). It carries the Connect `code`
 * (a numeric `@connectrpc/connect` `Code`) so an app can render the right affordance
 * instead of hot-looping on a raw Connect error.
 */
export class FuseTerminalError extends Error {
  /** The terminal Connect `Code` that stopped the observe. */
  readonly code: Code;
  constructor(code: Code, message: string) {
    super(message);
    this.name = "FuseTerminalError";
    this.code = code;
  }
}

// TERMINAL_CODES is the D2 terminal set — the four Connect codes the loop handler
// (internal/loopconnect) emits that a reconnect can never recover from:
//   - Unauthenticated / PermissionDenied — auth rejected or tenant spoof / cross-owner;
//   - NotFound                            — the loop does not exist for this tenant;
//   - FailedPrecondition                  — the loop is finished (no longer accepts input).
// Everything else (a mid-stream network drop, a clean stream end) is TRANSIENT →
// reconnect, which ports websocket-read-errors-are-not-closeerror to the Connect-stream
// shape (a post-handshake read/close is a resume signal, not a fault).
const TERMINAL_CODES: ReadonlySet<Code> = new Set<Code>([
  Code.Unauthenticated,
  Code.PermissionDenied,
  Code.NotFound,
  Code.FailedPrecondition,
]);

// terminalCodeOf returns the terminal Connect Code if `err` is a ConnectError whose code
// is in the terminal set, else undefined (transient → reconnect).
function terminalCodeOf(err: unknown): Code | undefined {
  if (err instanceof ConnectError && TERMINAL_CODES.has(err.code)) {
    return err.code;
  }
  return undefined;
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
   *
   * Two additive forms (change 0056, D1):
   *   - positional `observe(loopId, fromSeq?)` — the original #50 surface (back-compat);
   *   - options `observe(loopId, { fromSeq?, onState?, signal? })` — surfaces the
   *     {@link ConnState} lifecycle via `onState` and idempotent teardown via `signal`.
   */
  observe(loopId: string, fromSeq?: bigint): AsyncIterable<Event>;
  observe(loopId: string, options: ObserveOptions): AsyncIterable<Event>;
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

/** sleep resolves after ms milliseconds — the reconnect-backoff delay. */
function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
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

    observe(loopId: string, arg?: bigint | ObserveOptions): AsyncIterable<Event> {
      // Normalize the two additive forms (D1): a bigint is the positional fromSeq; an
      // object is ObserveOptions. Undefined = from the start with no callbacks.
      const opts: ObserveOptions =
        typeof arg === "bigint" ? { fromSeq: arg } : (arg ?? {});
      const fromSeq = opts.fromSeq ?? 0n;
      const onState = opts.onState;

      // emitState fires the lifecycle callback (D1), guarding against a throwing consumer
      // callback taking down the reconnect loop.
      const emitState = (s: ConnState) => {
        if (!onState) return;
        try {
          onState(s);
        } catch {
          // A caller's onState must never break the stream: swallow its throw.
        }
      };

      return {
        async *[Symbol.asyncIterator](): AsyncGenerator<Event> {
          // last is the reconnect cursor: the highest seq delivered so far. On a stream
          // end/error we re-open Observe(fromSeq = last) so no event is lost or repeated.
          let last = fromSeq;
          // Reconnect backoff: a stream that ends and re-opens IMMEDIATELY (server down,
          // an instant error) would hot-spin the CPU and hammer the network. Back off
          // exponentially (capped) between re-opens, and RESET the backoff the moment a
          // re-opened stream delivers at least one frame — a healthy stream must not carry
          // a stale penalty from an earlier blip.
          const backoffBaseMs = 100;
          const backoffCapMs = 30_000;
          let backoffMs = backoffBaseMs;
          // Reconnect loop. Each pass opens one server-stream; when it ends we resume
          // from `last`. A hard error that is not a clean end still triggers a resume —
          // the browser analogue of a websocket read error being a reconnect, not a fault.
          try {
            for (;;) {
              let deliveredThisPass = false;
              // `connecting` at the top of every pass (initial open AND every re-open).
              emitState("connecting");
              // The Connect stream returned by wire.observe() is an AsyncIterable; a
              // `finally` on the for-await guarantees the underlying HTTP stream is torn
              // down whether this pass ends by completion, a thrown error, OR the CONSUMER
              // breaking out of the outer generator (which propagates return() to us).
              const stream = wire.observe({ loopId, fromSeq: last, tenant });
              try {
                for await (const frame of stream) {
                  if (frame.keepalive || !frame.event) {
                    continue; // idle heartbeat: no seq, ignore.
                  }
                  const ev = frame.event;
                  if (!deliveredThisPass) {
                    deliveredThisPass = true;
                    emitState("live"); // first frame of this pass: the stream is live.
                  }
                  if (ev.seq <= last) {
                    continue; // defensive dedup-at-watermark: never re-yield a seen seq.
                  }
                  last = ev.seq;
                  yield toDomainEvent(ev, frame.gap);
                }
                // Clean stream end (server closed the tail): fall through to re-open.
              } catch (err) {
                // D2 classification. A TERMINAL Connect code (auth rejected, unknown or
                // finished loop) can never be recovered by re-observing from `last`, so
                // STOP the loop and surface a typed FuseTerminalError (the `finally` fires
                // `closed`). Everything else is a transient post-handshake read/close — a
                // clean reconnect signal, not a fault (ported from
                // websocket-read-errors-are-not-closeerror) — so fall through to re-open.
                const code = terminalCodeOf(err);
                if (code !== undefined) {
                  const detail = err instanceof ConnectError ? err.rawMessage : String(err);
                  throw new FuseTerminalError(code, `fuse observe terminated: ${detail}`);
                }
              }
              // Re-open from the last-seen seq. The server replays history since `last`
              // then live-tails; the watermark dedup above drops any overlap frame.
              if (deliveredThisPass) {
                backoffMs = backoffBaseMs; // healthy stream: clear any accrued penalty.
              } else {
                await sleep(backoffMs);
                backoffMs = Math.min(backoffMs * 2, backoffCapMs);
              }
              // About to re-open: signal the reconnecting transition.
              emitState("reconnecting");
            }
          } finally {
            // The generator is done (consumer broke out, or a throw propagated): the
            // observe is closed. Fire `closed` exactly once on the way out.
            emitState("closed");
          }
        },
      };
    },
  };
}
