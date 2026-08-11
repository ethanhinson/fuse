package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/mcp"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/permissions"
	"github.com/ethanhinson/fuse/internal/probe"
	"github.com/ethanhinson/fuse/internal/runtime"
	"github.com/ethanhinson/fuse/internal/session"
	"github.com/ethanhinson/fuse/internal/toolidentity"
	"github.com/ethanhinson/fuse/internal/tools"
	"github.com/ethanhinson/fuse/internal/tui"
)

// eventStoreHolder is a per-loop, INSTANCE-scoped indirection that carries the
// Runtime-owned event store from BuildAgent (where StartLoop hands it in as a
// value) to the child-builder / spawner closures a Deps builder wires eagerly at
// construction time (change 0046). It replaces the retired process-global
// currentEventStore()/setActiveEventStore holder: because each Deps builder creates
// its OWN holder, N concurrent single-loop bindings never clobber each other. It is
// mutex-guarded because the child-builder/spawner read it on child-spawn goroutines
// while BuildAgent sets it on the StartLoop goroutine. Unset ⇒ get() returns the
// no-op default so an emission before StartLoop never nil-panics.
type eventStoreHolder struct {
	mu    sync.RWMutex
	store event.EventStore
}

func (h *eventStoreHolder) set(s event.EventStore) {
	h.mu.Lock()
	h.store = s
	h.mu.Unlock()
}

func (h *eventStoreHolder) get() event.EventStore {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.store == nil {
		return event.NoopStore{}
	}
	return h.store
}

// runtime_binding.go is CLI binding #1's construction seam over internal/runtime.
// Each entry point (one-shot, shell, research-probe) assembles a runtime.Deps that
// closes over its own renderer / approval gate / tool wiring — those cmd-layer
// policy types stay in cmd/fuse and never cross into the Runtime. The Runtime
// receives only construction closures (BuildAgent, the per-loop factory that also
// returns the loop's child-builder) plus the store ownership hooks (BaseDir / Tree).
// The child-builder closures here are the ONLY cmd-site callers of a.Run() — they are
// the child runner the Runtime's Spawner invokes, not a root-loop drive (see
// TestNoDirectEngineDriveAtCmdSites). The per-loop event store flows in as a VALUE
// through BuildAgent + a per-Deps-instance eventStoreHolder (change 0046), never a
// process-global.

// buildOneShotRuntimeDeps assembles runtime.Deps for the one-shot entry point,
// closing over the resolved cfg/reg/toolReg/renderer/approve wiring exactly as the
// pre-migration builder did. The renderer, approval gate, and tool registry stay in
// cmd/fuse — the seam receives only construction closures, never those types by
// name. BaseDir is "" so the Runtime installs a NoopStore (one-shot writes no event
// log — byte-identical to pre-migration). BuildAgent publishes the Runtime-owned
// store to the per-loop holder so the eagerly-wired child-builder / spawner closures
// emit onto it (change 0046 — no process-global).
func buildOneShotRuntimeDeps(cfg config.Config, reg *model.Registry, modelAlias string,
	toolReg *tools.Registry, tree *agent.AgentTree, stdout io.Writer, verbose bool, traceW io.Writer,
	rootApprove permissions.ApprovalFunc, oneShotSystemBlock string, oneShotBudget bool,
	rateGate model.RateGate) runtime.Deps {

	sched := tree.Scheduler()
	rootNode := tree.Node(tree.RootID())
	// One blackboard per session, shared by every agent in the tree (change 0023).
	bb := agent.NewBlackboard(tree)
	// Per-loop store holder (change 0046): BuildAgent sets it from the Runtime-owned
	// store; the child-builder/spawner closures below read it instead of a global.
	storeHolder := &eventStoreHolder{}

	// MCP attach on the one-shot path (change #59, Task 5): route through the SAME
	// shared helper every binding uses, so one-shot can list + invoke MCP tools with
	// #52 identity propagation. mcp.NewManager registers the configured servers' tools
	// into the one-shot registry (mcpOpts carry the egress CredentialSource); the loop
	// initiator is seeded as localPrincipal(cfg) (→ DefaultTenant) via LoopContext
	// below, mirroring the shell's local-identity model — NO new CLI identity flag
	// (explicit non-goal). The mediator reaches the gate via buildGate →
	// buildTargetMediator. One-shot is single-loop-per-process, so one manager over the
	// one registry; it is closed at loop completion via LoopTeardown. A no-op when no
	// MCP server is configured (no dial, no goroutines).
	mcpOpts, _ := mcpAttach(cfg, os.Stderr)
	var oneShotMCP *mcp.Manager
	if mgr, err := mcp.NewManager(cfg.MCPServers, toolReg, mcpOpts...); err != nil {
		fmt.Fprintf(os.Stderr, "one-shot: mcp manager: %v\n", err)
	} else {
		oneShotMCP = mgr
	}

	var makeSpawner func(parentNode *agent.AgentNode, depth int) *agent.Spawner
	makeSpawnFunc := func(parentNode *agent.AgentNode, depth int) tools.SpawnFunc {
		return spawnFuncFrom(makeSpawner(parentNode, depth), sched, parentNode)
	}
	// childBuilder is the loop's per-child runner (cloned child-builder site 1 of 3,
	// learning patch-every-cloned-child-builder). It is BOTH the ChildBuilder every
	// makeSpawner wires (so the root's spawn_agent tool fan-out is unchanged) AND the
	// child-builder BuildAgent returns to the Runtime for its own Spawner (change 0046
	// retired the separate Deps.BuildChild field). Extracted to a named var so the same
	// closure backs both.
	childBuilder := func(ctx context.Context, opts agent.SpawnOpts, childNode *agent.AgentNode, childTree *agent.AgentTree) (string, error) {
		childToolReg, terr := childToolRegistry(toolReg, opts.Tools)
		if terr != nil {
			return "", terr
		}
		if childNode.Depth >= agent.MaxDepth || !shouldWireChildSpawn(opts.Tools) {
			// Depth strip (static): a child at MaxDepth can never spawn.
			// Folded-in fix (change 0034): a parent that omits spawn_agent
			// from its requested tools subset withholds it from the child.
			// Either way, drop any copy inherited from the parent's registry.
			childToolReg.Unregister("spawn_agent")
		} else {
			childToolReg.Register(tools.NewSpawnAgentToolWithBudget(makeSpawnFunc(childNode, childNode.Depth), sched.SpawnBudget).
				WithQuotaWarning(quotaWarningFor(tree, childNode.ID)))
		}
		// Blackboard tools bound to the child's provenance — always wired
		// (not spawn-gated), honoring an explicit subset that omits them.
		wireChildBlackboard(childToolReg, bb, childNode, opts.Tools)
		// pipeline_run bound to the child's own spawner — always wired unless
		// an explicit subset omits it (mirrors the blackboard wiring). (0026)
		pipeChildWorkers, pipeChildTools := pipelineSynthPalette(cfg, childToolReg)
		childSpawner := makeSpawner(childNode, childNode.Depth)
		wirePipelineTool(childToolReg,
			makePipelineRunFn(childSpawner, bb, sched, childNode),
			makePipelineSynthFn(childSpawner, bb, sched, childNode, cfg, pipeChildWorkers, pipeChildTools, traceW),
			opts.Tools)

		r := tui.NewRenderer(stdout, verbose)
		modelID := opts.ModelID
		if modelID == "" {
			modelID = modelAlias
		}
		var a *agent.Agent
		var aerr error
		// Subagent approvals route to the parent channel, prefixed so the
		// human sees which child is asking (same posture as CloneForChild).
		childApprove := permissions.PrefixApproval(opts.Label, rootApprove)
		// One-shot passes no session-mode source: gates default to
		// cfg.Permissions.Mode exactly as before this seam.
		if opts.SystemPrompt != "" {
			a, aerr = buildChildAgent(cfg, reg, modelID, r, opts.SystemPrompt, childToolReg, childApprove, traceW, opts.Label, nil, oneShotBudget, rateGate, nil)
		} else {
			a, aerr = buildAgentWithRendererAndTrace(cfg, reg, modelID, r, verbose, oneShotSystemBlock, childToolReg, childApprove, traceW, opts.Label, nil, oneShotBudget, rateGate, nil)
		}
		if aerr != nil {
			return "", aerr
		}
		a.SetStripSpawn(sched.StripPredicate(childNode.ID))
		a.SetExpects(opts.ExpectsSchema(), opts.ExpectsSink()) // 0042: structured-delegation return_result channel
		// Event stream wiring (change 0043/0046): the child emits into the loop's own
		// store, resolved from the per-loop holder (no process-global).
		a.SetEventSink(storeHolder.get())
		a.SetNodeIdentity(childNode.ID, childNode.ParentID, childNode.Depth)
		// spawn.start / spawn.done are emitted by the Spawner (change 0044),
		// the single choke point — no per-site emission here.
		msgs, rerr := a.Run(ctx, []model.Message{{Role: "user", Content: opts.Task}})
		// Report the RAW run error to the Spawner's spawn.done event (change
		// 0044) so its `kind` matches the direct session-log write even when
		// childResult collapses a max-turns/loop stop into a partial-success
		// string.
		opts.RunErrSink().Set(rerr)
		return childResult(msgs, rerr)
	}
	makeSpawner = func(parentNode *agent.AgentNode, depth int) *agent.Spawner {
		return agent.NewSpawner(
			agent.WithTree(tree),
			agent.WithNode(parentNode),
			agent.WithSpawnDepth(depth),
			// Spawn lifecycle events (change 0044/0046): the Spawner is the single choke
			// point that emits spawn.start/spawn.done onto the loop's own store.
			agent.WithEventStore(storeHolder.get()),
			agent.WithChildBuilder(childBuilder),
		)
	}
	toolReg.Register(tools.NewSpawnAgentToolWithBudget(makeSpawnFunc(rootNode, 0), sched.SpawnBudget).
		WithQuotaWarning(quotaWarningFor(tree, rootNode.ID)))
	// Root-node-wired blackboard tools (provenance = rootNode).
	wireRootBlackboard(toolReg, bb, rootNode)
	// Root pipeline_run bound to the root spawner (provenance = rootNode). (0026)
	rootPipeWorkers, rootPipeTools := pipelineSynthPalette(cfg, toolReg)
	rootPipeSpawner := makeSpawner(rootNode, 0)
	wirePipelineTool(toolReg,
		makePipelineRunFn(rootPipeSpawner, bb, sched, rootNode),
		makePipelineSynthFn(rootPipeSpawner, bb, sched, rootNode, cfg, rootPipeWorkers, rootPipeTools, traceW),
		nil)

	return runtime.Deps{
		Tree:            tree,
		BaseDir:         "", // NoopStore: one-shot writes no event log (byte-identical).
		MaxConcurrent:   cfg.Agents.MaxConcurrent,
		NewToolRegistry: func() *tools.Registry { return toolReg },
		// LoopContext seeds the one-shot loop initiator as the config-derived local
		// principal (change #59, Task 5) — subject from cfg.ToolIdentity.LocalSubject,
		// DefaultTenant — exactly the shell's local-identity model, so identity-propagating
		// MCP calls mint a per-call credential from it. No per-invocation tenant/principal
		// input (explicit non-goal). Stamped at the composition root, not the runtime seam
		// (ADR-0030).
		LoopContext: func(ctx context.Context, _ runtime.LoopConfig) context.Context {
			return toolidentity.WithPrincipal(ctx, localPrincipal(cfg))
		},
		// LoopTeardown closes the one-shot MCP manager at loop completion so its
		// read-pump/notify goroutines do not outlive the run.
		LoopTeardown: func(_ *tools.Registry) {
			if oneShotMCP != nil {
				oneShotMCP.Close()
			}
		},
		BuildAgent: func(store event.EventStore, _ *agent.AgentTree, modelID string, reg2 *tools.Registry) (*agent.Agent, agent.ChildBuilder, string, error) {
			// Publish the Runtime-owned store to the per-loop holder so the eagerly-wired
			// child-builder/spawner closures above emit onto THIS loop's store (change
			// 0046). One-shot is single-loop-per-process; the holder is instance state.
			storeHolder.set(store)
			a, mid, err := buildAgentCore(cfg, reg, modelAlias, tui.NewRenderer(stdout, verbose), oneShotSystemBlock, traceW, "root", reg2, rootApprove, nil, oneShotBudget, rateGate, nil)
			if err != nil {
				return nil, nil, "", err
			}
			a.SetStripSpawn(sched.StripPredicate(rootNode.ID))
			// Event stream wiring (change 0043/0046): the root emits onto the loop's own
			// store. SetEventSink is also called by StartLoop with the same store —
			// symmetric, inert here.
			a.SetEventSink(store)
			a.SetNodeIdentity(rootNode.ID, rootNode.ParentID, rootNode.Depth)
			return a, childBuilder, mid, nil
		},
	}
}

// researchProbeDepsInput carries the research-probe binding's wiring into its Deps
// builder (many params — a struct keeps the call site readable). act is the optional
// research workflow activation; logSink is the shared probe recorder Log.
type researchProbeDepsInput struct {
	cfg      config.Config
	reg      *model.Registry
	alias    string
	toolReg  *tools.Registry
	tree     *agent.AgentTree
	act      *workflowActivation
	rootID   string
	logSink  *probe.Log
	traceW   io.Writer
	rateGate model.RateGate
}

// buildResearchProbeRuntimeDeps assembles runtime.Deps for the research-probe entry
// point. It preserves the probe's unique wiring: MultiRenderer (tree node +
// recorder), workflow activation (worker allowlists, pool backstop), AlwaysApprove
// (headless). BaseDir is "" (NoopStore — the probe writes no event log). The
// tree is supplied by the caller (Deps.Tree) so probe.Summarize still sees it after
// h.Wait(). Cloned child-builder site 3 of 3 (learning patch-every-cloned-child-builder).
//
// MCP attach choice (change #59, Task 5): the research-probe deliberately does NOT
// attach an MCP manager. It is a bounded web-research fan-out over a fixed,
// research-specific tool palette (web_search/web_fetch + spawn/blackboard/pipeline)
// driving workflow workers; it exposes no configured downstream MCP target and has no
// authenticated per-user identity to propagate (it runs under AlwaysApprove with no
// principal surface). Attaching MCP here would add downstream reach the probe's role
// does not call for. The two loop bindings that DO carry MCP — the shell and the
// loop-server (and the one-shot local path) — go through mcpAttach; this site is the
// documented exception, not an accidental omission.
func buildResearchProbeRuntimeDeps(in researchProbeDepsInput) runtime.Deps {
	cfg, reg, alias := in.cfg, in.reg, in.alias
	toolReg, tree, act, rootID := in.toolReg, in.tree, in.act, in.rootID
	logSink, traceW, rateGate := in.logSink, in.traceW, in.rateGate

	sched := tree.Scheduler()
	rootNode := tree.Node(tree.RootID())
	// One blackboard per session, shared by every agent in the tree (change 0023).
	bb := agent.NewBlackboard(tree)
	// Per-loop store holder (change 0046): set by BuildAgent from the Runtime-owned
	// store; read by the child-builder/spawner closures below.
	storeHolder := &eventStoreHolder{}

	var makeSpawner func(parentNode *agent.AgentNode, depth int) *agent.Spawner
	makeSpawnFunc := func(parentNode *agent.AgentNode, depth int) tools.SpawnFunc {
		return spawnFuncFrom(makeSpawner(parentNode, depth), sched, parentNode)
	}
	childBuilder := func(ctx context.Context, opts agent.SpawnOpts, childNode *agent.AgentNode, childTree *agent.AgentTree) (string, error) {
		// Resolve the effective tool list: inside a workflow a selected worker's
		// allowlist is authoritative (opts.Tools may only narrow it); outside,
		// opts.Tools is the freeform subset. (change 0034)
		effectiveTools := opts.Tools
		if act != nil {
			rt, rerr := resolveWorkerTools(act.cfg, opts.Worker, opts.Tools)
			if rerr != nil {
				return "", rerr
			}
			effectiveTools = rt
		}
		childToolReg, terr := childToolRegistry(toolReg, effectiveTools)
		if terr != nil {
			return "", terr
		}
		if childNode.Depth >= agent.MaxDepth || !shouldWireChildSpawn(effectiveTools) {
			// Depth strip (static): a child at MaxDepth can never spawn.
			// Folded-in fix (change 0034): a parent (or worker allowlist)
			// that omits spawn_agent withholds it from the child.
			childToolReg.Unregister("spawn_agent")
		} else {
			spawnTool := tools.NewSpawnAgentToolWithBudget(makeSpawnFunc(childNode, childNode.Depth), budgetFor(tree, act, rootID)).
				WithQuotaWarning(quotaWarningFor(tree, childNode.ID))
			if act != nil {
				spawnTool = spawnTool.WithWorkers(act.workerNames())
			}
			childToolReg.Register(spawnTool)
		}
		// Blackboard tools bound to the child's provenance — always wired
		// (not spawn-gated), honoring an explicit subset that omits them.
		// effectiveTools is the list childToolRegistry actually built from.
		wireChildBlackboard(childToolReg, bb, childNode, effectiveTools)
		// pipeline_run bound to the child's own spawner — always wired unless
		// an explicit subset omits it (mirrors the blackboard wiring). (0026)
		pipeChildWorkers, pipeChildTools := pipelineSynthPalette(cfg, childToolReg)
		childSpawner := makeSpawner(childNode, childNode.Depth)
		wirePipelineTool(childToolReg,
			makePipelineRunFn(childSpawner, bb, sched, childNode),
			makePipelineSynthFn(childSpawner, bb, sched, childNode, cfg, pipeChildWorkers, pipeChildTools, traceW),
			effectiveTools)

		label := childNode.Label
		if label == "" {
			label = "child"
		}
		r := tui.NewMultiRenderer(
			tui.NewNodeRenderer(childNode, childTree),
			logSink.Recorder(label),
		)

		modelID := opts.ModelID
		// A worker's optional model pin applies when the caller did not
		// pick a model explicitly (change 0034).
		if modelID == "" && act != nil && opts.Worker != "" {
			if w, ok := act.cfg.Workers[opts.Worker]; ok && w.Model != "" {
				modelID = w.Model
			}
		}
		if modelID == "" {
			modelID = alias
		}
		var a *agent.Agent
		var aerr error
		// research-probe is headless (no TTY, AlwaysApprove) ⇒ backstopped.
		if opts.SystemPrompt != "" {
			a, aerr = buildChildAgent(cfg, reg, modelID, r, opts.SystemPrompt, childToolReg, permissions.AlwaysApprove, traceW, label, nil, false, rateGate, nil)
		} else {
			a, _, aerr = buildAgentCore(cfg, reg, modelID, r, spawnAgentBlock, traceW, label, childToolReg, permissions.AlwaysApprove, nil, false, rateGate, nil)
		}
		if aerr != nil {
			return "", aerr
		}
		// Unified visibility predicate (change 0036): the scheduler folds
		// the global brakes, the workflow pool (when the node is inside a
		// registered subtree), and the queue bound into one rule — tighter
		// scope wins.
		a.SetStripSpawn(sched.StripPredicate(childNode.ID))
		a.SetExpects(opts.ExpectsSchema(), opts.ExpectsSink()) // 0042: structured-delegation return_result channel
		// Event stream wiring (change 0043/0046) — cloned child-builder site 3 of 3:
		// the child emits onto the loop's own store via the per-loop holder.
		a.SetEventSink(storeHolder.get())
		a.SetNodeIdentity(childNode.ID, childNode.ParentID, childNode.Depth)
		// spawn.start / spawn.done are emitted by the Spawner (change 0044).
		msgs, rerr := a.Run(ctx, []model.Message{{Role: "user", Content: opts.Task}})
		// Raw run error → Spawner spawn.done (change 0044): matches the
		// session-log `kind` even when childResult collapses a stop to success.
		opts.RunErrSink().Set(rerr)
		return childResult(msgs, rerr)
	}
	makeSpawner = func(parentNode *agent.AgentNode, depth int) *agent.Spawner {
		return agent.NewSpawner(
			agent.WithTree(tree),
			agent.WithNode(parentNode),
			agent.WithSpawnDepth(depth),
			agent.WithSpawnBackstop(backstopFor(tree, act, rootID)),
			// Spawn lifecycle events (change 0044/0046): emitted by the Spawner choke
			// point onto the loop's own store.
			agent.WithEventStore(storeHolder.get()),
			agent.WithChildBuilder(childBuilder),
		)
	}
	rootSpawn := tools.NewSpawnAgentToolWithBudget(makeSpawnFunc(rootNode, 0), budgetFor(tree, act, rootID)).
		WithQuotaWarning(quotaWarningFor(tree, rootNode.ID))
	if act != nil {
		// The root offers the workflow's typed workers so the model selects a
		// facet-researcher per facet instead of hand-assembling a toolset.
		rootSpawn = rootSpawn.WithWorkers(act.workerNames())
	}
	toolReg.Register(rootSpawn)
	// Root-node-wired blackboard tools (provenance = rootNode).
	wireRootBlackboard(toolReg, bb, rootNode)
	// Root pipeline_run bound to the root spawner (provenance = rootNode). (0026)
	rootPipeWorkers, rootPipeTools := pipelineSynthPalette(cfg, toolReg)
	rootPipeSpawner := makeSpawner(rootNode, 0)
	wirePipelineTool(toolReg,
		makePipelineRunFn(rootPipeSpawner, bb, sched, rootNode),
		makePipelineSynthFn(rootPipeSpawner, bb, sched, rootNode, cfg, rootPipeWorkers, rootPipeTools, traceW),
		nil)

	return runtime.Deps{
		Tree:            tree,
		BaseDir:         "", // NoopStore: research-probe writes no event log (byte-identical).
		MaxConcurrent:   cfg.Agents.MaxConcurrent,
		NewToolRegistry: func() *tools.Registry { return toolReg },
		BuildAgent: func(store event.EventStore, _ *agent.AgentTree, modelID string, reg2 *tools.Registry) (*agent.Agent, agent.ChildBuilder, string, error) {
			// Publish the Runtime-owned store so the eagerly-wired child-builder/spawner
			// closures emit onto THIS loop's store (change 0046). Research-probe is
			// single-loop-per-process; the holder is instance state.
			storeHolder.set(store)
			// Root renderer: tree node + recorder, same MultiRenderer shape as children.
			rootR := tui.NewMultiRenderer(
				tui.NewNodeRenderer(rootNode, tree),
				logSink.Recorder("root"),
			)
			a, mid, err := buildAgentCore(cfg, reg, alias, rootR, spawnAgentBlock, traceW, "root", reg2, permissions.AlwaysApprove, nil, false, rateGate, nil)
			if err != nil {
				return nil, nil, "", err
			}
			// The root is the workflow root: its own spawn schema is governed by the
			// workflow pool composed with the global brakes, folded into the
			// scheduler's unified visibility predicate (change 0036).
			a.SetStripSpawn(sched.StripPredicate(rootNode.ID))
			// Event stream wiring (change 0043/0046): symmetric with the child sites.
			a.SetEventSink(store)
			a.SetNodeIdentity(rootNode.ID, rootNode.ParentID, rootNode.Depth)
			return a, childBuilder, mid, nil
		},
	}
}

// shellDepsInput carries the interactive shell's wiring into its Deps builder. The
// shell is unique among the three bindings: the bubbletea TUI owns the turn cadence
// and per-turn renderer, so its root agent is built by the TUI's own AgentBuilder
// seam (which needs the per-turn renderer). buildShellRuntimeDeps therefore routes
// the CHILD builder + tool registration + store ownership through runtime.Deps while
// the TUI keeps rendering/turn cadence, human messaging, projected-log consumer, and
// segment sink exactly as before (plan NOTE on shell.go).
type shellDepsInput struct {
	cfg     config.Config
	reg     *model.Registry
	alias   string
	toolReg *tools.Registry
	tree    *agent.AgentTree
	// bb is the session blackboard the TUI's /agents overlay also reads — it MUST be
	// the same instance the shell passes to m.WithBlackboard, so the tools wired here
	// write into the blackboard the overlay observes. May be nil in the wiring test.
	bb          *agent.Blackboard
	verbose     bool
	skillBlock  string
	sessionMode *permissions.SessionMode
	humanBus    *agent.HumanBus
	handleReg   *agent.HandleRegistry
	sessLog     *session.Logger
	traceW      io.Writer
	rateGate    model.RateGate
	logDir      string
	// eventStore is the shell's own session event store, opened in shell.go BEFORE this
	// builder runs (the TUI — not StartLoop — drives shell turns, so the shell owns the
	// store lifecycle). It seeds the per-loop holder directly so the child-builder /
	// spawner closures emit onto the same store the shell's own root build closure uses
	// (change 0046 — replaces the retired currentEventStore() global). May be nil (the
	// wiring test, or an open failure) ⇒ the holder's no-op default.
	eventStore event.EventStore
	// segmentSink is the shell's own per-session SegmentSink (change 0030), threaded
	// per-loop through installSummarizer instead of the retired setActiveSegmentSink
	// global (change 0046). Nil ⇒ the agent's no-op default. It is the sink the shell's
	// own root build closure (shell.go) also uses, so root and children share one sink.
	segmentSink agent.SegmentSink
	// childApprove is the per-child base approval func (the TUI channel); rootApprove
	// is the build-seam approval used by BuildAgent for the root construction. In the
	// wiring-assertion test both may be nil.
	childApprove permissions.ApprovalFunc
	rootApprove  permissions.ApprovalFunc
}

// buildShellRuntimeDeps assembles runtime.Deps for the interactive shell. It wires
// the child-builder closure (cloned child-builder site 2 of 3), registers the root's
// spawn_agent / blackboard / pipeline tools on toolReg (side effect the TUI relies
// on), and seeds the per-loop store holder from the shell's OWN event store (the shell
// owns its store lifecycle because the TUI — not StartLoop — drives turns, change
// 0046). BuildAgent builds a root agent with a discarding default renderer — the TUI's
// own AgentBuilder seam
// (which owns the per-turn renderer) is what actually drives shell turns; BuildAgent
// exists for seam symmetry and binding-#2 reuse.
func buildShellRuntimeDeps(in shellDepsInput) runtime.Deps {
	cfg, reg, alias := in.cfg, in.reg, in.alias
	toolReg, tree := in.toolReg, in.tree
	verbose, skillBlock := in.verbose, in.skillBlock
	sessionMode, humanBus, handleReg := in.sessionMode, in.humanBus, in.handleReg
	sessLog, traceW, rateGate := in.sessLog, in.traceW, in.rateGate

	sched := tree.Scheduler()
	rootNode := tree.Node(tree.RootID())
	// The session blackboard is supplied by the shell so the TUI's /agents overlay
	// and the tools wired here share ONE instance (change 0023). In the wiring test
	// it may be nil, so fall back to a fresh one.
	bb := in.bb
	if bb == nil {
		bb = agent.NewBlackboard(tree)
	}
	// Per-loop store holder (change 0046): the shell owns its store lifecycle (the TUI
	// drives turns, not StartLoop), so the holder is seeded directly from the shell's
	// own store here rather than by BuildAgent. The child-builder/spawner closures read
	// it instead of the retired currentEventStore() global.
	storeHolder := &eventStoreHolder{}
	storeHolder.set(in.eventStore)

	var makeSpawner func(parentNode *agent.AgentNode, parentDepth int) *agent.Spawner
	makeSpawnFunc := func(parentNode *agent.AgentNode, parentDepth int) tools.SpawnFunc {
		return spawnFuncFrom(makeSpawner(parentNode, parentDepth), sched, parentNode)
	}
	childBuilder := func(ctx context.Context, opts agent.SpawnOpts, childNode *agent.AgentNode, childTree *agent.AgentTree) (string, error) {
		// Register a human-typeable @handle for this child (auto-derived from
		// its label, collision-disambiguated) so the human can address it.
		if handleReg != nil {
			handleReg.Register(childNode.ID, opts.Label)
		}
		// Child-specific tool registry (clone or subset); unknown tool
		// names fail the spawn so the model can self-correct.
		childToolReg, terr := childToolRegistry(toolReg, opts.Tools)
		if terr != nil {
			return "", terr
		}
		if childNode.Depth >= agent.MaxDepth || !shouldWireChildSpawn(opts.Tools) {
			// Depth strip (static): a child at MaxDepth can never spawn.
			// Folded-in fix (change 0034): a parent that omits spawn_agent
			// from its requested tools subset withholds it from the child.
			// Either way, drop any copy inherited via Clone()/Subset().
			childToolReg.Unregister("spawn_agent")
		} else {
			// Replace spawn_agent with one wired to the child's spawner.
			childToolReg.Register(tools.NewSpawnAgentToolWithBudget(makeSpawnFunc(childNode, childNode.Depth), sched.SpawnBudget).
				WithQuotaWarning(quotaWarningFor(tree, childNode.ID)))
		}
		// Blackboard tools bound to the child's provenance — always wired
		// (not spawn-gated), honoring an explicit subset that omits them.
		wireChildBlackboard(childToolReg, bb, childNode, opts.Tools)
		// pipeline_run bound to the child's own spawner — always wired unless
		// an explicit subset omits it (mirrors the blackboard wiring). (0026)
		pipeChildWorkers, pipeChildTools := pipelineSynthPalette(cfg, childToolReg)
		childSpawner := makeSpawner(childNode, childNode.Depth)
		wirePipelineTool(childToolReg,
			makePipelineRunFn(childSpawner, bb, sched, childNode),
			makePipelineSynthFn(childSpawner, bb, sched, childNode, cfg, pipeChildWorkers, pipeChildTools, traceW),
			opts.Tools)

		r := tui.NewNodeRenderer(childNode, childTree)
		// Child agents inherit the parent's permission config and route their
		// approvals to the same parent TUI channel, prefixed so the human sees
		// which subagent is asking. The configured mode is enforced for
		// children exactly as for the root (no blanket-auto-approve bypass).
		childApprove := permissions.PrefixApproval(opts.Label, in.childApprove)

		modelAlias := opts.ModelID
		if modelAlias == "" {
			modelAlias = alias
		}

		var a *agent.Agent
		var aerr error
		// Children spawned inside the interactive shell inherit its
		// interactive posture — a human is reachable via the shell.
		if opts.SystemPrompt != "" {
			a, aerr = buildChildAgent(cfg, reg, modelAlias, r, opts.SystemPrompt, childToolReg, childApprove, traceW, opts.Label, sessionMode, true, rateGate, in.segmentSink)
		} else {
			a, aerr = buildAgentWithRendererAndTrace(cfg, reg, modelAlias, r, verbose, skillBlock, childToolReg, childApprove, traceW, opts.Label, sessionMode, true, rateGate, in.segmentSink)
		}
		if aerr != nil {
			return "", aerr
		}
		// Per-turn spawn-strip brake: omit spawn_agent from this child's
		// tool schemas when admission from its scope would not currently be
		// granted or queued within bound (change 0033, unified in change 0036).
		a.SetStripSpawn(sched.StripPredicate(childNode.ID))
		a.SetExpects(opts.ExpectsSchema(), opts.ExpectsSink()) // 0042: structured-delegation return_result channel
		// Child's human-message injector: drains humanq/<child> each turn.
		if humanBus != nil {
			a.SetHumanInjector(agent.NewHumanInjector(childNode.ID, humanBus))
		}
		// Event stream wiring (change 0043/0046): the child emits its loop events
		// into the session store (the shell's own store via the per-loop holder),
		// tagged with its node identity.
		a.SetEventSink(storeHolder.get())
		a.SetNodeIdentity(childNode.ID, childNode.ParentID, childNode.Depth)

		// spawn.start / spawn.done are emitted by the Spawner (change 0044), the
		// single choke point every spawn passes through. The direct sessLog.Write
		// below is independent and unchanged; the event-stream projection consumer
		// (startProjectedLogConsumer) still reproduces the same LogEntry from the
		// Spawner-emitted spawn.done.
		history := []model.Message{{Role: "user", Content: opts.Task}}
		msgs, rerr := a.Run(ctx, history)
		// Report the RAW run error to the Spawner's spawn.done event (change 0044)
		// so the projected session log's `kind` matches this direct write's
		// raw-error selection below — the byte-equivalence 0043 relies on — even
		// when childResult collapses a max-turns/loop stop into a partial-success
		// string (runErr == nil).
		opts.RunErrSink().Set(rerr)

		if sessLog != nil {
			kind := "done"
			if rerr != nil {
				kind = "error"
			}
			_ = sessLog.Write(session.LogEntry{
				TS:       time.Now(),
				NodeID:   childNode.ID,
				ParentID: childNode.ParentID,
				Label:    childNode.Label,
				Depth:    childNode.Depth,
				Kind:     kind,
			})
		}

		return childResult(msgs, rerr)
	}
	makeSpawner = func(parentNode *agent.AgentNode, parentDepth int) *agent.Spawner {
		return agent.NewSpawner(
			agent.WithTree(tree),
			agent.WithNode(parentNode),
			agent.WithSpawnDepth(parentDepth),
			// Completion hook (ADR-0022): bubble a finished child's undelivered
			// human messages up to its parent so none are silently stranded.
			agent.WithHumanBus(humanBus),
			// Spawn lifecycle events (change 0044/0046): emitted by the Spawner choke
			// point onto the shell's own store via the per-loop holder.
			agent.WithEventStore(storeHolder.get()),
			agent.WithChildBuilder(childBuilder),
		)
	}

	// Register spawn_agent in the tool registry before any agent runs.
	toolReg.Register(tools.NewSpawnAgentToolWithBudget(makeSpawnFunc(rootNode, 0), sched.SpawnBudget).
		WithQuotaWarning(quotaWarningFor(tree, rootNode.ID)))
	// Root-node-wired blackboard tools (provenance = rootNode).
	wireRootBlackboard(toolReg, bb, rootNode)
	// Root pipeline_run bound to the root spawner (provenance = rootNode). (0026)
	rootPipeWorkers, rootPipeTools := pipelineSynthPalette(cfg, toolReg)
	rootPipeSpawner := makeSpawner(rootNode, 0)
	wirePipelineTool(toolReg,
		makePipelineRunFn(rootPipeSpawner, bb, sched, rootNode),
		makePipelineSynthFn(rootPipeSpawner, bb, sched, rootNode, cfg, rootPipeWorkers, rootPipeTools, traceW),
		nil)

	return runtime.Deps{
		Tree:            tree,
		BaseDir:         in.logDir, // present for seam symmetry; the shell's TUI drives turns, not StartLoop.
		MaxConcurrent:   cfg.Agents.MaxConcurrent,
		NewToolRegistry: func() *tools.Registry { return toolReg },
		BuildAgent: func(store event.EventStore, _ *agent.AgentTree, modelID string, reg2 *tools.Registry) (*agent.Agent, agent.ChildBuilder, string, error) {
			// The TUI's own AgentBuilder seam builds the per-turn root with the live
			// renderer; BuildAgent here builds a root with a discarding renderer for
			// seam symmetry / binding-#2 reuse. It honors the same wiring the shell's
			// build closure applies (strip predicate, injector, event sink). The shell
			// owns its store lifecycle, so if StartLoop DID drive this loop the Runtime's
			// store is published to the holder; otherwise the holder keeps the shell's
			// own store (seeded above).
			if store != nil {
				storeHolder.set(store)
			}
			a, err := buildAgentWithRendererAndTrace(cfg, reg, alias, tui.NewRenderer(io.Discard, verbose), verbose, skillBlock, reg2, in.rootApprove, traceW, "root", sessionMode, true, rateGate, in.segmentSink)
			if err != nil {
				return nil, nil, "", err
			}
			a.SetStripSpawn(sched.StripPredicate(rootNode.ID))
			if humanBus != nil {
				a.SetHumanInjector(agent.NewHumanInjector(rootNode.ID, humanBus))
			}
			a.SetEventSink(storeHolder.get())
			a.SetNodeIdentity(rootNode.ID, rootNode.ParentID, rootNode.Depth)
			return a, childBuilder, modelID, nil
		},
	}
}
