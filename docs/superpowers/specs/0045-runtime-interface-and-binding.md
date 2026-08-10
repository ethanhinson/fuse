<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0045 — Runtime interface + second binding — prove the platform boundary is emergent](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/archive/2026-08-10-0045-runtime-interface-and-binding.md)**
<!-- docket:backlink:end -->

# Spec 0045 — Runtime interface + second binding: prove the platform boundary is emergent

## Problem

The two prior changes built the engine primitives a portable "agentic loop runtime"
needs — a typed event stream (0043) and a handle-returning, location-transparent spawn
seam (0044) — but **nothing yet names the boundary** those primitives are supposed to
form, and **only one consumer (the CLI) drives them**. The whole thesis of the trilogy —
*the engine sits behind a small, policy-free interface, and every integration (mobile app,
website, Claude/Codex/Cursor) is just a binding over it* — is unproven until (a) the
interface is a real, named Go type, and (b) at least **two** distinct bindings drive that
identical interface. One binding always leaks, because you shape the seam to it; two
bindings is the minimum that forces it to stay policy-free.

Today the engine is reachable only as **in-process Go calls threaded through three cmd-site
builders** (`main.go` one-shot, `shell.go`, `research_probe.go`), each independently wiring
`agent.New` + post-`New` setters + `Agent.Run`, `Spawner.Spawn`, and `EventStore`. There is
no `Runtime` type; there is no surface where something *other than the CLI* can start a
loop, send it input, spawn into it, or observe it. The existing MCP server
(`cmd/fuse/mcp_server.go`) is **tool-call only** (`tools/call`) — a model using fuse's
tools, not a client driving a fuse loop.

This is the **third and final change** of the Runtime-seam trilogy. It depends on **0044**
(merged): `Spawn` returns the handle 0044 exposed, and `Observe`/`Attach` read the event
stream 0043 shipped and 0044 wired spawn lifecycle onto.

1. `runtime-eventstore-seam` (0043, done) — the typed event stream.
2. `spawn-handle-async` (0044, done) — handle-returning spawn; spawn.start/spawn.done on the stream.
3. **`runtime-interface-and-binding` (this change)** — name the `Runtime` interface, migrate
   the CLI to consume it, and stand up a second binding to prove policy-freedom.

## Scope of this change (hard boundaries)

**In scope.**
- A named `Runtime` interface (`StartLoop`/`Send`/`Spawn`/`Observe`/`Attach`) extracted over
  what already exists, with one concrete in-process implementation.
- **Migrate the CLI (binding #1)** — the three cmd-site builders — to construct and drive the
  engine *through* `Runtime` rather than by direct in-process calls. This is what makes "two
  bindings over one interface" true rather than half-true.
- **Binding #2: a new `fuse loop-server` subcommand** — a dedicated stdio MCP loop-control
  surface exposing `loop.start` / `loop.send` / `loop.observe`, driving the identical
  `Runtime`. Distinct entrypoint from the tool-call `mcp-server`.
- `Observe` delivered two ways: **live subscribe (server-push) + replay catch-up** — the
  durable-reattach story.

**Out of scope — deliberate non-goals.**
- **A networked spawn backend.** Still in-process. The `Runtime` interface must merely
  *permit* a remote spawn backend later (async handle, serializable request) — no transport
  is built.
- **Modifying the existing tool-call MCP server** (`cmd/fuse/mcp_server.go`). Binding #2 is a
  **separate** `fuse loop-server` subcommand; the tool-call surface stays byte-untouched.
- **Authentication / multi-tenancy on the loop-server.** Single-tenant stdio, same trust model
  as the existing mcp-server. The "platform / many users" story is later work.
- **A TUI or renderer for binding #2.** The point is that the seam carries none — binding #2 is
  headless, proving the seam is policy-free.

## Decisions

### D1 — the `Runtime` interface, extracted over what exists

```go
// internal/runtime — the named seam. Every binding is a client of this.
type Runtime interface {
    // StartLoop constructs a loop (agent + event store + node identity) and runs it.
    // Wraps agent.New + SetEventSink/SetNodeIdentity/EnableSummarization + Agent.Run.
    // Returns a handle whose ID is the addressable session id (tree.RootID()).
    StartLoop(ctx context.Context, cfg LoopConfig) (LoopHandle, error)

    // Send injects human input at a turn boundary — the ADR-0022 human-bus queue.
    Send(ctx context.Context, loopID string, input string) error

    // Spawn fans out a child under a loop — wraps Spawner.Spawn (0044 handle).
    Spawn(ctx context.Context, loopID string, opts SpawnOpts) (SpawnHandle, error)

    // Observe live-tails a loop's event stream — wraps EventStore.Subscribe.
    Observe(loopID string) (<-chan event.Event, func(), error)

    // Attach replays durable history from a cursor — wraps EventStore.Replay.
    // Observe + Attach together are the reattach-after-disconnect contract.
    Attach(loopID string, from event.Seq) ([]event.Event, error)
}
```

- **`loopID` is `tree.RootID()`** — the existing stable per-session identifier
  (`internal/agent/tree.go`), already the key for the per-session dir, event store, and
  segment sink. No new identity is minted.
- The interface lives in a new package (proposed `internal/runtime`) that composes
  `internal/agent`, `internal/event`, and the scheduler — it is the composition seam, so it
  may import the heavy packages (unlike the agent-free leaf packages of 0043/0044).
- **Policy-free:** the interface names loop mechanics only. No renderer, no TUI, no approval
  gate, no MCP/CLI vocabulary appears in it. Approval gating, rendering, and transport live in
  the *bindings*, not the seam — that is precisely what two bindings proves.
- **Location-transparency preserved:** `Spawn` returns a handle (0044), `StartLoop` returns a
  handle keyed by a string id — both shapes already permit a future out-of-process
  implementation behind the same interface.

### D2 — migrate the CLI to consume `Runtime` (binding #1)

The three cmd-site builders stop calling `agent.New` + setters + `Agent.Run` /
`Spawner.Spawn` directly and instead construct a `Runtime` (the in-process impl) and call
`StartLoop` / `Spawn` / `Observe`. The **renderer, TUI, and approval gate stay in the cmd
layer** — they wrap the binding, they are not passed into the seam. The shared builder
(`cmd/fuse/run.go` `buildAgentCore` / `spawnFuncFrom`) becomes the construction of the
in-process `Runtime`, consumed identically by all three cmd entrypoints. This migration is
the load-bearing proof: if the CLI can be re-expressed as a pure `Runtime` client with all
its policy pushed to the binding layer, the seam is genuinely policy-free.

### D3 — binding #2: a `fuse loop-server` subcommand (separate from `mcp-server`)

A new stdio MCP subcommand — `fuse loop-server` — dedicated to **loop control**, leaving
`cmd/fuse/mcp_server.go` byte-untouched. It exposes:

- `loop.start(task, model?) → { loop_id }` — `Runtime.StartLoop`; returns `tree.RootID()`.
- `loop.send(loop_id, input)` — `Runtime.Send` (ADR-0022 human-bus injection).
- `loop.observe(loop_id, from_seq?)` — see D4.

It is a **pure adapter from MCP JSON-RPC to `Runtime`** — no approval gate, no renderer, no
TUI. That headlessness is the proof: the same engine that runs the CLI runs here with *zero*
agent-specific policy, reached only through the interface.

### D4 — `Observe` over MCP: live subscribe + replay catch-up (both)

`loop.observe` supports both halves of a durable reattach:

- **Live tail (server-push):** while subscribed, events fire to the client via MCP
  **notifications** (the same mechanism the tool-call server already uses for
  `resources/updated`), sourced from `Runtime.Observe` → `EventStore.Subscribe`.
- **Replay catch-up:** `loop.observe(loop_id, from_seq)` first returns durable history since
  `from_seq` via `Runtime.Attach` → `EventStore.Replay`, then continues live. A client that
  disconnects and reconnects passes its last-seen `Seq` and misses nothing.

Together these are the concrete "attach to a running loop from your mobile app" contract —
and they exercise **both** `EventStore.Subscribe` and `Replay`, the strongest proof the
stream is binding-agnostic and reattachable. Back-pressure remains ADR-0025 (non-blocking
drop-newest-with-gap): a slow MCP client can never wedge the loop; a gap marker tells it to
`Attach`-replay to recover.

## Open questions (resolve during planning)

- **Runtime impl & the process-global holders.** Today the event store and segment sink are
  installed via process-global lock-guarded holders (`setActiveEventStore`, ADR-0019 pattern).
  Should the in-process `Runtime` own these as instance state instead of globals — cleaner for
  a server that may host multiple loops — or keep the globals for this change and defer
  de-globalization? Lean: `Runtime` owns them as instance state (a server hosting >1 loop can't
  share one process-global store), but scope the blast radius carefully; a full de-globalization
  may warrant its own change. **Record an ADR for the Runtime seam + this decision.**
- **Multiple concurrent loops in one `loop-server` process.** Does `loop.start` support N live
  loops keyed by `loop_id` (the server as a mini multi-loop host), or one loop per process for
  this change? Multi-loop is the real platform story but interacts with the global-holder
  question above. Lean: design the interface for N (loopID-keyed), implement 1-or-N as the
  holder decision allows.
- **`Send` semantics through the human-bus.** ADR-0022's human-bus injects at turn boundaries
  via a per-node queue. Confirm `Runtime.Send` maps to that path cleanly for a headless binding
  (no TUI to originate the message) and that a message sent to an idle/finished loop is handled
  (queued vs rejected).
- **Approval gating in binding #2.** The tool-call mcp-server relays approvals to a parent TUI
  via `FUSE_HITL_SOCKET` or auto-approves. For headless `loop-server`, is auto-approve correct
  (it's the "policy lives in the binding" stance — this binding's policy is "no gate"), or does
  it need a relay? Lean: auto-approve for this change (headless by design), documented as the
  binding's policy choice, not the seam's.

## Verification

- **Interface is real & CLI consumes it:** all three cmd entrypoints construct and drive the
  engine through `Runtime`; grep confirms no cmd-site calls `agent.Run`/`Spawner.Spawn`
  directly outside the `Runtime` impl. The existing CLI/shell/research-probe behavior is
  unchanged (same output, same spawn results, same events).
- **Two bindings, one seam:** a test drives a loop via `fuse loop-server` (`loop.start` →
  `loop.spawn`/model turn → `loop.observe`) and asserts the **same** `event.Event` stream a CLI
  run produces — proving the events are binding-agnostic.
- **Reattach works:** a client `loop.observe`s live, disconnects, reconnects with its last
  `from_seq`, and receives the intervening events via replay then resumes live — no gap, no
  duplication.
- **Policy-free seam:** the `internal/runtime` interface imports no renderer/TUI/MCP type; the
  approval gate, renderer, and transport exist only in bindings. A reviewer can confirm the seam
  names loop mechanics only.
- **Out-of-scope untouched:** `cmd/fuse/mcp_server.go` (tool-call surface) is byte-identical;
  spawn remains in-process; `-race ./...` green.
