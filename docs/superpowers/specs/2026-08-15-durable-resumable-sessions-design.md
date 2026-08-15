<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0054 — Durable, resumable sessions — a conversation survives disconnect; refresh restores transcript + memory](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0054-durable-resumable-sessions.md)**
<!-- docket:backlink:end -->

# Durable, resumable sessions — design

Change: [#0054](../../changes/active/0054-durable-resumable-sessions.md) ·
Depends on #53 (done) · Related #47, #48, #49, #50 · ADRs consulted: 0031, 0033, 0034

## Problem

Change #53 made a conversation persist **across turns** on one `loop_id` (the loop parks at
each terminal turn boundary awaiting the next `Send`). But the parked session is pinned to the
liveness of **one connection**. The run context descends from the caller's request context —
`internal/runtime/inproc.go:221`, `loopCtx, loopSpan := r.deps.Observer.Start(ctx, …)`, and
`runCtx` is a child of `loopCtx` — so when a client disconnects and the transport cancels that
request ctx, the parked `a.Run(runCtx, …)` unwinds, the run goroutine marks the loop finished
in the durable registry (`Registry.SetLive(…, false)`, `inproc.go:388-392`), and a later `Send`
returns `ErrLoopFinished`. The in-memory model-facing `messages` transcript is discarded with
the goroutine.

**User-visible failure:** refresh the page (or reconnect from another device) and the
conversation is gone. This directly contradicts #53's north-star framing ("attach to your
running loop from your phone; a conversation as a first-class, resumable, server-side thing"),
which #53 consciously scoped out ("cross-instance session migration / durable park state …
deferred until 0049/0050 need it"). #0054 owns that deferred work.

The gap has two halves, because two different things are lost on disconnect:

1. **Event history** — already durable. `Attach(ctx, tenant, loopID, from) → Replay(from)` over
   the #47 durable store (ADR-0031) already survives disconnect, and the Connect
   `Observe(from_seq)` RPC (server-streaming history-then-live, ADR-0033) already replays it to
   a reconnecting client. This half needs no new persistence.
2. **The model-facing `messages` transcript + park state** — NOT durable. Lives only in the
   running goroutine's memory. Resuming requires (a) the session to *survive* the disconnect and
   (b) if it was already reaped/evicted, *rehydrating* the transcript and re-parking a loop for
   that `loop_id` so `Send` resumes instead of returning `ErrLoopFinished`.

## Reconciliation note — the wire moved under this stub

The stub was filed 2026-08-11 against the pre-#55 transport (`ServeWS`, `GET /loops/{id}/events`,
"WS owns all mutation", "REST resume surface"). Since then **ADR-0033 (change #55) replaced the
JSON-over-WebSocket + HTTP-replay wire with Connect/protobuf (`fuse.loop.v1`)**, superseding
ADR-0032. The stub's mechanism names are dead, but its **design decisions survive the remap** (cf.
learning `reconcile-transport-swapped-under-spec-remap-not-halt`): decouple session from connection,
persist/rehydrate transcript, re-park. This spec is written against **today's** reality —
`fuse.loop.v1` (`StartLoop`/`Send`/`Observe`) and the `runtime.Runtime` seam. The implementer's
just-in-time reconcile re-validates against `origin/main` at build time.

## Decisions

### D1 — Persistence model: rebuild from the event stream (no second source of truth)

The model-facing `messages` transcript is **reconstructed from the durable #47 event stream**, not
persisted as a separate artifact. The event stream is already durable and tenant-scoped (ADR-0031);
a second transcript store would be a parallel source of truth to keep consistent. Reconstruction is a
deterministic `events → []model.Message` fold (see D5).

**Delivery is the materialized final state, not step-by-step catch-up.** On resume the runtime folds
the whole durable stream into the final `messages` **once, server-side**, and seeds the re-parked
agent with it. The client is never asked to re-derive `messages` by replaying every intermediate
event to "catch up" to the final state. (This is the concrete form of learning
`persistent-loop-needs-explicit-completion-event`: state is handed over materialized, not inferred
from stream shape.) The client's existing `Observe(from_seq)` replay is for *its own view* of history
and is orthogonal to the server's transcript rehydration.

### D2 — Session lifetime severed from connection lifetime, bounded by an idle TTL

An interactive (persistent) loop gets a **session-scoped context** distinct from the request/connection
context, so a client disconnect no longer cancels the parked run. The session is bounded by an
**idle TTL** (no `Send` and no live `Observe` for the TTL window ⇒ reap: cancel the session ctx,
let the run finish and flip the registry to not-live). Default **30 minutes idle**; the value is a
single named constant, not yet a per-tenant knob (see Out of scope). One-shot (non-interactive)
loops are unchanged — they already own their run to completion and are not connection-pinned in a way
this changes.

### D3 — Resume surface: reuse the existing Connect stream + replay (no new wire RPC)

No new `fuse.loop.v1` RPC. Client resume is: re-open `Observe(loop_id, from_seq=last_seen)` — which
already replays durable history then tails live (ADR-0033) — and `Send` to continue. The **only**
new surface is a server-side runtime seam (D4); the wire is unchanged. This keeps the protobuf
surface stable and leans on primitives verified to exist (`Observe`, `Attach→Replay`, both
tenant-scoped).

**Ergonomics requirement (explicit):** a client must be able to resume by re-opening `Observe` from
its last-seen seq and calling `Send`, with no bespoke handshake. An acceptance test drives exactly
this client-visible sequence against the real server (D6, test 3) to prove it works end to end — not
just that the primitives exist in isolation.

### D4 — Rehydration runs in the runtime, before re-park (new `Resume` seam)

A new runtime seam, provisionally `Resume(ctx, tenant, loopID) (LoopHandle, error)` on
`runtime.Runtime`:

1. Resolve the durable `LoopRecord` for `(tenant, loopID)` (reusing `resolveDurable`, `inproc.go:471`),
   asserting tenant ownership (ADR-0034) — a cross-tenant resume is `ErrLoopNotFound`, never a leak
   (cf. learning `cache-over-tenant-scoped-source-reassert-key-on-hit`).
2. If the loop is **still live** on this instance (parked, session ctx alive), `Resume` is a no-op
   that returns the existing handle — resume just means "re-attach your `Observe`."
3. If the loop is **finished/evicted** (the disconnect-reaped or cold-instance case), replay the
   durable event store, fold `events → []model.Message` (D5) into the final transcript, construct a
   fresh agent **seeded with that transcript**, re-park it under a session-scoped context (D2), and
   re-arm the human injector so `Send` lands at the next turn boundary instead of returning
   `ErrLoopFinished`. Registry flips back to live for this instance under the owner-liveness lease
   (change #49 Task 7).

The runtime "never learns what the decorator stamps" identity invariant (ADR-0030) is preserved:
`Resume` derives its context through the same `deps.LoopContext` seam as `StartLoop`
(`inproc.go:363-366`), so per-principal tool egress holds for resumed turns too.

### D5 — Reconstruction is a pinned event-kind → message-role fold, round-trip tested

The `events → []model.Message` reconstruction is the correctness core. The spec pins the mapping;
the build implements and tests it:

| Event kind (`internal/event.Kind`) | Reconstructed message |
|---|---|
| human/user input (the `Send` payload, and the initial `cfg.Task`) | `{Role: "user", Content: <input>}` |
| assistant text / answer | `{Role: "assistant", Content: <text>}` |
| tool call | assistant message carrying the tool call (name + args), matching the live shape |
| tool result | `{Role: "tool", …}` (or the live transcript's tool-result shape) keyed to its call |
| loop lifecycle / park / keepalive / gap markers | **skipped** (not model-facing) |

The build enumerates the exact live `Kind` constants at implementation time (grep
`internal/event`), maps each model-facing one, and **explicitly skips** the rest. Fidelity is
guarded by a round-trip test (D6, test 1): run N real turns → capture the live in-memory
`messages` → reconstruct from that loop's durable events → assert **byte-equal** (feed the
reconstruction its own real production event source, per learning
`parity-test-feeds-each-side-its-own-production-source`; do not hand-synthesize events).

### D6 — Tests (TDD; the build writes these first)

1. **Reconstruction round-trip (unit).** Drive a persistent loop for ≥2 turns incl. a tool call;
   reconstruct `messages` from its durable events; assert equal to the live transcript. Covers D5.
2. **Disconnect-survives (runtime).** Start an interactive loop; cancel the *request* ctx (simulate
   disconnect); assert the parked loop is **still live** and a subsequent `Send` (via `Resume` /
   re-attach) drives another turn instead of `ErrLoopFinished`. Covers D2.
3. **Cold-resume acceptance (loopserver, real server).** Start → run a turn → drop the connection →
   finish/evict the loop → `Resume` → re-open `Observe(from_seq=0)` and see the full transcript
   replayed, then `Send` and get a new turn. Drive the **client-visible** RPC sequence against the
   real server (per learning `smoke-over-fake-backend-proves-wire-not-system` — the rigorous
   property test on the authoritative side, a loud acceptance against the real backend). Covers
   D3 + D4 ergonomics.
4. **Idle-reap (runtime).** With a short test TTL, a session with no `Send`/`Observe` is reaped
   (registry not-live, session ctx canceled) without leaking the run goroutine or its store handle
   (cf. learning `per-instance-resource-needs-teardown-on-every-early-return`). Covers D2's bound.

## What changes (scope)

- `internal/runtime`: sever the interactive-loop run context from the request ctx (session-scoped
  ctx + idle-TTL reaper); add the `Resume` seam; add the `events → []model.Message` reconstruction.
- `internal/loopserver`: route a resume (a `Send`/`Observe` against a finished-but-resumable loop)
  through `Resume` before returning `ErrLoopFinished`; the wire is unchanged (D3).
- Tests per D6.

## Out of scope

- **Per-tenant / configurable idle-TTL policy** — land a single named constant now; a
  configurable/tenant-aware timeout is a follow-up. (D2.)
- **Tenant/ownership model itself** — consumed from #49 (ADR-0034), not redefined.
- **Client-SDK session ergonomics** (versioned session envelope, resume helpers) — #50 wraps
  whatever primitive this lands.
- **Streaming token deltas** for the restored view — independent (#53 out-of-scope too).
- **New `fuse.loop.v1` RPCs** — explicitly avoided (D3).

## Open questions for the reconcile pass

- Exact `internal/event.Kind` constants and the live `messages` tool-call/tool-result shape must be
  read from `origin/main` at build time and mapped per D5 — the table is by role, not by a frozen
  constant list.
- Whether the idle-TTL reaper is a per-loop timer or a single sweep goroutine is a build-time
  implementation choice; the observable contract (D2, test 4) is what's pinned.
