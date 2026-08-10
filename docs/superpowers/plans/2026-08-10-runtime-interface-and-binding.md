<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0045 — Runtime interface + second binding — prove the platform boundary is emergent](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0045-runtime-interface-and-binding.md)**
<!-- docket:backlink:end -->

# Implementation Plan — 0045 Runtime Interface and Binding

## Goal

Introduce a named `Runtime` interface in a new package `internal/runtime` that names loop
mechanics only (start / send / spawn / observe / attach), one concrete in-process
implementation, migrate all three CLI command builders to drive the engine *through* that
interface (binding #1), and add a headless stdio MCP `fuse loop-server` subcommand as a
second, policy-different binding (#2). The two bindings must produce the identical
`event.Event` stream for the same run, proving the seam is policy-free and reusable.

Out of scope (do NOT implement): networked spawn backend, any change to
`cmd/fuse/mcp_server.go`, auth / multi-tenancy on loop-server, a TUI/renderer for binding
#2, and full de-globalization / a multi-loop Seq allocator.

## Architecture

```
                       cmd/fuse (composition root — the ONLY importer of internal/runtime)
        ┌───────────────────────────┬──────────────────────────────┐
   binding #1 (CLI)            binding #1 (CLI)                binding #2 (loop-server)
   one-shot / shell /          renderer + TUI +                MCP JSON-RPC adapter,
   research-probe              approval gate live HERE          auto-approve, notifications
        │                           │                                │
        └───────────── construct + call ────────────────────────────┘
                                    │
                        internal/runtime.Runtime  (the composition seam — policy-free)
                          composes agent + event + scheduler
                                    │
        ┌───────────────┬───────────┴────────────┬──────────────────┐
   internal/agent   internal/event          internal/event/fsstore  (scheduler = agent.Scheduler)
```

Import direction is load-bearing (learning `break-import-cycle-with-agent-free-subpackage`):
`internal/runtime` imports the heavy packages (`internal/agent`, `internal/event`,
`internal/event/fsstore`, `internal/model`, `internal/tools`) and is imported **only** by
`cmd/fuse`. It must NEVER be imported by `internal/agent` or `internal/tools` — that would
form an import cycle. A build+grep verification step enforces this.

The `loopID` is `(*agent.AgentTree).RootID()` — no new identity is minted. The interface is
designed for N loops (all methods key on `loopID`, the impl holds `loopID -> loop` maps), but
this change IMPLEMENTS single-loop-per-process because the per-session-global Seq allocator
(ADR-0025) plus the process-global holders (ADR-0019) assume one process ⇒ one session. The
Runtime impl OWNS those holders as instance state where feasible (see Decision Note D-1).

## Tech Stack

- Language: Go 1.26 (module `github.com/ethanhinson/fuse`).
- Tests: `go test ./...`. Race gate: `make test-race` (`go test -race ./...`).
- No new third-party dependencies. JSON-RPC framing reuses `encoding/json` exactly as
  `internal/mcp/server.go` does.

## Global Constraints

- TDD, bite-sized: every code step is (write failing test → run to see it fail → minimal
  impl → run to pass → commit). Each task ends in an independently testable deliverable.
- Byte-equivalence: the `event.Event` stream emitted after the CLI migration must be
  identical to before it (learning `relocated-emission-resource-every-projected-field`); the
  two bindings must emit the identical stream for the same run
  (learning `parity-test-feeds-each-side-its-own-production-source`).
- Policy-free seam: `internal/runtime` imports NO renderer / TUI / MCP / CLI type. Grep-
  asserted.
- `cmd/fuse/mcp_server.go` stays byte-identical (verified with `git diff --exit-code`).
- Real-binary verification for loop mechanics uses a scripted `LLM_GATEWAY_URL` httptest
  double (learning `verify-tool-loop-at-gateway-seam`); a fake Completer/renderer never
  exercises `cmd/fuse` wiring.
- Best-effort emission everywhere (ADR-0016): a slow/absent observer never wedges the loop
  (ADR-0025 non-blocking drop-newest-with-gap is preserved end to end).

## Locked signatures (verified in tree — do not re-guess)

```go
// internal/agent
func New(m Completer, t ToolExecutor, r Renderer, modelID, systemPrompt string, maxTurns, maxTokens int) *Agent
func (a *Agent) SetEventSink(s event.EventStore)
func (a *Agent) SetNodeIdentity(nodeID, parentID string, depth int)
func (a *Agent) EnableSummarization(c Completer, modelID string, maxOutput int, sink SegmentSink)
func (a *Agent) SetHumanInjector(inj *HumanInjector)
func (a *Agent) SetStripSpawn(fn func() bool)
func (a *Agent) SetExpects(schema map[string]any, sink *ExpectsSink)
func (a *Agent) Run(ctx context.Context, history []model.Message) ([]model.Message, error)   // loop.go
func NewAgentTreeWithConcurrency(rootLabel, rootModel string, maxConcurrent int) *AgentTree
func (t *AgentTree) RootID() string
func (t *AgentTree) Node(id string) *AgentNode
func (t *AgentTree) Scheduler() *Scheduler
func NewSpawner(opts ...Option) *Spawner
func (s *Spawner) Spawn(ctx context.Context, opts SpawnOpts) (AgentHandle, error)
type AgentHandle struct { NodeID string; Done <-chan SpawnDone; /* memo */ }
func (h *AgentHandle) Wait() SpawnDone
func (h *AgentHandle) Result() (any, error)
type SpawnDone struct { Result string; Err error; Structured any }
func NewHumanBus(tree *AgentTree) *HumanBus
func (b *HumanBus) Enqueue(nodeID string, mode MsgMode, handle, text string) HumanMsg
const ModeRespond MsgMode = iota; const ModeBroadcast

// internal/event
type Seq uint64
type Event struct { Seq Seq; TS time.Time; NodeID, ParentID string; Depth, Turn int; Kind Kind; Payload json.RawMessage }
type EventStore interface {
    Append(Event) error
    Subscribe() (<-chan Event, func())
    Replay(from Seq) ([]Event, error)
}
type NoopStore struct{}

// internal/event/fsstore
func NewFSEventStore(baseDir, sessionID string) (*FSEventStore, error)
func (s *FSEventStore) Close() error
// Seq is a per-instance field (s.seq), NOT process-global — one store per loop is clean.

// cmd/fuse/run.go (existing shared builder)
func buildAgentCore(cfg config.Config, reg *model.Registry, alias string, r agent.Renderer,
    extra string, traceW io.Writer, traceLabel string, toolReg *tools.Registry,
    approve permissions.ApprovalFunc, sm *permissions.SessionMode, interactive bool,
    gate model.RateGate) (*agent.Agent, string, error)
func spawnFuncFrom(spawner *agent.Spawner, sched *agent.Scheduler, parentNode *agent.AgentNode) tools.SpawnFunc
// process-global holders: setActiveEventStore/currentEventStore, setActiveSegmentSink/currentSegmentSink
```

## Decision Notes (implementer captures the ADR at review time; docket-adr)

- **D-1 — Runtime owns holders as instance state vs keeps globals.** The process-global
  `activeEventStore`/`activeSegmentSink` holders in `cmd/fuse/run.go` exist because ADR-0019
  wired a single per-process session store, resting on ADR-0025's one-process ⇒ one-session
  Seq allocator. This change scopes the holders to the Runtime impl **as instance state where
  feasible**: the in-process Runtime constructs its own `*fsstore.FSEventStore` and its own
  `SegmentSink`, and holds them on the `inProcRuntime` struct. Because the cmd-site child
  builders still call the package-level `currentEventStore()`/`currentSegmentSink()` (read on
  child-spawn goroutines), the Runtime impl *also* installs its store via
  `setActiveEventStore`/`setActiveSegmentSink` at `StartLoop` time so those readers see it —
  the holders stay as a compatibility bridge, but the *owner of the lifecycle* is now the
  Runtime instance (it opens and closes the store). A full de-globalization (each loop's
  builders reading from the Runtime instance instead of the package global, and a multi-loop
  Seq allocator) is explicitly a LATER change. **Blast radius:** the holders and their two
  writers move ownership; the ~six `currentEventStore()` reader sites in the three builders
  are unchanged. Record this as an ADR ("Runtime owns the event/segment store lifecycle;
  package holders remain a single-loop compatibility bridge").

- **D-2 — binding #2 hosts loop.* as MCP tools, not custom JSON-RPC methods.** Investigation:
  `internal/mcp.Server` (server.go) is a closed struct with a fixed `dispatch` switch
  (initialize / tools/list / tools/call / resources/*) and NO exported hook to register
  arbitrary JSON-RPC methods; the only server→client notification primitive is
  `PushResourceUpdated` (server_resources.go), gated on a `resources/subscribe` to
  `fuse://tools`. Therefore `fuse loop-server` does NOT reuse `internal/mcp.Server`. It is a
  **new, small stdio JSON-RPC server in a new package `internal/loopserver`** that speaks the
  same JSON-RPC 2.0 framing (identical `serverReq`/`serverResp` shapes) and exposes
  `loop.start` / `loop.send` / `loop.observe` as **custom JSON-RPC methods** (cleaner than
  overloading tools/call, and this server has no tool registry to host tools on). Live
  `loop.observe` events are pushed as id-less `loop.event` notifications on the same encoder,
  serialized under an encoder mutex exactly as mcp/server.go serializes `$/progress`. This
  keeps `cmd/fuse/mcp_server.go` and `internal/mcp` byte-untouched (a hard constraint) while
  reusing the proven framing pattern. Record as an ADR
  ("loop-server is a dedicated JSON-RPC server, not the tool-call MCP server").

---

## Task 1 — `internal/runtime` package: types + interface (compile-only seam)

Establish the package, the policy-free interface, and the value types. No behavior yet; the
deliverable is a compiling, importable, type-checked seam with a table-driven type test.

### Files
- Create `internal/runtime/runtime.go` — the `Runtime` interface and the four value types.
- Create `internal/runtime/runtime_test.go` — compile/type assertions.

### Interfaces
- Consumes: `context.Context`, `internal/event` (`event.Event`, `event.Seq`),
  `internal/agent` (`agent.AgentHandle`).
- Produces (exact Go signatures):

```go
package runtime

import (
    "context"

    "github.com/ethanhinson/fuse/internal/agent"
    "github.com/ethanhinson/fuse/internal/event"
)

// Runtime is the policy-free composition seam over the agent engine. It names loop
// mechanics only — no renderer, TUI, approval gate, or MCP/CLI vocabulary appears here.
// Every method keys on loopID = (*agent.AgentTree).RootID(); the interface is designed for
// N loops though this change implements one loop per process (see plan Decision Note D-1).
type Runtime interface {
    // StartLoop constructs and drives one agent loop from cfg, returning a handle whose
    // ID() is the tree RootID. It wraps agent.New + SetEventSink/SetNodeIdentity/
    // EnableSummarization + Agent.Run. The run executes in a goroutine; the handle awaits it.
    StartLoop(ctx context.Context, cfg LoopConfig) (LoopHandle, error)

    // Send injects human input at the loop's next turn boundary (ADR-0022 human-bus).
    Send(ctx context.Context, loopID string, input string) error

    // Spawn starts a child agent under the loop and returns a handle wrapping
    // agent.AgentHandle (ADR-0026: Wait()/Result()/Done).
    Spawn(ctx context.Context, loopID string, opts SpawnOpts) (SpawnHandle, error)

    // Observe returns a live event tail plus an idempotent unsubscribe (EventStore.Subscribe).
    Observe(loopID string) (<-chan event.Event, func(), error)

    // Attach returns durable history with Seq > from (EventStore.Replay) for reconnect.
    Attach(loopID string, from event.Seq) ([]event.Event, error)
}

// LoopConfig is the policy-free description of one loop to start. It carries loop mechanics
// inputs only; renderer/gate/transport are the binding's business and never appear here.
type LoopConfig struct {
    Task    string // the initial user task
    ModelID string // resolved gateway model id; "" ⇒ the binding resolves the default first
}

// LoopHandle observes and awaits one running loop.
type LoopHandle interface {
    ID() string                 // the loop id = tree RootID
    Wait() ([]model.Message, error) // blocks until the loop's Run returns; memoized
}

// SpawnOpts describes one child spawn at the seam (mirrors agent.SpawnOpts' public fields
// without importing agent vocabulary into bindings).
type SpawnOpts struct {
    Label        string
    Task         string
    SystemPrompt string
    ModelID      string
    Tools        []string
    Worker       string
    Expects      any
}

// SpawnHandle wraps agent.AgentHandle (ADR-0026) so a binding awaits a child without
// importing internal/agent's async internals directly at the call site.
type SpawnHandle interface {
    NodeID() string
    Wait() agent.SpawnDone
    Result() (any, error)
}
```

> Note: `LoopHandle.Wait` returns `[]model.Message` — add `"github.com/ethanhinson/fuse/internal/model"` to the import block. `model` is agent-adjacent but NOT a renderer/TUI/MCP type, so it does not violate the policy-free constraint.

### Steps
- [ ] Write `internal/runtime/runtime_test.go` with a compile-time assertion that a nil
      `*inProcRuntime` (declared but not yet defined) satisfies `Runtime`. Since the impl does
      not exist yet, instead write a test that only references the interface + value types to
      pin their shape:
      ```go
      package runtime

      import (
          "context"
          "testing"

          "github.com/ethanhinson/fuse/internal/event"
      )

      func TestRuntimeInterfaceShape(t *testing.T) {
          // A struct literal proves every LoopConfig/SpawnOpts field name+type is stable.
          _ = LoopConfig{Task: "t", ModelID: "m"}
          _ = SpawnOpts{Label: "l", Task: "t", SystemPrompt: "s", ModelID: "m", Tools: []string{"bash"}, Worker: "w", Expects: map[string]any{}}
          var _ Runtime = (Runtime)(nil)
          var _ event.Seq = event.Seq(0)
          _ = context.Background()
      }
      ```
- [ ] Run `go test ./internal/runtime/...` — see it FAIL (package/types do not exist:
      `undefined: LoopConfig`).
- [ ] Create `internal/runtime/runtime.go` with the interface and value types exactly as
      above (including the `model` import for `LoopHandle.Wait`).
- [ ] Run `go test ./internal/runtime/...` — PASS.
- [ ] Run `go build ./...` — PASS (nothing imports the package yet).
- [ ] Commit: `feat(runtime): #45 add policy-free Runtime interface and value types`.

### Deliverable
A compiling `internal/runtime` package exporting the `Runtime` interface and its four value
types, with a type-shape test. No implementation, no importers.

---

## Task 2 — in-process Runtime: StartLoop + LoopHandle (single loop, event stream wired)

Add the concrete `inProcRuntime`. Implement `StartLoop` and the `LoopHandle`, owning the
event store as instance state (Decision Note D-1). This is the smallest reviewable unit that
proves a loop runs through the seam and emits events.

### Files
- Create `internal/runtime/inproc.go` — `inProcRuntime`, `New`, `StartLoop`, `loopHandle`.
- Create `internal/runtime/inproc_test.go` — a StartLoop test using a fake Completer.
- Modify `internal/runtime/runtime_test.go` — add the satisfaction assertion
  `var _ Runtime = (*inProcRuntime)(nil)` (now that the type exists).

### Interfaces
- Consumes: `agent.New`, `agent.NewAgentTreeWithConcurrency`, `(*AgentTree).RootID/Node/
  Scheduler`, `(*Agent).SetEventSink/SetNodeIdentity/Run`, `event.EventStore`,
  `fsstore.NewFSEventStore`, `model.Completer`, `model.Message`.
- Produces (exact Go signatures):

```go
// New builds an in-process Runtime. deps carries the collaborators a binding constructs
// (model Completer factory, tool registry factory, config) so the Runtime stays free of
// cmd-layer policy. store, when nil, is opened per-loop under baseDir/<rootID>.
func New(deps Deps) *inProcRuntime

// Deps are the binding-supplied collaborators the Runtime composes. None are renderer/TUI/
// gate types; a binding wires those around the Runtime, not into it.
type Deps struct {
    // BuildAgent constructs the root *agent.Agent for a loop from its resolved model id and
    // task-time system prompt, using the binding's own completer/tools/gate wiring. It is the
    // one seam through which a binding injects its engine construction WITHOUT the Runtime
    // importing cmd-layer builders. Returns the agent and the resolved gateway model id.
    BuildAgent func(modelID string, toolReg *tools.Registry) (*agent.Agent, string, error)
    // NewToolRegistry builds the per-loop tool registry (spawn_agent etc. are wired by the
    // binding via BuildAgent's closure over the same registry). May be nil for observe-only.
    NewToolRegistry func() *tools.Registry
    // BaseDir is where per-loop fsstore event logs live (e.g. session.DefaultLogDir()).
    BaseDir string
    // MaxConcurrent seeds the loop tree's scheduler concurrency.
    MaxConcurrent int
}

func (r *inProcRuntime) StartLoop(ctx context.Context, cfg LoopConfig) (LoopHandle, error)

type loopHandle struct { /* id string; done chan struct{}; msgs []model.Message; err error; once sync.Once */ }
func (h *loopHandle) ID() string
func (h *loopHandle) Wait() ([]model.Message, error)
```

### Design of StartLoop (no placeholders)

```go
func (r *inProcRuntime) StartLoop(ctx context.Context, cfg LoopConfig) (LoopHandle, error) {
    tree := agent.NewAgentTreeWithConcurrency(cfg.ModelID, cfg.ModelID, r.deps.MaxConcurrent)
    rootID := tree.RootID()
    rootNode := tree.Node(rootID)

    store, err := fsstore.NewFSEventStore(r.deps.BaseDir, rootID)
    if err != nil {
        return nil, fmt.Errorf("runtime: open event store: %w", err)
    }

    var toolReg *tools.Registry
    if r.deps.NewToolRegistry != nil {
        toolReg = r.deps.NewToolRegistry()
    } else {
        toolReg = tools.NewRegistry()
    }

    a, _, err := r.deps.BuildAgent(cfg.ModelID, toolReg)
    if err != nil {
        _ = store.Close()
        return nil, fmt.Errorf("runtime: build agent: %w", err)
    }
    a.SetEventSink(store)
    a.SetNodeIdentity(rootNode.ID, rootNode.ParentID, rootNode.Depth)

    lp := &loop{
        id:      rootID,
        tree:    tree,
        node:    rootNode,
        store:   store,
        humanBus: agent.NewHumanBus(tree),
    }
    r.mu.Lock()
    if r.loops == nil {
        r.loops = map[string]*loop{}
    }
    r.loops[rootID] = lp
    r.mu.Unlock()

    // Root human-message injector so Send() lands at a turn boundary (ADR-0022).
    a.SetHumanInjector(agent.NewHumanInjector(rootNode.ID, lp.humanBus))

    h := &loopHandle{id: rootID, done: make(chan struct{})}
    lp.handle = h
    go func() {
        defer close(h.done)
        msgs, rerr := a.Run(ctx, []model.Message{{Role: "user", Content: cfg.Task}})
        h.msgs, h.err = msgs, rerr
    }()
    return h, nil
}
```

> `loop` is an unexported struct on the Runtime holding per-loopID state (tree, node, store,
> humanBus, handle). It is the loopID-keyed map value that makes the N-loop design real while
> only one is populated per process. `inProcRuntime` fields: `deps Deps`, `mu sync.Mutex`,
> `loops map[string]*loop`.

### Steps
- [ ] Write `internal/runtime/inproc_test.go`: a `fakeCompleter` returning one assistant
      message with no tool calls, wired via a `Deps.BuildAgent` that calls
      `agent.New(fake, execAll, nopRenderer{}, modelID, "", 1, 0)`. Assert
      `StartLoop` returns a handle whose `ID()` equals a non-empty string, `Wait()` returns
      nil error, and the per-loop fsstore (opened under a `t.TempDir()` BaseDir) contains a
      `turn.start` then `turn.end` event via `Replay(0)`.
      ```go
      rt := New(Deps{BaseDir: t.TempDir(), MaxConcurrent: 1,
          BuildAgent: func(modelID string, reg *tools.Registry) (*agent.Agent, string, error) {
              return agent.New(fake, execAll{reg}, nopRenderer{}, modelID, "", 1, 0), modelID, nil
          }})
      h, err := rt.StartLoop(context.Background(), LoopConfig{Task: "hi", ModelID: "cloud/x"})
      // ... assert h.ID() != "", h.Wait() err == nil, Replay shows turn.start/turn.end
      ```
- [ ] Run `go test ./internal/runtime/...` — FAIL (`undefined: New`, `undefined: inProcRuntime`).
- [ ] Create `internal/runtime/inproc.go` with `inProcRuntime`, `loop`, `Deps`, `New`,
      `StartLoop`, `loopHandle` exactly per the design above. Import `fmt`, `sync`, `context`,
      `agent`, `event`, `fsstore`, `model`, `tools`.
- [ ] Add `var _ Runtime = (*inProcRuntime)(nil)` to `runtime_test.go` (it will fail to
      compile until `Send/Spawn/Observe/Attach` exist — so at THIS task, temporarily add
      panicking stubs for those four methods with `panic("not implemented")` bodies and a
      `// implemented in Task 3/4` comment, so the type satisfies the interface and the
      StartLoop test can run). Each stub is one line; they are replaced with real bodies in
      Tasks 3–4, not left as placeholders in the shipped change.
- [ ] Run `go test ./internal/runtime/...` — PASS.
- [ ] Run `go build ./...` — PASS.
- [ ] Commit: `feat(runtime): #45 in-process StartLoop wires event store and runs the loop`.

### Deliverable
`rt.StartLoop(...)` runs a loop end to end through the seam and produces a real fsstore event
stream, verifiable via `Replay`. The four unimplemented methods panic (filled next).

---

## Task 3 — in-process Runtime: Observe + Attach + Send + Spawn

Replace the four stubs with real bodies. Each is a thin, faithful wrapper over the underlying
primitive keyed by loopID.

### Files
- Modify `internal/runtime/inproc.go` — implement `Observe`, `Attach`, `Send`, `Spawn`; add
  `spawnHandle` type. (Line refs: replace the four `panic("not implemented")` stubs added in
  Task 2.)
- Modify `internal/runtime/inproc_test.go` — add tests for observe/attach/send/spawn.

### Interfaces
- Consumes: `(*loop).store` (`event.EventStore.Subscribe/Replay`), `(*HumanBus).Enqueue`,
  `agent.NewSpawner`+options, `(*Spawner).Spawn`, `agent.AgentHandle`.
- Produces (exact Go signatures):

```go
func (r *inProcRuntime) Observe(loopID string) (<-chan event.Event, func(), error)
func (r *inProcRuntime) Attach(loopID string, from event.Seq) ([]event.Event, error)
func (r *inProcRuntime) Send(ctx context.Context, loopID string, input string) error
func (r *inProcRuntime) Spawn(ctx context.Context, loopID string, opts SpawnOpts) (SpawnHandle, error)

type spawnHandle struct { h agent.AgentHandle }
func (s spawnHandle) NodeID() string          { return s.h.NodeID }
func (s spawnHandle) Wait() agent.SpawnDone    { return s.h.Wait() }
func (s spawnHandle) Result() (any, error)     { return s.h.Result() }
```

### Bodies (no placeholders)

```go
var ErrLoopNotFound = errors.New("runtime: loop not found")

func (r *inProcRuntime) lookup(loopID string) (*loop, error) {
    r.mu.Lock()
    defer r.mu.Unlock()
    lp, ok := r.loops[loopID]
    if !ok {
        return nil, fmt.Errorf("%w: %q", ErrLoopNotFound, loopID)
    }
    return lp, nil
}

func (r *inProcRuntime) Observe(loopID string) (<-chan event.Event, func(), error) {
    lp, err := r.lookup(loopID)
    if err != nil {
        return nil, nil, err
    }
    ch, cancel := lp.store.Subscribe()
    return ch, cancel, nil
}

func (r *inProcRuntime) Attach(loopID string, from event.Seq) ([]event.Event, error) {
    lp, err := r.lookup(loopID)
    if err != nil {
        return nil, err
    }
    return lp.store.Replay(from)
}

func (r *inProcRuntime) Send(ctx context.Context, loopID string, input string) error {
    lp, err := r.lookup(loopID)
    if err != nil {
        return err
    }
    // ADR-0022 turn-boundary injection: enqueue for the root node; the root injector
    // installed at StartLoop drains it at the next turn boundary. ModeRespond = a direct
    // reply to that node (not a broadcast).
    lp.humanBus.Enqueue(lp.node.ID, agent.ModeRespond, "@human", input)
    return nil
}

func (r *inProcRuntime) Spawn(ctx context.Context, loopID string, opts SpawnOpts) (SpawnHandle, error) {
    lp, err := r.lookup(loopID)
    if err != nil {
        return nil, err
    }
    spawner := agent.NewSpawner(
        agent.WithTree(lp.tree),
        agent.WithNode(lp.node),
        agent.WithSpawnDepth(lp.node.Depth),
        agent.WithEventStore(lp.store),   // spawn.start/spawn.done land on the same stream
        agent.WithChildBuilder(lp.buildChild), // set by the binding's Deps; see below
    )
    h, herr := spawner.Spawn(ctx, agent.SpawnOpts{
        Label:        opts.Label,
        Task:         opts.Task,
        SystemPrompt: opts.SystemPrompt,
        ModelID:      opts.ModelID,
        Tools:        opts.Tools,
        Worker:       opts.Worker,
        Expects:      opts.Expects,
    })
    if herr != nil {
        return nil, herr
    }
    return spawnHandle{h: h}, nil
}
```

> The child builder is loop-specific policy the binding supplies. Extend `Deps` with
> `BuildChild agent.ChildBuilder` (a `func(ctx, agent.SpawnOpts, *AgentNode, *AgentTree)
> (string, error)`), and store it on `loop.buildChild` in `StartLoop`. `agent.ChildBuilder`
> is an agent type, not a renderer/TUI/MCP type, so it does not break the policy-free
> constraint (a binding still owns what the child renders/gates INSIDE that closure).

### Steps
- [ ] Write test `TestObserveAndAttach`: StartLoop, `Attach(id, 0)` returns the run's events;
      `Observe(id)` on a second loop started fresh yields a live channel that receives at
      least a `turn.start` when the loop runs (drive ordering by starting Observe before Run
      finishes — use a fake Completer that blocks on a channel until the test has subscribed).
- [ ] Write test `TestSendEnqueuesForRoot`: StartLoop, `Send(ctx, id, "more")`, then assert
      `lp.humanBus.Pending(rootID)` (via a small exported test accessor or by observing the
      injected user message on the next turn through a two-turn fake Completer) contains the
      text. Prefer the observable route: a fake Completer whose 2nd turn echoes the injected
      human message content, asserted on the fsstore `model.call.start` msg_count bump or the
      returned final message.
- [ ] Write test `TestSpawnReturnsHandle`: StartLoop with a `Deps.BuildChild` returning a
      fixed string; `Spawn(ctx, id, SpawnOpts{Task:"x"})`; assert `h.Wait().Result == "x-out"`
      and that a `spawn.start`+`spawn.done` pair appears in `Attach(id, 0)`.
- [ ] Write test `TestLoopNotFound`: `Observe("nope")`, `Attach("nope",0)`, `Send(ctx,"nope","")`,
      `Spawn(ctx,"nope",...)` each return `ErrLoopNotFound`.
- [ ] Run `go test ./internal/runtime/...` — FAIL (methods still panic).
- [ ] Replace the four stubs with the bodies above; add `spawnHandle`, `ErrLoopNotFound`,
      `lookup`, `loop.buildChild`, and `Deps.BuildChild`. Import `errors`.
- [ ] Run `go test ./internal/runtime/...` — PASS.
- [ ] Run `make test-race` scoped: `go test -race ./internal/runtime/...` — PASS (Subscribe
      fan-out + Run goroutine + Send are concurrent).
- [ ] Commit: `feat(runtime): #45 implement Observe/Attach/Send/Spawn on in-process Runtime`.

### Deliverable
A fully-implemented `inProcRuntime` satisfying `Runtime` with race-clean observe/attach/send/
spawn, each verified against a real fsstore event stream.

---

## Task 4 — CLI binding #1: shared Runtime construction in run.go, migrate the ONE-SHOT builder

Migrate the first of the three cloned builders (`cmd/fuse/main.go` `run()`, the one-shot
path) to construct the in-process Runtime and drive `StartLoop`, keeping renderer/gate/TUI in
the cmd layer. Add a shared `cmd/fuse` helper that assembles `runtime.Deps` from the existing
`buildAgentCore`/`spawnFuncFrom` wiring so all three builders reuse it.

> Re-derive the exact builder site list by grep at build time — do NOT trust this frozen list
> (learning `patch-every-cloned-child-builder`). Run:
> `grep -rn "a\.Run(\|\.Run(ctx\|Spawner\|spawnFuncFrom\|buildAgentCore" cmd/fuse/*.go | grep -v _test.go`
> and confirm the migration targets are exactly `cmd/fuse/main.go`, `cmd/fuse/shell.go`,
> `cmd/fuse/research_probe.go`. If grep reveals a fourth cmd-site calling `agent.Run` or
> `Spawner.Spawn` directly, that site is added to Tasks 4–6's scope before proceeding.

### Files
- Create `cmd/fuse/runtime_binding.go` — `func newInProcRuntime(...) *runtime.inProcRuntime`
  wrapper is NOT possible (inProcRuntime is unexported); instead export a constructor: in
  Task 2 the constructor `runtime.New` already returns the interface? It returns
  `*inProcRuntime`. **Change**: make `runtime.New` return the `Runtime` interface
  (`func New(deps Deps) Runtime`) so cmd/fuse consumes it abstractly. Update Task 2's
  signature accordingly (the interface satisfaction test still holds). `runtime_binding.go`
  builds `runtime.Deps` from the existing cmd helpers.
- Modify `cmd/fuse/main.go` — replace the direct `buildAgentCore(...) + a.Run(...)` root-run
  tail (current lines ~254–272) with `runtime.New(deps).StartLoop(...)` + `h.Wait()`. The
  spawn factory closure (lines ~165–241) becomes the `Deps.BuildChild`; the root agent build
  becomes `Deps.BuildAgent`.
- Modify `internal/runtime/runtime.go` — change `func New` return type to `Runtime`.

### Interfaces
- Consumes: `runtime.New`, `runtime.Deps`, `runtime.LoopConfig`, `runtime.LoopHandle`,
  existing `buildAgentCore`, `spawnFuncFrom`, `currentEventStore`, `setActiveEventStore`.
- Produces (exact Go signatures):

```go
// buildOneShotRuntimeDeps assembles runtime.Deps for the one-shot entry point, closing over
// the resolved cfg/reg/toolReg/renderer/approve wiring exactly as the pre-migration builder
// did. The renderer, approval gate, and tool registry stay in cmd/fuse — the seam receives
// only construction closures, never those types by name.
func buildOneShotRuntimeDeps(cfg config.Config, reg *model.Registry, modelAlias string,
    toolReg *tools.Registry, stdout io.Writer, verbose bool, traceW io.Writer,
    rootApprove permissions.ApprovalFunc, oneShotSystemBlock string, oneShotBudget bool,
    rateGate model.RateGate) runtime.Deps
```

### Migration shape (no placeholders)

The one-shot `run()` currently: builds tree/sched/bb, wires spawn factory + tools, builds the
root agent, sets event sink to `currentEventStore()` (no-op for one-shot), and calls
`a.Run(...)`. After migration:

- The tree/sched/bb + spawn-factory + tool wiring MOVE INTO `buildOneShotRuntimeDeps`, which
  returns `runtime.Deps` whose `BuildAgent` runs the existing `buildAgentCore(...)` +
  `SetStripSpawn` + `SetEventSink(store)` **against the store the Runtime passes it** (not the
  package global), and whose `BuildChild` is the existing child-builder closure verbatim.
  IMPORTANT (learning `relocated-emission-resource-every-projected-field`): the Runtime now
  owns the store; `BuildAgent`/`BuildChild` must set the sink to that store. To keep the
  package-global readers working during single-loop operation (D-1), `StartLoop` also calls
  `setActiveEventStore(store)` — add that one line to `StartLoop` in `inproc.go` behind a
  `Deps.InstallGlobalStore func(event.EventStore)` hook (so `internal/runtime` does not import
  cmd/fuse). The one-shot binding passes `InstallGlobalStore: setActiveEventStore`.
- `run()`'s tail becomes:
  ```go
  deps := buildOneShotRuntimeDeps(cfg, reg, *modelAlias, toolReg, stdout, *verbose, traceW, rootApprove, oneShotSystemBlock, oneShotBudget, rateGate)
  rt := runtime.New(deps)
  h, err := rt.StartLoop(context.Background(), runtime.LoopConfig{Task: task, ModelID: *modelAlias})
  if err != nil { fmt.Fprintf(stderr, "%v\n", err); return 1 }
  if _, err := h.Wait(); err != nil {
      fmt.Fprintf(stderr, "run error: %v\n", err)
      return 1
  }
  return 0
  ```
- `Deps.BaseDir` for one-shot is `session.DefaultLogDir()` (was: no store installed, so the
  new fsstore is a behavior ADDITION for one-shot — acceptable and inert to output, but to
  preserve byte-identical *output* the plan keeps one-shot's stdout untouched; only a new
  events.jsonl is written under the session dir, exactly as shell already does). If preserving
  "one-shot writes no event log" is required, pass `Deps.BaseDir == ""` and have `StartLoop`
  use `event.NoopStore{}` when BaseDir is empty — DECISION: use the NoopStore path for
  one-shot to keep one-shot behavior byte-identical (no new files), and a real fsstore only
  for shell (Task 5) and loop-server (Task 6). Add to `StartLoop`:
  ```go
  var store event.EventStore
  if r.deps.BaseDir == "" {
      store = event.NoopStore{}
  } else {
      fs, err := fsstore.NewFSEventStore(r.deps.BaseDir, rootID)
      if err != nil { return nil, fmt.Errorf("runtime: open event store: %w", err) }
      store = fs
  }
  ```

### Steps
- [ ] Re-derive the builder site list by grep (command above); record the confirmed three
      sites in the commit body.
- [ ] Change `runtime.New` to return `Runtime`; run `go test ./internal/runtime/...` — PASS.
- [ ] Write `cmd/fuse/runtime_binding_test.go` `TestOneShotRuntimeParity`: a scripted httptest
      gateway (pattern from `structured_delegation_e2e_test.go`) returning one no-tool-call
      assistant turn; `t.Setenv("LLM_GATEWAY_URL", srv.URL)`, `t.Setenv("HOME", t.TempDir())`;
      call `run([]string{"do a thing"}, &out, &errb)`; assert exit 0 and that the gateway
      received exactly one request whose messages carry the task. This is the real-binary seam
      (learning `verify-tool-loop-at-gateway-seam`).
- [ ] Run the test — FAIL (still on the old code path OR failing because deps not built).
- [ ] Implement `buildOneShotRuntimeDeps` + `Deps.InstallGlobalStore` + the BaseDir/NoopStore
      branch; rewrite `run()`'s tail. Move the spawn factory + tool wiring into the deps
      builder (behavior-preserving relocation).
- [ ] Run `go test ./cmd/fuse/... -run TestOneShotRuntime` — PASS.
- [ ] Run the FULL cmd/fuse suite `go test ./cmd/fuse/...` — PASS (existing one-shot tests
      unchanged: same output, same exit codes).
- [ ] Run `go build ./...` — PASS.
- [ ] Commit: `refactor(cmd): #45 migrate one-shot builder to consume Runtime (binding #1a)`.

### Deliverable
`fuse "<task>"` runs entirely through `runtime.StartLoop`; one-shot output/exit behavior is
byte-identical, proven by the existing suite plus a gateway-double parity test.

---

## Task 5 — CLI binding #1: migrate shell.go and research_probe.go

Migrate the remaining two cloned builders through the SAME `runtime.Deps` seam, adding
per-binding deps builders that preserve each path's unique wiring (shell: TUI renderer +
session mode + human bus + fsstore + projected-log consumer; research-probe: MultiRenderer +
workflow activation + AlwaysApprove).

### Files
- Modify `cmd/fuse/shell.go` — replace the `build` closure + spawn factory + `a.Run` in the
  TUI's per-turn build with a Runtime constructed via a new `buildShellRuntimeDeps`. The
  ShellModel's `build func(...)` seam (passed to `tui.NewShellModel`) now returns an agent the
  TUI drives; keep that seam but back it by `runtime.StartLoop` semantics. (Line refs: `build`
  closure ~213–231, spawn factory ~273–392, root tool wiring ~394–409, `p.Run()` ~416–424.)
- Modify `cmd/fuse/research_probe.go` — replace the spawn factory + `rootAgent.Run` (~137–288)
  with a `buildResearchProbeRuntimeDeps` + `StartLoop` + `h.Wait()`; keep `probe.Summarize`.
- Create `cmd/fuse/runtime_binding.go` additions: `buildShellRuntimeDeps`,
  `buildResearchProbeRuntimeDeps` (same shape as Task 4's one-shot deps builder).

> NOTE on shell.go: the interactive TUI drives turns itself (bubbletea program owns the loop
> cadence via the `build` seam), so shell's migration keeps the TUI ownership of *rendering
> and turn cadence* while routing engine CONSTRUCTION through `runtime.Deps` (BuildAgent /
> BuildChild) and the event store through the Runtime instance. `StartLoop` is used for the
> ROOT engine construction + store ownership; the TUI's existing `build` seam calls into the
> Runtime-owned deps rather than `buildAgentWithRendererAndTrace` directly. The human-bus
> `Send` path is already covered by the TUI's own channel — the Runtime's `Send` is exercised
> by binding #2, not required to replace the TUI's ask/human wiring here. Keep shell's human
> messaging, projected-log consumer, and segment sink exactly as-is; only the event store's
> OWNER changes (Runtime opens/closes it; shell stops calling `setActiveEventStore` directly
> and instead passes `InstallGlobalStore: setActiveEventStore` + `BaseDir: logDir`).

### Interfaces
- Consumes: same as Task 4 plus shell-specific (`sessionMode`, `humanBus`, `handleReg`,
  `startProjectedLogConsumer`, `fssink.NewFSSegmentSink`) and probe-specific
  (`workflowActivation`, `probe.NewLog`, `tui.NewMultiRenderer`).
- Produces:

```go
func buildShellRuntimeDeps(cfg config.Config, reg *model.Registry, alias string,
    toolReg *tools.Registry, /* sessionMode, humanBus, handleReg, traceW, rateGate, logDir, verbose ... */) runtime.Deps
func buildResearchProbeRuntimeDeps(cfg config.Config, reg *model.Registry, alias string,
    toolReg *tools.Registry, act *workflowActivation, logSink *probe.Log, /* traceW, rateGate ... */) runtime.Deps
```

### Steps
- [ ] Re-run the builder-site grep to confirm shell.go + research_probe.go are the exact
      remaining sites and no fourth site emerged.
- [ ] Write `cmd/fuse/runtime_binding_test.go` additions:
      - `TestResearchProbeRuntimeParity`: scripted gateway returning a one-turn synthesis; run
        `run([]string{"research-probe", "q"}, ...)` with `LLM_GATEWAY_URL` set; assert exit 0
        and the digest is printed (probe path drives through StartLoop).
      - For shell: a headless assertion is hard under bubbletea; instead assert
        `buildShellRuntimeDeps(...)` returns a `Deps` whose `BuildAgent` produces a non-nil
        `*agent.Agent` and whose `BuildChild` is non-nil (a wiring-assertion test, matching the
        existing `blackboard_wiring_test.go` / `spawn_func_from_test.go` style), plus keep the
        existing `shell_test.go` teatest green.
- [ ] Run tests — FAIL (deps builders undefined).
- [ ] Implement `buildShellRuntimeDeps` and `buildResearchProbeRuntimeDeps`; rewrite the two
      builders to construct `runtime.New(deps)`; for research-probe replace `rootAgent.Run`
      with `StartLoop` + `h.Wait()` (wrap in `tree.BeginTurn()/EndTurn()` exactly as before —
      the tree is owned by the deps builder and returned alongside so the probe can still call
      `probe.Summarize(logSink, tree)`; add a `Deps` accessor `Tree() *agent.AgentTree` on the
      handle OR return the tree from the deps builder as a second value).
      - DECISION: return the tree from the deps builder: `func buildResearchProbeRuntimeDeps(...)
        (runtime.Deps, *agent.AgentTree)` so `probe.Summarize` still has the tree. (StartLoop
        creates its OWN tree — so instead, add `Deps.Tree *agent.AgentTree` and have
        `StartLoop` USE a caller-supplied tree when non-nil, else create one. This lets the
        probe/shell keep their externally-observed tree. Add to `StartLoop`:
        `tree := r.deps.Tree; if tree == nil { tree = agent.NewAgentTreeWithConcurrency(...) }`.)
- [ ] Run `go test ./cmd/fuse/...` — PASS (shell teatest, research-probe, one-shot all green).
- [ ] Run `go build ./...` — PASS.
- [ ] **Verification step (learning `patch-every-cloned-child-builder`):** grep-assert that no
      cmd-site outside the Runtime impl calls the engine directly:
      `grep -rn "\.Run(ctx\|a\.Run(\|rootAgent\.Run(\|spawner\.Spawn(\|Spawner)\.Spawn(" cmd/fuse/*.go | grep -v _test.go`
      must return ONLY lines inside `runtime_binding.go`'s `BuildChild` closures (which call
      `a.Run` legitimately as the child runner the Runtime's Spawner invokes) — and the root
      `a.Run` must appear ONLY in `internal/runtime/inproc.go`. Encode this as a Go test
      `TestNoDirectEngineDriveAtCmdSites` that reads the cmd/fuse `.go` files (non-test) and
      asserts no top-level `run()`/`runShell()`/`runResearchProbe()` body calls `a.Run` for
      the ROOT loop (the child-builder closures are exempt and are inside deps builders).
- [ ] Commit: `refactor(cmd): #45 migrate shell + research-probe builders to Runtime (binding #1b)`.

### Deliverable
All three CLI entrypoints construct and drive the engine through `runtime.Runtime`. A grep/
Go test proves no cmd-site drives the root loop directly. Existing CLI/shell/research-probe
behavior is unchanged (same output, same spawn results, same events).

---

## Task 6 — binding #2: `internal/loopserver` JSON-RPC server (loop.start / loop.send)

Add the new dedicated stdio JSON-RPC server (Decision Note D-2) exposing `loop.start` and
`loop.send` as custom methods over `runtime.Runtime`. `loop.observe` is Task 7.
`cmd/fuse/mcp_server.go` and `internal/mcp` stay byte-untouched.

### Files
- Create `internal/loopserver/server.go` — the JSON-RPC server, request/response frames
  (copied shape from mcp/server.go), `dispatch`, `handleStart`, `handleSend`.
- Create `internal/loopserver/server_test.go` — dispatch-level unit tests using a fake
  `runtime.Runtime`.

### Interfaces
- Consumes: `runtime.Runtime`, `runtime.LoopConfig`, `context.Context`, `encoding/json`,
  `io`.
- Produces (exact Go signatures):

```go
package loopserver

// Server is a stdio JSON-RPC 2.0 server dedicated to loop control (binding #2). It is a pure
// adapter over runtime.Runtime — no renderer, TUI, approval gate, or MCP tool registry. Its
// policy is "no approval gate" (auto-approve), a binding choice documented here, NOT a
// property of the Runtime seam.
type Server struct {
    rt      runtime.Runtime
    enc     *json.Encoder
    dec     *json.Decoder
    encMu   sync.Mutex // serialize every write (responses + notifications) on the shared encoder
    // subs maps loopID -> active observe subscription cancels (Task 7).
}

func NewServer(r io.Reader, w io.Writer, rt runtime.Runtime) *Server
func (s *Server) Serve(ctx context.Context) error

// wire frames — identical shape to internal/mcp for a proven encoder discipline.
type req  struct { JSONRPC string; ID json.RawMessage `json:"id"`; Method string; Params json.RawMessage `json:"params,omitempty"` }
type resp struct { JSONRPC string `json:"jsonrpc"`; ID json.RawMessage `json:"id"`; Result json.RawMessage `json:"result,omitempty"`; Error *rpcError `json:"error,omitempty"` }
type rpcError struct { Code int `json:"code"`; Message string `json:"message"` }

// method params/results
type startParams  struct { Task string `json:"task"`; Model string `json:"model,omitempty"` }
type startResult  struct { LoopID string `json:"loop_id"` }
type sendParams   struct { LoopID string `json:"loop_id"`; Input string `json:"input"` }
```

### Bodies (no placeholders)

```go
const (
    codeParseError    = -32700
    codeInvalidParams = -32602
    codeMethodNotFound = -32601
    codeInternal      = -32603
)

func (s *Server) Serve(ctx context.Context) error {
    for {
        var r req
        if err := s.dec.Decode(&r); err != nil {
            if err == io.EOF { return nil }
            return fmt.Errorf("loopserver: decode: %w", err)
        }
        if len(r.ID) == 0 { // notification: no response
            continue
        }
        resp := s.dispatch(ctx, r)
        if err := s.encode(resp); err != nil {
            return fmt.Errorf("loopserver: encode: %w", err)
        }
    }
}

func (s *Server) encode(v any) error {
    s.encMu.Lock()
    defer s.encMu.Unlock()
    return s.enc.Encode(v)
}

func (s *Server) dispatch(ctx context.Context, r req) resp {
    switch r.Method {
    case "loop.start":
        return s.handleStart(ctx, r)
    case "loop.send":
        return s.handleSend(ctx, r)
    case "loop.observe":
        return s.handleObserve(ctx, r) // Task 7
    default:
        return s.errResp(r.ID, codeMethodNotFound, "method not found: "+r.Method)
    }
}

func (s *Server) handleStart(ctx context.Context, r req) resp {
    var p startParams
    if err := json.Unmarshal(r.Params, &p); err != nil {
        return s.errResp(r.ID, codeInvalidParams, "invalid params: "+err.Error())
    }
    h, err := s.rt.StartLoop(ctx, runtime.LoopConfig{Task: p.Task, ModelID: p.Model})
    if err != nil {
        return s.errResp(r.ID, codeInternal, err.Error())
    }
    return s.okResp(r.ID, startResult{LoopID: h.ID()})
}

func (s *Server) handleSend(ctx context.Context, r req) resp {
    var p sendParams
    if err := json.Unmarshal(r.Params, &p); err != nil {
        return s.errResp(r.ID, codeInvalidParams, "invalid params: "+err.Error())
    }
    if err := s.rt.Send(ctx, p.LoopID, p.Input); err != nil {
        return s.errResp(r.ID, codeInternal, err.Error())
    }
    return s.okResp(r.ID, map[string]any{})
}
```

(`okResp`/`errResp` mirror mcp/server.go's `ok`/`errResp`.)

### Steps
- [ ] Write `server_test.go` with a `fakeRuntime` implementing `runtime.Runtime` (StartLoop
      returns a handle with a fixed ID; Send records its args). Test:
      `TestDispatchLoopStartReturnsLoopID` (dispatch a `loop.start` req, assert result carries
      `loop_id`), `TestDispatchLoopSendCallsRuntime` (assert fakeRuntime.Send saw the args),
      `TestUnknownMethod` (`codeMethodNotFound`), `TestLoopStartBadParams`
      (`codeInvalidParams`).
- [ ] Run `go test ./internal/loopserver/...` — FAIL (package undefined).
- [ ] Create `internal/loopserver/server.go` with the frames, `NewServer`, `Serve`,
      `dispatch`, `handleStart`, `handleSend`, and a `handleObserve` stub returning
      `codeMethodNotFound` temporarily (replaced in Task 7).
- [ ] Run `go test ./internal/loopserver/...` — PASS.
- [ ] Run `go build ./...` — PASS.
- [ ] Commit: `feat(loopserver): #45 stdio JSON-RPC server with loop.start/loop.send`.

### Deliverable
An `internal/loopserver.Server` that adapts JSON-RPC `loop.start`/`loop.send` to a
`runtime.Runtime`, unit-tested at the dispatch level.

---

## Task 7 — binding #2: loop.observe (replay catch-up then live tail via notifications)

Implement `loop.observe(loop_id, from_seq?)`: first replay durable history since `from_seq`
via `Runtime.Attach`, then stream live events via `Runtime.Observe` as id-less `loop.event`
notifications on the same encoder. Preserve ADR-0025 back-pressure (a gap marker tells the
client to Attach-replay).

### Files
- Modify `internal/loopserver/server.go` — replace the `handleObserve` stub with the real
  body; add the `loop.event` notification frame and the per-observe pump goroutine; add gap
  detection.
- Modify `internal/loopserver/server_test.go` — observe tests.

### Interfaces
- Consumes: `runtime.Attach`, `runtime.Observe`, `event.Event`, `event.Seq`.
- Produces:

```go
type observeParams struct { LoopID string `json:"loop_id"`; FromSeq event.Seq `json:"from_seq,omitempty"` }
type observeResult struct { Replayed int `json:"replayed"`; LastSeq event.Seq `json:"last_seq"` }
type eventNote struct { JSONRPC string `json:"jsonrpc"`; Method string `json:"method"`; Params eventNoteParams `json:"params"` }
type eventNoteParams struct { LoopID string `json:"loop_id"`; Event event.Event `json:"event"`; Gap bool `json:"gap,omitempty"` }
```

### Body (no placeholders)

```go
func (s *Server) handleObserve(ctx context.Context, r req) resp {
    var p observeParams
    if err := json.Unmarshal(r.Params, &p); err != nil {
        return s.errResp(r.ID, codeInvalidParams, "invalid params: "+err.Error())
    }
    // 1) Subscribe BEFORE replay so no event slips between replay end and live start.
    ch, cancel, err := s.rt.Observe(p.LoopID)
    if err != nil {
        return s.errResp(r.ID, codeInternal, err.Error())
    }
    // 2) Replay durable history since from_seq; push each as a loop.event notification.
    hist, err := s.rt.Attach(p.LoopID, p.FromSeq)
    if err != nil {
        cancel()
        return s.errResp(r.ID, codeInternal, err.Error())
    }
    last := p.FromSeq
    for _, ev := range hist {
        s.pushEvent(p.LoopID, ev, false)
        last = ev.Seq
    }
    // 3) Live tail on a goroutine until ctx done or the loop's store closes (channel closed).
    //    Detect a Seq gap (ADR-0025 drop-newest) and flag it so the client Attach-replays.
    go func() {
        defer cancel()
        prev := last
        for {
            select {
            case <-ctx.Done():
                return
            case ev, ok := <-ch:
                if !ok { return }
                gap := ev.Seq > prev+1 && prev != 0
                s.pushEvent(p.LoopID, ev, gap)
                prev = ev.Seq
            }
        }
    }()
    // 4) The response acknowledges the subscription and the replay watermark.
    return s.okResp(r.ID, observeResult{Replayed: len(hist), LastSeq: last})
}

func (s *Server) pushEvent(loopID string, ev event.Event, gap bool) {
    _ = s.encode(eventNote{
        JSONRPC: "2.0",
        Method:  "loop.event",
        Params:  eventNoteParams{LoopID: loopID, Event: ev, Gap: gap},
    })
}
```

> Gap semantics: because live delivery is non-blocking drop-newest (ADR-0025), a subscriber
> can miss events; the `prev+1 != ev.Seq` check marks the FIRST post-gap event with
> `gap:true`, and the client reacts by re-issuing `loop.observe` with its last-seen `from_seq`
> to Attach-replay the hole. A slow client can never wedge the loop — the pump only drains the
> buffered channel; the store already dropped rather than blocked Append.

### Steps
- [ ] Write `TestObserveReplaysThenTails`: a `fakeRuntime` whose `Attach(id, from)` returns two
      historical events (Seq 1,2) and whose `Observe(id)` returns a channel the test feeds a
      live event (Seq 3). Drive the server over an in-memory pipe pair (`io.Pipe`), send a
      `loop.observe` req, and read raw frames from the server's output with a `json.Decoder`
      (a RAW JSON-RPC test client — NOT fuse's mcp client, whose read-pump drops id-less
      notifications; learning `mcp-read-pumps-drop-inbound-notifications`). Assert: one `resp`
      with `replayed:2,last_seq:2`, then three id-less `loop.event` notifications for Seq 1,2,3.
- [ ] Write `TestObserveMarksGap`: feed the live channel Seq 5 after replay watermark 2; assert
      the `loop.event` for Seq 5 carries `gap:true`.
- [ ] Run `go test ./internal/loopserver/...` — FAIL (stub returns method-not-found).
- [ ] Implement `handleObserve` + `pushEvent` + `eventNote` frames per the body above.
- [ ] Run `go test ./internal/loopserver/...` — PASS.
- [ ] Run `go test -race ./internal/loopserver/...` — PASS (pump goroutine + encoder mutex).
- [ ] Commit: `feat(loopserver): #45 loop.observe replay-then-live with gap markers`.

### Deliverable
`loop.observe` returns a replay watermark and streams live `loop.event` notifications with gap
detection, verified with a raw JSON-RPC test client over a pipe.

---

## Task 8 — `fuse loop-server` subcommand wired into dispatch

Add `runLoopServer` and hook it into `cmd/fuse/main.go`'s `switch args[0]` dispatch. This
binding auto-approves (no gate) and reuses the one-shot Runtime deps builder for BuildAgent/
BuildChild, but with a REAL fsstore (BaseDir = session dir) so observe/attach have durable
history.

### Files
- Create `cmd/fuse/loop_server.go` — `func runLoopServer(args []string, cfg config.Config,
  reg *model.Registry, stdout, stderr io.Writer) int`.
- Modify `cmd/fuse/main.go` — add `case "loop-server": return runLoopServer(args[1:], cfg,
  reg, stdout, stderr)` to the dispatch switch (currently ~lines 51–80), and a help line.

### Interfaces
- Consumes: `loopserver.NewServer`, `runtime.New`, `runtime.Deps`, `permissions.AlwaysApprove`,
  `os.Stdin`, `os.Stdout`, `session.DefaultLogDir`.
- Produces (exact Go signature):

```go
func runLoopServer(args []string, cfg config.Config, reg *model.Registry, stdout, stderr io.Writer) int
```

### Body (no placeholders)

```go
func runLoopServer(_ []string, cfg config.Config, reg *model.Registry, _ io.Writer, stderr io.Writer) int {
    // Auto-approve is THIS binding's policy (documented): headless loop control has no human
    // on a TTY. It is not a property of the Runtime seam.
    approve := permissions.AlwaysApprove

    skillSet, serr := skills.LoadWithEmbedded(skills.DefaultDirs())
    if serr != nil {
        fmt.Fprintf(stderr, "skills error: %v\n", serr)
        return 1
    }
    systemBlock := skillSet.SystemPromptBlock() + spawnAgentBlock
    toolReg := defaultToolRegistry(cfg.Research, skillSet.Lookup)

    // Reuse the one-shot deps wiring but with a REAL event store so observe/attach have
    // durable history. Renderer is a discarding renderer — binding #2 has no display.
    deps := buildLoopServerRuntimeDeps(cfg, reg, reg.Default, toolReg, systemBlock, approve, sessionRateGate(cfg))
    rt := runtime.New(deps)

    srv := loopserver.NewServer(os.Stdin, os.Stdout, rt)
    if err := srv.Serve(context.Background()); err != nil {
        fmt.Fprintf(stderr, "loop-server: %v\n", err)
        return 1
    }
    return 0
}
```

`buildLoopServerRuntimeDeps` mirrors `buildOneShotRuntimeDeps` but: renderer is a
`discardRenderer` (a nop `agent.Renderer` local to cmd/fuse), `BaseDir` is
`session.DefaultLogDir()` (real fsstore), and `InstallGlobalStore: setActiveEventStore` so the
child-builder readers see the store.

### Steps
- [ ] Write `cmd/fuse/loop_server_test.go` `TestLoopServerDispatchRegistered`: call
      `run([]string{"loop-server"}, ...)` with stdin an immediately-closed reader (an
      `io.Pipe` whose writer is closed) so `Serve` returns nil at EOF; assert exit 0. Also a
      `TestHelpListsLoopServer` asserting `run([]string{"help"})` output mentions `loop-server`.
- [ ] Run — FAIL (`runLoopServer` undefined, case missing).
- [ ] Implement `runLoopServer` + `buildLoopServerRuntimeDeps` + `discardRenderer`; add the
      dispatch case and the help line.
- [ ] Run `go test ./cmd/fuse/...` — PASS.
- [ ] Assert `cmd/fuse/mcp_server.go` untouched: `git diff --exit-code -- cmd/fuse/mcp_server.go`
      returns clean. Assert `internal/mcp` untouched: `git diff --exit-code -- internal/mcp/`.
- [ ] Commit: `feat(cmd): #45 add fuse loop-server subcommand (binding #2)`.

### Deliverable
`fuse loop-server` is a real subcommand: a headless stdio JSON-RPC loop-control server backed
by the in-process Runtime, with `cmd/fuse/mcp_server.go` and `internal/mcp` byte-identical.

---

## Task 9 — two-bindings-one-seam parity test (real binary against gateway double)

Prove spec Verification bullet 2: the SAME `event.Event` stream from a CLI run and a
`loop-server` run for the same task, each fed its OWN real production source
(learning `parity-test-feeds-each-side-its-own-production-source`) — NOT a shared synthetic
input.

### Files
- Create `cmd/fuse/two_bindings_parity_test.go`.

### Interfaces
- Consumes: `run` (the cmd entry), a scripted `httptest` gateway, `loopserver` frames read via
  a raw JSON-RPC client over a pipe, `fsstore` (to read each side's events.jsonl) or the
  `loop.event` notification stream.

### Test design (no placeholders)

- Stand up ONE scripted gateway handler (a deterministic single-turn assistant reply, no tool
  calls) shared as the MODEL only — each binding drives its OWN engine against it, so the
  event streams are produced independently, not copied.
- **CLI side:** `t.Setenv("LLM_GATEWAY_URL", srv.URL)`, `t.Setenv("HOME", cliHome)`; run
  `run([]string{"research-probe", "TASK"}, ...)` (research-probe uses a real fsstore under
  `cliHome/.fuse/sessions/<root>/events.jsonl` — the durable stream). After it returns, read
  that events.jsonl via `fsstore` replay OR parse the file, collect the `Kind` sequence and
  key payload fields (turn.start, model.call.start/end, turn.end).
- **loop-server side:** `t.Setenv("HOME", serverHome)`; construct the server in-process
  (`loopserver.NewServer(pr, pw, runtime.New(buildLoopServerRuntimeDeps(...)))` with the same
  gateway env), send `loop.start{task:"TASK"}`, capture `loop_id`, send
  `loop.observe{loop_id, from_seq:0}`, and read the `loop.event` notification stream (raw JSON
  client). Collect the same `Kind` sequence + key fields.
- Assert the two `Kind` sequences are equal and the load-bearing payload fields match
  (model id, turn numbers). Ignore volatile fields (Seq numbering per store, TS, node IDs)
  by normalizing: compare `[]Kind` and, per event, the kind-specific payload with node IDs
  and timestamps stripped.

### Steps
- [ ] Write `two_bindings_parity_test.go` per the design; use the `structured_delegation_e2e_test.go`
      gateway-handler pattern for the scripted double and the `internal/loopserver`
      raw-client pattern from Task 7.
- [ ] Run — FAIL if any relocated emission diverges (this is the guard for learning
      `relocated-emission-resource-every-projected-field`; a collapsed error on a
      max-turns/loop stop path would show here).
- [ ] Fix any divergence by re-sourcing the diverging field from what the seam has in hand
      (per the same learning) — no behavior guess; the fix is to feed the emission the raw
      value, not a projected one.
- [ ] Run — PASS.
- [ ] Run `go test -race ./cmd/fuse/... ./internal/loopserver/... ./internal/runtime/...` — PASS.
- [ ] Commit: `test(cmd): #45 two-bindings-one-seam event-stream parity`.

### Deliverable
A test proving a CLI run and a `loop-server` run emit the identical normalized `event.Event`
stream for the same task, each from its own production source.

---

## Task 10 — reattach test + import-direction + policy-free guards + full race gate

Prove the remaining Verification bullets: reattach (replay-then-resume, no gap/dup), the
import cycle is broken, the seam is policy-free, and the whole suite passes under race.

### Files
- Create `internal/loopserver/reattach_test.go`.
- Create `internal/runtime/import_direction_test.go`.
- Create `internal/runtime/policy_free_test.go`.

### Test designs (no placeholders)

- **Reattach** (`reattach_test.go`): with a `fakeRuntime` (or a real in-proc Runtime driven by
  a fake Completer that emits N events across turns), subscribe via `loop.observe(from:0)`,
  read the first K `loop.event` notifications, then STOP reading (simulate disconnect) — the
  store keeps appending (ADR-0025 non-blocking, possibly dropping). Re-issue
  `loop.observe(from:lastSeenSeq)`; assert every event with Seq in (lastSeen, current] is
  delivered exactly once via replay, then live resumes, with NO duplicate of an
  already-seen Seq and NO missing Seq in the replayed range.

- **Import direction** (`import_direction_test.go`): a Go test that runs
  `go list -deps ./internal/agent/... ./internal/tools/...` (via `exec.Command`) and asserts
  the output contains NO `github.com/ethanhinson/fuse/internal/runtime`. And asserts
  `go list -deps github.com/ethanhinson/fuse/internal/runtime` DOES contain
  `internal/agent` and `internal/event` (the seam composes them). Build-tag guard so it only
  runs where `go` is on PATH.

- **Policy-free** (`policy_free_test.go`): `go list -deps .../internal/runtime` must NOT
  contain `internal/tui`, `internal/mcp`, `internal/permissions` (renderer/TUI/MCP/gate), and
  the `internal/runtime` source files must not import those packages. A second sub-assertion
  greps the `internal/runtime/*.go` (non-test) source for the literal strings
  `"internal/tui"`, `"internal/mcp"`, `"internal/permissions"` and fails if present.

### Steps
- [ ] Write `reattach_test.go`; run — FAIL if replay/live handoff drops or dups (surfaces any
      off-by-one in the `from_seq` boundary: `Replay` returns `Seq > from`, and the live pump
      must not also re-emit an already-replayed Seq — verify the subscribe-before-replay
      ordering closes the seam).
- [ ] Fix boundary if needed (Observe subscribes before Attach so nothing is missed; the pump
      must skip Seq <= the replay watermark to avoid a duplicate at the handoff — add a
      `prev` initialized to the replay watermark, already in Task 7's body; assert it here).
- [ ] Run `reattach_test.go` — PASS.
- [ ] Write `import_direction_test.go` and `policy_free_test.go`; run — they PASS (or FAIL and
      force a real fix if any accidental import crept in during Tasks 4–8).
- [ ] Run the FULL race gate: `make test-race` — PASS.
- [ ] Run the FULL suite: `go test ./...` — PASS.
- [ ] Commit: `test(runtime): #45 reattach, import-direction, and policy-free guards; full race gate green`.

### Deliverable
Reattach is proven lossless and duplication-free; the import direction and policy-free
constraints are enforced by tests; `go test ./...` and `make test-race` are green.

---

## Self-Review Pass

Before opening the PR, re-read the plan against these checks and fix any gap.

### Spec coverage (every Verification bullet has a proving task)
- [ ] Interface is real & CLI consumes it — Tasks 1–5; the grep/Go guard is Task 5's
      `TestNoDirectEngineDriveAtCmdSites`; unchanged behavior via the existing cmd/fuse suite.
- [ ] Two bindings, one seam (same event.Event stream) — Task 9, each side its own source.
- [ ] Reattach works (replay then resume, no gap/dup) — Task 10 `reattach_test.go`.
- [ ] Policy-free seam (no renderer/TUI/MCP type in internal/runtime) — Task 10
      `policy_free_test.go`.
- [ ] Out-of-scope untouched: `cmd/fuse/mcp_server.go` + `internal/mcp` byte-identical
      (Task 8 `git diff --exit-code`); spawn stays in-process (Runtime.Spawn wraps
      `agent.Spawner` only); `go test -race ./...` green (Task 10).
- [ ] loop.start/loop.send/loop.observe all implemented — Tasks 6–7.
- [ ] Live-tail via notifications + replay catch-up — Task 7; back-pressure gap marker — Task 7.

### Placeholder scan (no TBD / "add error handling" / "similar to Task N")
- [ ] Every code step contains real Go: interface (T1), StartLoop (T2), the four methods (T3),
      deps builders + migration tails (T4–5), JSON-RPC dispatch/handlers (T6), observe pump
      (T7), subcommand (T8). The only temporary bodies are the four `panic("not implemented")`
      stubs in Task 2 and the `handleObserve` method-not-found stub in Task 6, each explicitly
      REPLACED with a full body in the very next task (T3, T7) — none ship.
- [ ] No step says "similar to" without inlining the exact code; the three deps builders each
      list their unique wiring (one-shot NoopStore/render; shell TUI+humanbus+projected-log;
      probe MultiRenderer+workflow; loop-server discard-render+real-fsstore+auto-approve).

### Type consistency (signatures line up producer→consumer)
- [ ] `runtime.New` returns `Runtime` (fixed in T4) — cmd/fuse consumes the interface.
- [ ] `LoopHandle.Wait() ([]model.Message, error)` — T4/T5 tails consume both returns; the
      `model` import is in `runtime.go`.
- [ ] `SpawnHandle` methods (`NodeID()/Wait() agent.SpawnDone/Result() (any,error)`) match
      `agent.AgentHandle`'s field/methods exactly (NodeID is a FIELD on AgentHandle, so
      `spawnHandle.NodeID()` returns `s.h.NodeID`).
- [ ] `Runtime.Observe` returns `(<-chan event.Event, func(), error)` matching
      `EventStore.Subscribe`'s `(<-chan Event, func())` plus the loop-not-found error.
- [ ] `Runtime.Attach(loopID string, from event.Seq) ([]event.Event, error)` matches
      `EventStore.Replay(from Seq) ([]Event, error)`.
- [ ] `Send` uses `agent.ModeRespond` (verified: `MsgMode`, `ModeRespond` exists) and
      `HumanBus.Enqueue(nodeID, mode, handle, text)` (verified arity/types).
- [ ] `agent.New(...)` call in deps builders matches the 7-arg signature
      `(Completer, ToolExecutor, Renderer, modelID, systemPrompt string, maxTurns, maxTokens int)`.
- [ ] `Deps.Tree *agent.AgentTree` (added in T5) is consumed by `StartLoop` (create-if-nil) and
      returned to the probe for `probe.Summarize(logSink, tree)`.
- [ ] loop-server wire frames (`req`/`resp`/`rpcError`) mirror mcp/server.go's shapes; the
      `event.Event` in `eventNoteParams` marshals with its existing JSON tags (no new tags).

### Learnings honored
- [ ] break-import-cycle — T10 import-direction test + T1 import discipline.
- [ ] patch-every-cloned-child-builder — T4/T5 grep re-derivation + T5 no-direct-drive test.
- [ ] relocated-emission — T9 parity test guards byte-equivalence incl. max-turns/loop stop.
- [ ] parity-test-feeds-each-side-its-own-source — T9 drives two independent engines.
- [ ] verify-tool-loop-at-gateway-seam — T4/T9 use scripted LLM_GATEWAY_URL + real `run()`.
- [ ] mcp-read-pumps-drop-inbound-notifications — T7/T9/T10 use a RAW JSON-RPC client, never
      fuse's mcp client, to receive `loop.event` notifications.
