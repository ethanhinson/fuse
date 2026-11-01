package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
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
// human on a TTY to gate tool calls. AlwaysApprove is a policy approver, so its
// gates are policy provenance and deny explicit asks at the rules layer. That is
// a binding CHOICE wired here at the composition root, not a property of the
// policy-free Runtime seam. Unlike the CLI
// bindings it wires a REAL fsstore (BaseDir = session.DefaultLogDir()) so
// loop.observe / loop.attach have durable history to replay on reconnect.
func runLoopServer(_ []string, cfg config.Config, reg *model.Registry, _ io.Writer, stderr io.Writer) int {
	// Auto-approve is THIS binding's policy (documented): headless loop control has
	// no human on a TTY. It is not a property of the Runtime seam.
	approve := permissions.AlwaysApprove

	skillSet, serr := skills.LoadWithEmbedded(skills.DefaultDirs())
	if serr != nil {
		fmt.Fprintf(stderr, "skills error: %v\n", serr)
		return 1
	}
	systemBlock := rootSystemBlock(cfg, skillSet)
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
	deps, derr := buildLoopServerRuntimeDepsStrict(sb, cfg, reg, reg.Default, toolReg, systemBlock, approve, sessionRateGate(cfg), observe.NoopObserver{}, skillSet)
	if derr != nil {
		fmt.Fprintf(stderr, "loop-server: %v\n", derr)
		return 1
	}
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
// binding policy (no human on a TTY; a policy approver, so its gates are policy
// provenance), and (3) BaseDir = session.DefaultLogDir() opens
// a REAL fsstore per loop so observe/attach have durable history.
func buildLoopServerRuntimeDeps(sb *sandbox.Service, cfg config.Config, reg *model.Registry, modelAlias string,
	toolReg *tools.Registry, systemBlock string, rootApprove permissions.Approver,
	rateGate model.RateGate) runtime.Deps {
	return buildLoopServerRuntimeDepsWithObserver(sb, cfg, reg, modelAlias, toolReg, systemBlock, rootApprove, rateGate, observe.NoopObserver{})
}

// selectDurableBackendFn is the durable-backend selector the composition root
// consults. A package var so a test can make selection FAIL without a database:
// the untagged selector (durable_backend.go) never errors, and the pgstore one
// errors only when a DSN is configured and Postgres cannot be opened.
var selectDurableBackendFn = selectDurableBackend

// buildLoopServerRuntimeDepsWithObserver is the LENIENT composition variant used by
// tests and library callers: on a selector error it falls back to nil store/registry
// (the legacy per-loop fsstore path via BaseDir — no cross-process reattach).
//
// The two SERVER bindings do NOT use it — see buildLoopServerRuntimeDepsStrict. A
// server that silently ran on the filesystem store because Postgres was not up yet
// would report Ready (the nil store is "nothing to probe"), accept loops, and lose
// every one of them on its next restart. Observed live on the #0076 Helm chart:
// the server and the dev Postgres StatefulSet start concurrently, the first pod
// lost the race, and none of its loops ever reached the database.
func buildLoopServerRuntimeDepsWithObserver(sb *sandbox.Service, cfg config.Config, reg *model.Registry, modelAlias string,
	toolReg *tools.Registry, systemBlock string, rootApprove permissions.Approver,
	rateGate model.RateGate, observer observe.Observer) runtime.Deps {
	durableStore, durableReg, derr := selectDurableBackendFn(cfg)
	if derr != nil {
		durableStore, durableReg = nil, nil
	}
	return assembleLoopServerRuntimeDeps(sb, cfg, reg, modelAlias, toolReg, systemBlock, rootApprove, rateGate, observer, durableStore, durableReg, nil)
}

// buildLoopServerRuntimeDepsStrict is the composition variant BOTH server bindings
// use: a durable-backend selector error is returned, never absorbed, so a
// configured-but-unreachable Postgres refuses to start the server instead of
// degrading it to a store that does not survive a restart. Under Kubernetes that
// refusal is a restart until the database is up, which is the correct convergence;
// under Compose `depends_on: service_healthy` means it never fires.
func buildLoopServerRuntimeDepsStrict(sb *sandbox.Service, cfg config.Config, reg *model.Registry, modelAlias string,
	toolReg *tools.Registry, systemBlock string, rootApprove permissions.Approver,
	rateGate model.RateGate, observer observe.Observer, skillSet *skills.Set) (runtime.Deps, error) {
	durableStore, durableReg, derr := selectDurableBackendFn(cfg)
	var fsErr fsBackendError
	if errors.As(derr, &fsErr) {
		return runtime.Deps{}, fmt.Errorf("durable backend: %w; refusing to start without a durable store - check that the directory can be created and written by this user", derr)
	}
	if derr != nil {
		return runtime.Deps{}, fmt.Errorf("durable backend: %w — refusing to start on the filesystem store (loops would not survive a restart); fix the DSN or wait for Postgres", derr)
	}
	return assembleLoopServerRuntimeDeps(sb, cfg, reg, modelAlias, toolReg, systemBlock, rootApprove, rateGate, observer, durableStore, durableReg, skillSet), nil
}

// assembleLoopServerRuntimeDeps is the shared assembly behind both variants: the
// caller has already selected the durable backend (change 0047 — a process-wide
// durable event store + loop registry, threaded into Deps as VALUES so the
// loop-server resolves loops via the durable seam and a FRESH process can reattach
// to a prior process's loop). nil store/registry means the legacy per-loop fsstore
// path via BaseDir.
//
// It also wires Deps.Harness (change 0085), so a client may start a loop from a
// fuse.harness.v1 HarnessSpec: the spec is validated and layered over cfg (the
// operator's trusted config is the floor), and that loop is built by the same
// per-loop factory as every other loop, from its resolved settings instead of the
// server's. skillSet is the set of skills installed on the server, which a spec's
// skill references resolve against (nil: none).
func assembleLoopServerRuntimeDeps(sb *sandbox.Service, cfg config.Config, reg *model.Registry, modelAlias string,
	toolReg *tools.Registry, systemBlock string, rootApprove permissions.Approver,
	rateGate model.RateGate, observer observe.Observer,
	durableStore event.DurableStore, durableReg event.LoopRegistry, skillSet *skills.Set) runtime.Deps {
	if observer == nil {
		observer = observe.NoopObserver{}
	}

	// Resolve the shared MCP attach options ONCE (change #59, Task 4): they are derived
	// from trusted config (the identity-propagation egress CredentialSource + the posture
	// log), so they are safe to share across loops — the egress seam mints per-call,
	// per-principal tokens from the ctx principal at Execute time, not from any manager
	// state. The complete-mediation TargetMediator reaches each loop's gate separately via
	// buildGate → buildTargetMediator(cfg) inside buildAgentCore, so we discard it here.
	// The broker records each delegated mint into mintAudit, which appends a
	// tool.identity.minted event to the stream of the loop the token was minted for.
	mintAudit := newLoopMintAudit()
	mcpOpts, _ := mcpAttachAudited(cfg, os.Stderr, mintAudit)

	host := &loopServerHost{
		sb:          sb,
		rootApprove: rootApprove,
		observer:    observer,
		skills:      skillSet,
		mintAudit:   mintAudit,
		operator: loopSettings{
			cfg:         cfg,
			reg:         reg,
			modelAlias:  modelAlias,
			toolReg:     toolReg,
			systemBlock: systemBlock,
			rateGate:    rateGate,
			mcpOpts:     mcpOpts,
		},
	}
	server := host.factory(host.operator)

	return runtime.Deps{
		Observer: observer,
		// DurableStore/Registry are the 0047 durable seam (shared, cross-process). When
		// set, StartLoop emits into this shared store per StreamKey and registers the
		// loop's liveness, so observe/attach resolve loops a prior process started. BaseDir
		// remains set as the legacy fallback (used only when DurableStore is nil).
		DurableStore:  durableStore,
		Registry:      durableReg,
		BaseDir:       session.DefaultLogDir(),
		MaxConcurrent: server.MaxConcurrent,
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
		// It also stamps the loop's own id (toolidentity.WithLoop), which the delegation
		// token carries as loop_id, so the downstream service knows which conversation a
		// call comes from, not only whose. The same loop id routes the broker's audit
		// record of each mint to that loop's stream (loopMintAudit).
		LoopContext: func(ctx context.Context, lc runtime.LoopConfig) context.Context {
			ctx = toolidentity.WithPrincipal(ctx, loopServerPrincipal(lc))
			if lc.LoopID != "" {
				ctx = toolidentity.WithLoop(ctx, lc.LoopID)
			}
			return ctx
		},
		// LoopTeardown closes THIS loop's own MCP manager at loop completion (change #59,
		// Task 4), so no manager (and its read-pump/notify goroutines) outlives its loop and
		// no two loops ever share one. The registry key is the loop's own registry — the
		// exact one NewToolRegistry attached the manager to. It serves loops built from a
		// harness spec too: their factory registers into the same map.
		LoopTeardown:    host.teardown,
		NewToolRegistry: server.NewToolRegistry,
		BuildAgent:      server.BuildAgent,
		Harness:         host,
	}
}

// loopSettings is everything one hosted loop's construction is built from. The
// server's own settings come from its trusted config at startup; a loop started
// from a harness spec gets settings resolved from the spec layered over them
// (change 0085), so its agent, subagents, workflows and pools are built from its
// own config through the very same factory.
type loopSettings struct {
	cfg         config.Config
	reg         *model.Registry
	modelAlias  string
	toolReg     *tools.Registry // the base registry each loop clones
	systemBlock string
	rateGate    model.RateGate
	mcpOpts     []mcp.ManagerOption
}

// loopServerHost is the per-server state every hosted loop's construction
// shares: the frozen sandbox substrate, the binding's approval policy, the
// observer, the installed skills, the per-loop MCP manager registry, and the
// server's own loop settings (the operator floor for harness specs).
type loopServerHost struct {
	sb          *sandbox.Service
	rootApprove permissions.Approver
	observer    observe.Observer
	skills      *skills.Set
	operator    loopSettings

	// Per-loop MCP manager registry (change #59, Task 4): each hosted loop gets its OWN
	// mcp.Manager, constructed in NewToolRegistry and registered into that loop's OWN
	// cloned tool registry - never a shared manager, registry, or credential across loops
	// (guardrail deglobalize-holder-also-per-instance-the-shared-graph). Keyed by the
	// per-loop registry pointer so LoopTeardown closes exactly that loop's manager at loop
	// completion. sync.Map because NewToolRegistry (StartLoop goroutine) and LoopTeardown
	// (run-completion goroutine) touch it from different goroutines for concurrent loops.
	loopManagers sync.Map // *tools.Registry -> *mcp.Manager

	// mintAudit is the tool-identity broker's audit sink for every loop this
	// host runs, operator and harness-spec loops alike: each delegated token
	// minted for a loop becomes a tool.identity.minted event in that loop's own
	// stream. BuildAgent binds a loop to its store; teardown unbinds it.
	mintAudit *loopMintAudit
}

// teardown releases what a loop's NewToolRegistry attached to its registry.
func (h *loopServerHost) teardown(loopReg *tools.Registry) {
	if v, ok := h.loopManagers.LoadAndDelete(loopReg); ok {
		v.(*mcp.Manager).Close()
	}
	// Stop routing mint audit records to this loop's store.
	h.mintAudit.unbind(loopReg)
	// Release THIS loop's own bash sandbox pool (change 0063): its warm
	// Runners and its reaper goroutine must not outlive the loop that
	// created them. It runs on the completion goroutine AND on the
	// StartLoop early-return path, where nothing else would (learning
	// per-instance-resource-needs-teardown-on-every-early-return).
	_ = tools.ReleaseSandboxes(context.Background(), loopReg)
}

// factory builds the per-loop construction closures from s.
func (h *loopServerHost) factory(s loopSettings) runtime.LoopFactory {
	sb, cfg, reg, modelAlias, toolReg, systemBlock := h.sb, s.cfg, s.reg, s.modelAlias, s.toolReg, s.systemBlock
	rootApprove, rateGate, observer, mcpOpts := h.rootApprove, s.rateGate, h.observer, s.mcpOpts
	loopManagers := &h.loopManagers
	mintAudit := h.mintAudit
	return runtime.LoopFactory{
		MaxConcurrent: cfg.Agents.MaxConcurrent,
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
				// Route the broker's mint audit records for this loop (keyed by its
				// root id, which is the loop id) to this loop's own store, before any
				// tool can run. teardown unbinds it.
				mintAudit.bind(loopToolReg, tree.RootID(), store)
				emitMCPServerEvents(store, tree.RootID(), loopManagers, loopToolReg)
				loopToolReg.Register(tools.NewBash(sb, sandbox.WithPoolHooks(
					tools.SandboxEventHooks(store, tree.RootID()))))
				// The admission gate (change 0077) and the substrate-health emitter
				// (change 0065) are both PROCESS-scoped seams on sb whose observers
				// are per-loop, so both are installed here — the one place holding
				// the frozen Service AND this loop's store. installSandboxLoopHooks
				// documents why that coarseness is acceptable for both, and it is
				// the sole production caller of SandboxGateHooks and
				// SandboxHealthHooks: without it neither KindSandboxAdmission nor
				// KindSandboxHealth can ever be emitted by a real fuse.
				installSandboxLoopHooks(sb, store, tree.RootID())
			}
			// One blackboard per loop, shared by every agent in that loop's tree (change 0023).
			bb := agent.NewBlackboard(tree)

			// Per-loop workflow scope and shared tool wiring (change 0091): cfg is this
			// loop's resolved config, so spec-supplied workflows activate per spawn here
			// exactly as in the local bindings. traceW is nil (no trace on loop-server).
			scope := newWorkflowScope(tree, cfg.Workflows)
			var makeSpawner func(parentNode *agent.AgentNode, depth int) *agent.Spawner
			wiring := loopWiring{cfg: cfg, tree: tree, bb: bb, scope: scope,
				makeSpawner: func(p *agent.AgentNode, d int) *agent.Spawner { return makeSpawner(p, d) }}
			// childBuilder is the loop's per-child runner, bound to this loop's tree/store
			// (cloned child-builder site 4 of 4, wired through loopWiring; learning
			// patch-every-cloned-child-builder).
			childBuilder := func(ctx context.Context, opts agent.SpawnOpts, childNode *agent.AgentNode, childTree *agent.AgentTree) (string, error) {
				childToolReg, workerModel, terr := wiring.childTools(loopToolReg, opts, childNode)
				if terr != nil {
					return "", terr
				}

				childModelID := opts.ModelID
				if childModelID == "" {
					childModelID = workerModel // worker model pin (change 0034), now every binding
				}
				if childModelID == "" {
					childModelID = modelAlias
				}
				var a *agent.Agent
				var aerr error
				// loop-server is headless (no TTY, AlwaysApprove) ⇒ subagent approvals
				// auto-approve too, and PrefixApproval keeps the policy provenance, so
				// child gates deny explicit asks. No segment sink installed (nil).
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
				a.SetUsageSink(childNode.UpdateTokens) // the agent owns usage accounting (change 0090)
				a.SetCallGate(func() error { return sched.TokenQuotaDenied(childNode.ID) })
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
					// Workflow pool backstop (change 0091), resolved lazily per spawn.
					agent.WithSpawnBackstop(scope.backstop(parentNode)),
					agent.WithChildBuilder(childBuilder),
				)
			}
			// Root spawn_agent, blackboard and pipeline_run (provenance = rootNode).
			wiring.rootTools(loopToolReg, rootNode)

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

// emitMCPServerEvents appends one mcp.server event per MCP server the loop's
// manager holds (change 0089), right after loop.harness. A server that failed to
// connect is reported connected=false with its error; the loop continues without
// it. Payloads never carry a credential. A resumed loop rebuilds its manager and
// re-dials, so it records a fresh report of the servers' current state. Tools are
// the names exposed to the model (mcp:<server>/<tool>).
func emitMCPServerEvents(store event.EventStore, rootID string, managers *sync.Map, reg *tools.Registry) {
	v, ok := managers.Load(reg)
	if !ok {
		return
	}
	mgr, ok := v.(*mcp.Manager)
	if !ok || mgr == nil {
		return
	}
	statuses := mgr.Status()
	sort.Slice(statuses, func(i, j int) bool { return statuses[i].Name < statuses[j].Name })
	for _, st := range statuses {
		toolNames := make([]string, 0, len(st.Tools))
		for _, tn := range st.Tools {
			toolNames = append(toolNames, mcp.QualifiedToolName(st.Name, tn))
		}
		sort.Strings(toolNames)
		payload, err := event.MarshalPayload(event.MCPServerPayload{
			Name: st.Name, Transport: st.Transport, AuthType: st.AuthType,
			Connected: st.Connected, Error: redactMCPError(st.Error), Tools: toolNames,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "loop-server: marshal mcp.server %q: %v\n", st.Name, err)
			continue
		}
		if err := store.Append(event.Event{NodeID: rootID, Kind: event.KindMCPServer, Payload: payload}); err != nil {
			fmt.Fprintf(os.Stderr, "loop-server: record mcp.server %q: %v\n", st.Name, err)
		}
	}
}

var urlInMessage = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.-]*://[^\s"'<>]+`)

// redactMCPError strips URL userinfo and query strings from a connection error
// before it is persisted: net/http errors embed the full request URL, which can
// carry credentials. Every URL found in the message is rebuilt without them.
func redactMCPError(msg string) string {
	return urlInMessage.ReplaceAllStringFunc(msg, func(raw string) string {
		trail := ""
		for len(raw) > 0 && strings.ContainsRune(".,;:)]}", rune(raw[len(raw)-1])) {
			trail = string(raw[len(raw)-1]) + trail
			raw = raw[:len(raw)-1]
		}
		u, err := url.Parse(raw)
		if err != nil {
			if i := strings.IndexAny(raw, "?#"); i >= 0 {
				raw = raw[:i]
			}
			if i := strings.Index(raw, "://"); i >= 0 {
				if at := strings.LastIndex(raw[i+3:], "@"); at >= 0 {
					raw = raw[:i+3] + raw[i+3+at+1:]
				}
			}
			return raw + trail
		}
		u.User, u.RawQuery, u.Fragment, u.ForceQuery = nil, "", "", false
		return u.String() + trail
	})
}
