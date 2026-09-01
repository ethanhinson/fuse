package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/loopauth"
	"github.com/ethanhinson/fuse/internal/loopserver"
	"github.com/ethanhinson/fuse/internal/mcp"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/observe"
	"github.com/ethanhinson/fuse/internal/permissions"
	"github.com/ethanhinson/fuse/internal/runtime"
	"github.com/ethanhinson/fuse/internal/session"
	"github.com/ethanhinson/fuse/internal/skills"
	"github.com/ethanhinson/fuse/internal/toolidentity"
	"github.com/ethanhinson/fuse/internal/tools"
	"github.com/ethanhinson/fuse/internal/tools/sandbox"
)

// stdinForLoopServer is the reader loop-server drains for JSON-RPC frames. It is
// os.Stdin in production; a test swaps it for an immediately-EOF pipe to prove the
// dispatch wiring returns exit 0 without a real stdio session.
var stdinForLoopServer io.Reader = os.Stdin

// discardRenderer is binding #2's nop agent.Renderer. The loop-server has NO
// display — it is a headless JSON-RPC loop-control server, so every render call is
// discarded. It lives in cmd/fuse (the composition root), not in internal/runtime:
// the seam stays renderer-free; each binding supplies its own display (or none).
type discardRenderer struct{}

func (discardRenderer) Assistant(string)                {}
func (discardRenderer) ToolCall(string, string)         {}
func (discardRenderer) ToolResult(string, tools.Result) {}
func (discardRenderer) Errorf(string, ...any)           {}
func (discardRenderer) Tokens(int, int)                 {}

// runLoopServer implements the `fuse loop-server` subcommand (binding #2): a
// headless stdio JSON-RPC 2.0 loop-control server backed by the in-process Runtime.
// Its documented policy is AUTO-APPROVE (permissions.AlwaysApprove): there is no
// human on a TTY to gate tool calls. That is a binding CHOICE wired here at the
// composition root, not a property of the policy-free Runtime seam. Unlike the CLI
// bindings it wires a REAL fsstore (BaseDir = session.DefaultLogDir()) so
// loop.observe / loop.attach have durable history to replay on reconnect.
func runLoopServer(_ []string, cfg config.Config, reg *model.Registry, _ io.Writer, stderr io.Writer) int {
	// Auto-approve is THIS binding's policy (documented): headless loop control has
	// no human on a TTY. It is not a property of the Runtime seam.
	approve := permissions.AlwaysApprove
	markPolicyApproval()

	skillSet, serr := skills.LoadWithEmbedded(skills.DefaultDirs())
	if serr != nil {
		fmt.Fprintf(stderr, "skills error: %v\n", serr)
		return 1
	}
	systemBlock := skillSet.SystemPromptBlock() + spawnAgentBlock
	// Sandbox substrate (ADR-0044, change 0063): resolved ONCE at startup.
	// hosted=TRUE — the loop server executes workloads on behalf of REMOTE
	// principals, so the local off-switch file is structurally inert and a bash
	// call with no container runtime refuses rather than falling back to this host.
	sb, closeEgress := newSandboxService(cfg, true, stderr)
	defer closeEgress()
	toolReg := defaultToolRegistry(sb, cfg.Research, skillSet.Lookup)

	// Reuse the one-shot deps wiring but with a REAL event store so observe/attach
	// have durable history. Renderer is a discarding renderer — binding #2 has no
	// display.
	deps := buildLoopServerRuntimeDeps(sb, cfg, reg, reg.Default, toolReg, systemBlock, approve, sessionRateGate(cfg))
	rt := runtime.New(deps)

	srv := loopserver.NewServer(stdinForLoopServer, os.Stdout, rt)
	if err := srv.Serve(context.Background()); err != nil {
		fmt.Fprintf(stderr, "loop-server: %v\n", err)
		return 1
	}
	return 0
}

// buildLoopServerRuntimeDeps assembles runtime.Deps for binding #2 — the ONLY true
// multi-loop binding (one process hosts N concurrent loops keyed by loop_id, change
// 0046). Unlike the single-loop CLI bindings it does NOT pre-build a tree or set
// Deps.Tree: StartLoop creates a FRESH tree per loop, and BuildAgent (the per-loop
// construction factory) builds ALL per-loop wiring — tree scheduler config, the
// blackboard, the child-builder / spawner closures, the root spawn/blackboard/
// pipeline tool registration, and the store binding — against THAT loop's own tree
// and store. So N concurrent loop.start calls never share a tree, a store, a
// scheduler, or a blackboard.
//
// It differs from the CLI bindings in three documented ways: (1) the renderer is a
// discardRenderer (no display), (2) permissions.AlwaysApprove is the auto-approve
// binding policy (no human on a TTY), and (3) BaseDir = session.DefaultLogDir() opens
// a REAL fsstore per loop so observe/attach have durable history.
func buildLoopServerRuntimeDeps(sb *sandbox.Service, cfg config.Config, reg *model.Registry, modelAlias string,
	toolReg *tools.Registry, systemBlock string, rootApprove permissions.ApprovalFunc,
	rateGate model.RateGate) runtime.Deps {
	return buildLoopServerRuntimeDepsWithObserver(sb, cfg, reg, modelAlias, toolReg, systemBlock, rootApprove, rateGate, observe.NoopObserver{})
}

// buildLoopServerRuntimeDepsWithObserver is the production composition variant
// that keeps the configured provider-neutral observer in every child factory.
func buildLoopServerRuntimeDepsWithObserver(sb *sandbox.Service, cfg config.Config, reg *model.Registry, modelAlias string,
	toolReg *tools.Registry, systemBlock string, rootApprove permissions.ApprovalFunc,
	rateGate model.RateGate, observer observe.Observer) runtime.Deps {
	if observer == nil {
		observer = observe.NoopObserver{}
	}

	// Select the SHARED durable backend (change 0047): a process-wide durable event
	// store + loop registry, threaded into Deps as VALUES so the loop-server resolves
	// loops via the durable seam and a FRESH process can reattach to a prior process's
	// loop (cold cross-process reattach). The untagged build always returns the
	// filesystem backend (no pgx import); the pgstore build may return Postgres. On the
	// unlikely selector error, fall back to nil (the legacy per-loop fsstore path via
	// BaseDir still works — just without cross-process reattach).
	durableStore, durableReg, derr := selectDurableBackend(cfg)
	if derr != nil {
		durableStore, durableReg = nil, nil
	}

	// Resolve the shared MCP attach options ONCE (change #59, Task 4): they are derived
	// from trusted config (the identity-propagation egress CredentialSource + the posture
	// log), so they are safe to share across loops — the egress seam mints per-call,
	// per-principal tokens from the ctx principal at Execute time, not from any manager
	// state. The complete-mediation TargetMediator reaches each loop's gate separately via
	// buildGate → buildTargetMediator(cfg) inside buildAgentCore, so we discard it here.
	mcpOpts, _ := mcpAttach(cfg, os.Stderr)

	// Per-loop MCP manager registry (change #59, Task 4): each hosted loop gets its OWN
	// mcp.Manager, constructed in NewToolRegistry and registered into that loop's OWN
	// cloned tool registry — never a shared manager, registry, or credential across loops
	// (guardrail deglobalize-holder-also-per-instance-the-shared-graph). Keyed by the
	// per-loop registry pointer so LoopTeardown closes exactly that loop's manager at loop
	// completion. sync.Map because NewToolRegistry (StartLoop goroutine) and LoopTeardown
	// (run-completion goroutine) touch it from different goroutines for concurrent loops.
	var loopManagers sync.Map // *tools.Registry -> *mcp.Manager

	return runtime.Deps{
		Observer: observer,
		// DurableStore/Registry are the 0047 durable seam (shared, cross-process). When
		// set, StartLoop emits into this shared store per StreamKey and registers the
		// loop's liveness, so observe/attach resolve loops a prior process started. BaseDir
		// remains set as the legacy fallback (used only when DurableStore is nil).
		DurableStore:  durableStore,
		Registry:      durableReg,
		BaseDir:       session.DefaultLogDir(),
		MaxConcurrent: cfg.Agents.MaxConcurrent,
		// LoopContext threads the REAL authenticated loop-initiator principal onto the
		// run context (change #59, Task 3). The Connect edge resolved the principal and
		// carried its tenant + subject on LoopConfig; here — at the composition root, not
		// the policy-free runtime seam (ADR-0030) — we reconstitute the loopauth.Principal
		// and stamp it via toolidentity.WithPrincipal, so MCPTool.Execute mints a per-call
		// delegation token for the REAL user (user in `sub`, fuse in `act`), per tenant.
		// Each loop derives its own context, so two concurrent loops never cross. This is
		// the loop-server analogue of the shell's WithToolPrincipal, adapted to the
		// per-loop, principal-per-loop reality — retiring the DefaultTenant shim here by
		// construction.
		LoopContext: func(ctx context.Context, lc runtime.LoopConfig) context.Context {
			return toolidentity.WithPrincipal(ctx, loopServerPrincipal(lc))
		},
		// LoopTeardown closes THIS loop's own MCP manager at loop completion (change #59,
		// Task 4), so no manager (and its read-pump/notify goroutines) outlives its loop and
		// no two loops ever share one. The registry key is the loop's own registry — the
		// exact one NewToolRegistry attached the manager to.
		LoopTeardown: func(loopReg *tools.Registry) {
			if v, ok := loopManagers.LoadAndDelete(loopReg); ok {
				v.(*mcp.Manager).Close()
			}
			// Release THIS loop's own bash sandbox pool (change 0063): its warm
			// Runners and its reaper goroutine must not outlive the loop that
			// created them. It runs on the completion goroutine AND on the
			// StartLoop early-return path, where nothing else would (learning
			// per-instance-resource-needs-teardown-on-every-early-return).
			_ = tools.ReleaseSandboxes(context.Background(), loopReg)
		},
		// The per-loop tool registry is built fresh per loop from the same source as the
		// server's default, so each loop's root tool wiring binds to its own tree below.
		// It ALSO attaches a per-loop MCP manager (change #59, Task 4): mcp.NewManager
		// discovers the configured servers' tools and registers them into THIS loop's own
		// registry (mcpOpts carry the identity-propagation egress seam), so the loop can
		// list + invoke MCP tools that mint per-principal, per-tenant tokens at Execute.
		// The manager is tracked by the loop's registry so LoopTeardown closes it at loop
		// completion. Two concurrent loops get two independent managers over two registries
		// — no shared MCP state.
		NewToolRegistry: func() *tools.Registry {
			loopReg := cloneServerToolRegistry(toolReg)
			// Registry.Clone shares TOOL POINTERS, so the cloned registry would
			// otherwise carry the server-wide bash tool — and with it one warm
			// pool shared by every hosted loop, which LoopTeardown could not
			// close without tearing down another loop's live container. Rebind a
			// fresh bash over the SAME frozen Service (containment is unchanged;
			// only the pool is per-loop), which Register overwrites by name
			// (learning patch-every-cloned-child-builder).
			loopReg.Register(tools.NewBash(sb))
			// A no-op when cfg.MCPServers is empty (no dial, no goroutines). NewManager
			// returns a usable manager even if some servers fail to start (they are skipped
			// with a warning), so the loop still runs its non-MCP tools.
			if mgr, err := mcp.NewManager(cfg.MCPServers, loopReg, mcpOpts...); err != nil {
				fmt.Fprintf(os.Stderr, "loop-server: mcp manager: %v\n", err)
			} else {
				loopManagers.Store(loopReg, mgr)
			}
			return loopReg
		},
		// BuildAgent is the per-loop factory (change 0046): store + tree are THIS loop's
		// own, so all wiring below is loop-local — no process-global, no cross-loop clobber.
		BuildAgent: func(store event.EventStore, tree *agent.AgentTree, modelID string, loopToolReg *tools.Registry) (*agent.Agent, agent.ChildBuilder, string, error) {
			sched := tree.Scheduler()
			sched.SetMaxSpawns(cfg.Agents.MaxSpawns)
			sched.SetQueueBound(cfg.Agents.QueueBound)
			sched.SetSessionTokens(cfg.Throughput.SessionTokens)
			rootNode := tree.Node(tree.RootID())
			// Rebind THIS loop's bash tool with the sandbox emission hooks (change
			// 0063 T8–T11). NewToolRegistry above already gave the loop its own
			// hook-less bash so a StartLoop failure before this point still tears
			// down a per-loop pool; here — the first place that holds BOTH the
			// loop's frozen Service and the loop's event store — it is replaced by
			// one that emits. Register overwrites by name, and no command can have
			// run yet (the agent does not exist until this factory returns), so no
			// pool has been created and nothing is stranded.
			//
			// `store` is already bound to this loop's StreamKey by the runtime, so
			// appending to it lands on the right (tenant, loop) stream with no key
			// plumbing; the root node id completes the envelope. This is the ONLY
			// production emitter of the four sandbox kinds — without it the whole
			// sandbox projection (fuse_sandbox_* metrics, the fuse-sandbox
			// dashboard, its alert rules) can never observe data.
			//
			// The loop-server is the binding that has a per-loop event store. The
			// one-shot, shell, research-probe, and mcp-server bindings do not, and
			// deliberately pass no hooks rather than emitting into a NoopStore.
			if loopToolReg != nil {
				loopToolReg.Register(tools.NewBash(sb, sandbox.WithPoolHooks(
					tools.SandboxEventHooks(store, tree.RootID()))))
				// The admission gate is PROCESS-scoped (it lives on sb, shared across
				// every loop), while store is per-loop, so its hooks are installed
				// here wherever a per-loop store is available (change 0077). This is
				// the sole production emitter of KindSandboxAdmission — without it the
				// queue/rejected metrics and their dashboard panel never observe data.
				// A process serving many loops attributes an admission to whichever
				// loop's store was installed last; that coarseness is acceptable for a
				// host-wide capacity signal whose tenant rides on the event and whose
				// metric is host-level.
				sb.SetGateHooks(tools.SandboxGateHooks(store, tree.RootID()))
			}
			// One blackboard per loop, shared by every agent in that loop's tree (change 0023).
			bb := agent.NewBlackboard(tree)

			var makeSpawner func(parentNode *agent.AgentNode, depth int) *agent.Spawner
			makeSpawnFunc := func(parentNode *agent.AgentNode, depth int) tools.SpawnFunc {
				return spawnFuncFrom(makeSpawner(parentNode, depth), sched, parentNode)
			}
			// childBuilder is the loop's per-child runner, bound to this loop's tree/store.
			childBuilder := func(ctx context.Context, opts agent.SpawnOpts, childNode *agent.AgentNode, childTree *agent.AgentTree) (string, error) {
				childToolReg, terr := childToolRegistry(loopToolReg, opts.Tools)
				if terr != nil {
					return "", terr
				}
				if childNode.Depth >= agent.MaxDepth || !shouldWireChildSpawn(opts.Tools) {
					childToolReg.Unregister("spawn_agent")
				} else {
					childToolReg.Register(tools.NewSpawnAgentToolWithBudget(makeSpawnFunc(childNode, childNode.Depth), sched.SpawnBudget).
						WithQuotaWarning(quotaWarningFor(tree, childNode.ID)))
				}
				// Blackboard tools bound to the child's provenance — always wired (not
				// spawn-gated), honoring an explicit subset that omits them.
				wireChildBlackboard(childToolReg, bb, childNode, opts.Tools)
				// pipeline_run bound to the child's own spawner — always wired unless an
				// explicit subset omits it (mirrors the blackboard wiring). (0026)
				pipeChildWorkers, pipeChildTools := pipelineSynthPalette(cfg, childToolReg)
				childSpawner := makeSpawner(childNode, childNode.Depth)
				wirePipelineTool(childToolReg,
					makePipelineRunFn(childSpawner, bb, sched, childNode),
					makePipelineSynthFn(childSpawner, bb, sched, childNode, cfg, pipeChildWorkers, pipeChildTools, nil),
					opts.Tools)

				childModelID := opts.ModelID
				if childModelID == "" {
					childModelID = modelAlias
				}
				var a *agent.Agent
				var aerr error
				// loop-server is headless (no TTY, AlwaysApprove) ⇒ subagent approvals
				// auto-approve too. No segment sink installed (nil).
				childApprove := permissions.PrefixApproval(opts.Label, rootApprove)
				if opts.SystemPrompt != "" {
					a, aerr = buildChildAgent(cfg, reg, childModelID, discardRenderer{}, opts.SystemPrompt, childToolReg, childApprove, nil, opts.Label, nil, false, rateGate, nil)
				} else {
					a, _, aerr = buildAgentCore(cfg, reg, childModelID, discardRenderer{}, systemBlock, nil, opts.Label, childToolReg, childApprove, nil, false, rateGate, nil, observer)
				}
				if aerr != nil {
					return "", aerr
				}
				a.SetStripSpawn(sched.StripPredicate(childNode.ID))
				a.SetExpects(opts.ExpectsSchema(), opts.ExpectsSink()) // 0042: structured-delegation return_result channel
				// Event stream wiring (change 0043/0046): the child emits into THIS loop's
				// own store (threaded in as a value, not read from a global).
				a.SetEventSink(store)
				a.SetObserver(observer)
				a.SetNodeIdentity(childNode.ID, childNode.ParentID, childNode.Depth)
				// spawn.start / spawn.done are emitted by the Spawner choke point (change 0044).
				msgs, rerr := a.Run(ctx, []model.Message{{Role: "user", Content: opts.Task}})
				// Report the RAW run error to the Spawner's spawn.done event (change 0044) so
				// its `kind` matches even when childResult collapses a stop into success.
				opts.RunErrSink().Set(rerr)
				return childResult(msgs, rerr)
			}
			makeSpawner = func(parentNode *agent.AgentNode, depth int) *agent.Spawner {
				return agent.NewSpawner(
					agent.WithTree(tree),
					agent.WithNode(parentNode),
					agent.WithSpawnDepth(depth),
					// Spawn lifecycle events (change 0044/0046): emitted by the Spawner choke
					// point onto THIS loop's own store.
					agent.WithEventStore(store),
					agent.WithObserver(observer),
					agent.WithChildBuilder(childBuilder),
				)
			}
			loopToolReg.Register(tools.NewSpawnAgentToolWithBudget(makeSpawnFunc(rootNode, 0), sched.SpawnBudget).
				WithQuotaWarning(quotaWarningFor(tree, rootNode.ID)))
			// Root-node-wired blackboard tools (provenance = rootNode).
			wireRootBlackboard(loopToolReg, bb, rootNode)
			// Root pipeline_run bound to the root spawner (provenance = rootNode). (0026)
			rootPipeWorkers, rootPipeTools := pipelineSynthPalette(cfg, loopToolReg)
			rootPipeSpawner := makeSpawner(rootNode, 0)
			wirePipelineTool(loopToolReg,
				makePipelineRunFn(rootPipeSpawner, bb, sched, rootNode),
				makePipelineSynthFn(rootPipeSpawner, bb, sched, rootNode, cfg, rootPipeWorkers, rootPipeTools, nil),
				nil)

			a, mid, err := buildAgentCore(cfg, reg, modelAlias, discardRenderer{}, systemBlock, nil, "root", loopToolReg, rootApprove, nil, false, rateGate, nil, observer)
			if err != nil {
				return nil, nil, "", err
			}
			a.SetStripSpawn(sched.StripPredicate(rootNode.ID))
			// Event stream wiring (change 0043/0046): the root emits onto THIS loop's store.
			a.SetEventSink(store)
			a.SetNodeIdentity(rootNode.ID, rootNode.ParentID, rootNode.Depth)
			return a, childBuilder, mid, nil
		},
	}
}

// cloneServerToolRegistry produces a per-loop tool registry from the server's base
// registry so each hosted loop's root tool wiring (spawn_agent / blackboard /
// pipeline, bound to that loop's own tree) is isolated from every other loop's — no
// two loops mutate one shared registry (change 0046 multi-loop isolation).
func cloneServerToolRegistry(base *tools.Registry) *tools.Registry {
	return base.Clone()
}

// loopServerPrincipal reconstitutes the authenticated loop-initiator's Principal
// from the per-loop LoopConfig the Connect edge populated (change #59, Task 3). The
// tenant and subject originate from the loop-start root of trust — the bearer token
// the verifier resolved at the Connect edge — NEVER from model output.
//
// Fallback: when no authenticated subject is present (an un-intercepted or
// registry-less path — the pure-transport tests, or a local `fuse loop-server` over
// stdio with no auth), the subject is empty. Rather than mint under the zero,
// spoofable Principal (which would fail closed for an OAuth target), stamp a single
// explicit local subject scoped to the loop's tenant — mirroring localPrincipal on
// the CLI paths. The tenant is normalized so "" folds to _default.
func loopServerPrincipal(lc runtime.LoopConfig) loopauth.Principal {
	subject := lc.Subject
	if subject == "" {
		subject = "local"
	}
	return loopauth.Principal{Tenant: event.NormalizeTenant(lc.Tenant), Subject: subject}
}
