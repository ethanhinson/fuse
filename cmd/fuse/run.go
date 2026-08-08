package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/permissions"
	"github.com/ethanhinson/fuse/internal/pipeline"
	"github.com/ethanhinson/fuse/internal/ratelimit"
	"github.com/ethanhinson/fuse/internal/skills"
	"github.com/ethanhinson/fuse/internal/tools"
)

// sessionRateBucket builds the one shared rate-gate bucket for a session from
// cfg.Throughput (change 0036), or nil when no rpm/tpm axis is configured (global
// or per-provider) — the unlimited fast path. Returning the concrete
// *ratelimit.Bucket lets the interactive shell hand the same bucket to the
// observability surface (the agents overlay shows its live utilization) while the
// agents consult it via the model.RateGate interface. One bucket is shared by
// every agent in the session so N agents in tight turn loops cannot collectively
// outrun the configured budget; queued turns consume nothing because the gate
// sits at dispatch. Providers are keyed by name and matched against the request's
// model id by leading substring (e.g. "deepseek" ⇒ "deepseek-flash").
func sessionRateBucket(cfg config.Config) *ratelimit.Bucket {
	rc := ratelimit.Config{
		Global: ratelimit.Limits{
			RequestsPerMinute: cfg.Throughput.RequestsPerMinute,
			TokensPerMinute:   cfg.Throughput.TokensPerMinute,
		},
	}
	if len(cfg.Throughput.Providers) > 0 {
		rc.Providers = make(map[string]ratelimit.Limits, len(cfg.Throughput.Providers))
		for name, p := range cfg.Throughput.Providers {
			rc.Providers[name] = ratelimit.Limits{
				RequestsPerMinute: p.RequestsPerMinute,
				TokensPerMinute:   p.TokensPerMinute,
			}
		}
	}
	if !rc.Any() {
		return nil
	}
	return ratelimit.New(rc, nil)
}

// sessionRateGate returns the shared rate gate as a model.RateGate for the agent
// builders. It returns an UNTYPED-nil interface when no axis is configured so the
// adapter's gate stays nil (the fast path with zero latency) — a typed nil
// *ratelimit.Bucket wrapped in the interface would defeat the `gate != nil`
// check, so the bucket is unwrapped to an untyped nil here.
func sessionRateGate(cfg config.Config) model.RateGate {
	b := sessionRateBucket(cfg)
	if b == nil {
		return nil // untyped nil: the adapter's gate stays nil (fast path).
	}
	return b
}

// spawnAgentBlock is the system-prompt block that tells models to use spawn_agent.
// Shared between one-shot and shell mode so behaviour is identical across both.
const spawnAgentBlock = `

## Parallel subagents — use spawn_agent aggressively

You have a ` + "`spawn_agent`" + ` tool. Use it whenever work can be split into independent parts.

### CRITICAL: emit ALL spawn_agent calls in ONE message

When spawning multiple agents, you MUST call spawn_agent N times IN THE SAME RESPONSE.
Do NOT call spawn_agent once, wait for the result, then call it again — that is sequential and wastes time.

WRONG (sequential — do not do this):
  → spawn_agent(label="Read A") … wait … result
  → spawn_agent(label="Read B") … wait … result

CORRECT (parallel — all in one response):
  → spawn_agent(label="Read A")
  → spawn_agent(label="Read B")
  → spawn_agent(label="Read C")
  … all three run concurrently, results arrive together …

### When to spawn
- Reading multiple files, packages, or repos → spawn one agent per source
- Researching N topics → spawn N agents simultaneously
- Any step with ≥2 independent sub-steps → spawn agents for each

### Rules
- Emit ALL spawns as parallel tool calls in one response, then summarise once all results arrive.
- Never read 3+ files one at a time when you can spawn agents.
- By default (no ` + "`tools`" + ` argument) the child inherits your FULL tool set and runs to completion; its final assistant message is the result.

### Shared memory across agents (blackboard)
Children can coordinate through a shared, session-wide blackboard (` + "`blackboard_write` / `blackboard_read` / `blackboard_keys` / `blackboard_wait` / `blackboard_delete`" + `) — the substrate for debate, ensemble, and producer/consumer patterns where children exchange intermediate results.
- To have children collaborate through the blackboard, DO NOT pass a narrow ` + "`tools`" + ` subset that omits it. A non-empty ` + "`tools`" + ` list that leaves out the ` + "`blackboard_*`" + ` tools WITHHOLDS shared memory from the child, so it cannot read or write the board — a common reason a "debate" produces an empty board.
- Either omit ` + "`tools`" + ` entirely (the child inherits the blackboard), or explicitly include the ` + "`blackboard_*`" + ` tools you need in the subset.
- Pattern: instruct each child to WRITE its findings/position to a shared key (e.g. ` + "`review/<name>`" + `), then after they return, READ ` + "`review/*`" + ` and synthesise.`

// headlessTurnBackstop is the generous default cap applied to headless entry
// points (one-shot, non-TTY, mcp-server, research-probe) when max_turns is
// unset — nothing can interrupt a runaway there, so a backstop stays. The
// interactive shell has no such cap (unlimited by default). See change 0038.
const headlessTurnBackstop = 100

// resolveMaxTurns turns the presence-aware config value into the concrete cap
// the agent loop honors. This decision is context-aware and belongs at the
// call site (interactivity is known here, not in the context-free config
// resolve nor the uniform agent.New):
//
//   - unset (nil) + interactive ⇒ 0 (unlimited).
//   - unset (nil) + headless    ⇒ headlessTurnBackstop.
//   - explicit (non-nil)        ⇒ the value verbatim (0 = unlimited, N>0 = cap).
func resolveMaxTurns(cfgMaxTurns *int, interactive bool) int {
	if cfgMaxTurns != nil {
		return *cfgMaxTurns
	}
	if interactive {
		return 0
	}
	return headlessTurnBackstop
}

// oneShotBudgetInteractive resolves the turn/loop budget posture for the
// one-shot entry point, which is NOT the same as the approval-channel posture.
// A human is reachable on a TTY (driving the y/N/a approval prompt), but
// --approve-all is an explicit "don't ask me" scripted posture: with it set,
// the budget must be headless even on a TTY, so an unset max_turns backstops at
// 100 (resolveMaxTurns) and the doom-loop hook stays nil (loopApprovalFor) —
// otherwise a doom loop under --approve-all auto-continues every 3 turns forever
// with no backstop. Explicit max_turns config still wins via resolveMaxTurns.
// See change 0038 (review CONCERN).
func oneShotBudgetInteractive(tty, approveAll bool) bool {
	return tty && !approveAll
}

// loopApprovalFor adapts an ApprovalFunc into the agent's doom-loop
// force-through callback, but only in the interactive posture — a non-TTY run
// has no human to answer, so a nil hook keeps the loop's abort-on-trip
// behavior. The synthetic ApprovalRequest carries the "possible loop" preview
// through the same channel a tool-call prompt uses. See change 0038.
func loopApprovalFor(approve permissions.ApprovalFunc, interactive bool) func(context.Context, string) (bool, error) {
	if !interactive || approve == nil {
		return nil
	}
	return func(ctx context.Context, preview string) (bool, error) {
		// ToolName tags this as a loop check (not a real tool call) so the TUI
		// labels the prompt and drops the meaningless session option; the
		// session bool is discarded — "always for this loop" makes no sense.
		approved, _, err := approve(ctx, permissions.ApprovalRequest{
			ToolName: permissions.LoopApprovalToolName,
			Preview:  preview,
		})
		return approved, err
	}
}

// registryFromConfig builds a model.Registry, starting from the built-in
// default and overlaying any config-defined entries.
func registryFromConfig(cfg config.Config) *model.Registry {
	reg := model.DefaultRegistry()
	if cfg.Models.Default != "" {
		reg.Default = cfg.Models.Default
	}
	for alias, mc := range cfg.Models.Entries {
		reg = mergeEntry(reg, alias, model.ModelConfig{
			ID: mc.ID, MaxTokens: mc.MaxTokens, ContextWindow: mc.ContextWindow,
			Persona: mc.Persona, SystemPrefix: mc.SystemPrefix,
		})
	}
	return reg
}

// mergeEntry returns a registry with alias set to mc, preserving other entries.
func mergeEntry(reg *model.Registry, alias string, mc model.ModelConfig) *model.Registry {
	entries := map[string]model.ModelConfig{}
	for _, name := range reg.Names() {
		v, _ := reg.Resolve(name)
		entries[name] = v
	}
	entries[alias] = mc
	return model.NewRegistry(reg.Default, entries)
}

// defaultToolRegistry builds the full built-in tool registry. research supplies
// the web_search/web_fetch backends (provider resolution is lazy, so a missing
// key only surfaces when those tools are actually called). skillLookup is
// optional — when non-nil, a skill tool is added to the registry.
func defaultToolRegistry(research config.ResearchConfig, skillLookup func(string) (skills.Skill, bool)) *tools.Registry {
	r := tools.NewRegistry()
	for _, t := range tools.DefaultTools() {
		r.Register(t)
	}
	for _, t := range tools.CodeindexTools() {
		r.Register(t)
	}
	r.Register(tools.NewWebSearch(research))
	r.Register(tools.NewWebFetch(research))
	if skillLookup != nil {
		r.Register(tools.NewSkillTool(skillLookup))
	}
	return r
}

// buildSessionRegistryNoMCP builds a tool registry without starting MCP
// servers. Used by runShell where MCPProvider owns the server lifecycle.
func buildSessionRegistryNoMCP(cfg config.Config, skillLookup func(string) (skills.Skill, bool)) (*tools.Registry, error) {
	return defaultToolRegistry(cfg.Research, skillLookup), nil
}

// gatewayAdapter builds the LiteLLM gateway adapter for a session, decorating it
// with the session rate gate (change 0036) when one is configured. gate is the
// single shared *ratelimit.Bucket for the whole session — every agent (and the
// auto-mode classifier) shares one budget, so N agents in tight loops cannot
// collectively outrun the configured rpm/tpm. A nil gate is the unlimited fast
// path: WithRateGate(nil) leaves the adapter's gate nil, adding zero latency.
func gatewayAdapter(cfg config.Config, gate model.RateGate) *model.Adapter {
	a := model.NewAdapter(cfg.Gateway.URL, cfg.Gateway.Key, nil)
	if gate != nil {
		a = a.WithRateGate(gate)
	}
	return a
}

// installSummarizer wires Tier 2 anchored summarization (change 0027) onto a
// after agent.New. When cfg.Context.Summarization.Enabled it builds a bounded
// adapter (reusing the session rate gate) decorated with a distinct "summarizer"
// trace label — the bound-every-model-call learning — resolves the summarizer
// model (empty ⇒ the session's main model id, D4), and installs it with the
// default no-op sink (#0030 provides a real sink later). Disabled ⇒ no-op, so the
// agent stays byte-identical to the pre-0027 Tier-1 path.
func installSummarizer(a *agent.Agent, cfg config.Config, mainModelID string, traceW io.Writer, gate model.RateGate) {
	s := cfg.Context.Summarization
	if !s.Enabled {
		return
	}
	adapter := gatewayAdapter(cfg, gate)
	if traceW != nil {
		adapter = adapter.WithTraceLabel(traceW, "summarizer")
	}
	modelID := s.Model
	if modelID == "" {
		modelID = mainModelID // D4: default summarizer model is the main model
	}
	a.EnableSummarization(adapter, modelID, s.MaxOutput, nil)
}

// installRelevance wires relevance-aware pruning config (change 0028) onto a
// after agent.New. The heuristic scorer is always on (installed by agent.New),
// so this only applies the configured recency floor and, when a classifier
// model is set, the optional hybrid LLM classifier — a bounded adapter (reusing
// the session rate gate) decorated with a distinct "relevance-classifier" trace
// label (the bound-every-model-call learning). With heuristic:false the
// pure-recency scorer is installed (the no-op degeneration path); no classifier
// is wired in that mode.
func installRelevance(a *agent.Agent, cfg config.Config, traceW io.Writer, gate model.RateGate) {
	rc := cfg.Context.Relevance
	a.SetRecencyFloorPct(rc.RecencyFloorPct)
	if !rc.Heuristic {
		a.DisableHeuristicRelevance()
		return
	}
	if rc.ClassifierModel == "" {
		return
	}
	adapter := gatewayAdapter(cfg, gate)
	if traceW != nil {
		adapter = adapter.WithTraceLabel(traceW, "relevance-classifier")
	}
	a.EnableRelevanceClassifier(adapter, rc.ClassifierModel, rc.ClassifierBatchSize, rc.BorderlineLo, rc.BorderlineHi)
}

// applyToolTimeout installs the configured per-leaf-tool-call timeout. A zero
// (unset) config value leaves the agent's built-in default in place; a positive
// value overrides it. Orchestration tools stay exempt inside the agent.
func applyToolTimeout(a *agent.Agent, cfg config.Config) {
	if secs := cfg.Agents.ToolTimeoutSeconds; secs > 0 {
		a.SetToolTimeout(time.Duration(secs) * time.Second)
	}
}

// buildAgentWithRendererAndTrace is like buildAgentWithRenderer but also
// writes raw API request/response JSON to traceW (when non-nil), attributing
// blocks to traceLabel. The caller owns traceW's lifecycle; share one
// syncWriter across all agents of a session so concurrent blocks stay whole.
func buildAgentWithRendererAndTrace(cfg config.Config, reg *model.Registry, alias string, r agent.Renderer, verbose bool, extra string, toolReg *tools.Registry, approve permissions.ApprovalFunc, traceW io.Writer, traceLabel string, sm *permissions.SessionMode, interactive bool, gate model.RateGate) (*agent.Agent, error) {
	if alias == "" {
		alias = reg.Default
	}
	_ = verbose
	a, _, err := buildAgentCore(cfg, reg, alias, r, extra, traceW, traceLabel, toolReg, approve, sm, interactive, gate)
	return a, err
}

// workspaceRoot returns the canonical (symlink-resolved) working directory used
// as the auto-mode path-scoping root. It never panics: an error from os.Getwd
// yields "" (the gate treats an empty root conservatively), and an error from
// filepath.EvalSymlinks falls back to the raw cwd.
func workspaceRoot() string {
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(cwd); err == nil {
		return resolved
	}
	return cwd
}

// classifierConstructible reports whether an auto-mode classifier can be built
// from cfg — i.e. whether the gateway it must route its verdict calls through is
// configured at all. A classifier with no gateway URL would be an erroring stub,
// so when the gateway is entirely unconfigured we keep the classifier nil (the
// gate's fail-closed gray-area ask path) rather than construct a broken one.
//
// D10 item 5: constructibility is NOT gated on the configured mode. As long as
// the gateway is reachable, the classifier is wired regardless of whether the
// startup mode is auto, so a mid-session switch into auto is fully powered.
func classifierConstructible(cfg config.Config) bool {
	return cfg.Gateway.URL != ""
}

// autoModeOptions returns the permission-gate options that wire the auto-mode
// classifier, workspace root, and interactive posture whenever a classifier is
// CONSTRUCTIBLE (the gateway is configured) — regardless of the configured mode
// (D10 item 5), so a mid-session switch into auto gets the full pipeline. When
// no classifier is constructible (gateway entirely unconfigured) it returns nil,
// so permissions.New stays byte-for-byte behaviour-equivalent to the pre-wiring
// three-argument form and auto stays allowed but fail-closed (nil classifier ⇒
// gray area asks).
//
// The classifier gets a DEDICATED gateway adapter built from cfg.Gateway and
// labeled with permissions.ClassifierTraceLabel, so its verdict calls are
// attributable separately from the actor's calls (which carry the actor trace
// label). This is deliberately independent of the actor adapter — including on
// the cli/ paths, where the actor routes through the CLIAdapter but the
// classifier still needs a real gateway adapter for its own verdict calls.
func autoModeOptions(cfg config.Config, reg *model.Registry, traceW io.Writer) []permissions.Option {
	if !classifierConstructible(cfg) {
		return nil
	}
	clsAdapter := model.NewAdapter(cfg.Gateway.URL, cfg.Gateway.Key, nil)
	if traceW != nil {
		clsAdapter = clsAdapter.WithTraceLabel(traceW, permissions.ClassifierTraceLabel)
	}
	// No user-facing warning writer is cleanly in scope at these builders; the
	// constructor is nil-safe, so pass nil rather than plumb a new parameter
	// through several signatures just for the one-time startup fallback warning.
	cls := permissions.NewClassifier(clsAdapter, reg, cfg.Permissions.Auto, nil)
	return []permissions.Option{
		permissions.WithClassifier(cls),
		permissions.WithWorkspaceRoot(workspaceRoot()),
		permissions.WithInteractive(stdinIsTerminal()),
	}
}

// sessionGateMode returns the mode a freshly built gate should start at: the
// current session mode when a session source is threaded in (the interactive
// shell), or the raw cfg.Permissions.Mode when it is nil (one-shot / mcp-server,
// behaviour-identical to before this seam). It is the per-turn-construction half
// of the session-mode surface — read once at construction so the next built gate
// picks up a mid-session switch.
func sessionGateMode(cfg config.Config, sm *permissions.SessionMode) permissions.PermissionMode {
	if sm != nil {
		return sm.Get()
	}
	return permissions.ParseMode(cfg.Permissions.Mode)
}

// buildGate constructs a per-turn/child PermissionGate wired for the session. When
// a session source is threaded in (the interactive shell), the gate is wired to
// the live SessionMode holder so a mid-turn Shift+Tab / /mode switch bites the
// already-built gate and its running children — WithMode still seeds the initial
// snapshot for parity. When sm is nil (one-shot / mcp-server) no holder is wired
// and the gate resolves off the cfg-derived snapshot exactly as before.
// Centralizing construction here keeps the mode override and auto-mode wiring
// consistent across every gate builder and gives tests a single seam to inspect
// the constructed gate.
func buildGate(cfg config.Config, toolReg *tools.Registry, approve permissions.ApprovalFunc, reg *model.Registry, traceW io.Writer, sm *permissions.SessionMode) *permissions.PermissionGate {
	opts := autoModeOptions(cfg, reg, traceW)
	opts = append(opts, permissions.WithMode(sessionGateMode(cfg, sm)))
	if sm != nil {
		opts = append(opts, permissions.WithSessionMode(sm))
	}
	return permissions.New(cfg.Permissions, toolReg, approve, opts...)
}

// buildChildAgent builds an agent with an explicitly provided system prompt,
// bypassing persona composition. Used when spawn_agent sets system_prompt.
func buildChildAgent(cfg config.Config, reg *model.Registry, alias string, r agent.Renderer, systemPrompt string, toolReg *tools.Registry, approve permissions.ApprovalFunc, traceW io.Writer, traceLabel string, sm *permissions.SessionMode, interactive bool, gate model.RateGate) (*agent.Agent, error) {
	if alias == "" {
		alias = reg.Default
	}
	mc, err := reg.Resolve(alias)
	if err != nil {
		return nil, fmt.Errorf("model %q: %w", alias, err)
	}
	maxTurns := resolveMaxTurns(cfg.MaxTurns, interactive)
	if strings.HasPrefix(mc.ID, "cli/") {
		fuseExe, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("cli adapter: resolve binary: %w", err)
		}
		permGate := buildGate(cfg, toolReg, approve, reg, traceW, sm)
		return agent.New(newCLIAdapter(fuseExe, approve), permGate, r, mc.ID, systemPrompt, maxTurns, 0), nil
	}
	adapter := gatewayAdapter(cfg, gate)
	if traceW != nil {
		adapter = adapter.WithTraceLabel(traceW, traceLabel)
	}
	permGate := buildGate(cfg, toolReg, approve, reg, traceW, sm)
	maxTokens := mc.MaxTokens
	if maxTokens == 0 {
		maxTokens = cfg.MaxTokens
	}
	a := agent.New(adapter, permGate, r, mc.ID, systemPrompt, maxTurns, maxTokens)
	a.ContextWindow = mc.ContextWindow
	a.LoopApproval = loopApprovalFor(approve, interactive)
	installSummarizer(a, cfg, mc.ID, traceW, gate)
	installRelevance(a, cfg, traceW, gate)
	applyToolTimeout(a, cfg)
	return a, nil
}

// syncWriter serializes writes from concurrent agents sharing one trace file.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

// openTraceWriter opens traceFile for append and wraps it in a syncWriter.
// Returns nil (and no error surfaced) when traceFile is empty or unopenable —
// tracing is best-effort diagnostics.
func openTraceWriter(traceFile string) (io.Writer, func()) {
	if traceFile == "" {
		return nil, func() {}
	}
	f, err := os.OpenFile(traceFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, func() {}
	}
	return &syncWriter{w: f}, func() { _ = f.Close() }
}

// lastAssistantText returns the content of the last assistant message in msgs.
func lastAssistantText(msgs []model.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "assistant" && msgs[i].Content != "" {
			return msgs[i].Content
		}
	}
	return ""
}

// childResult converts a child run outcome into the spawn_agent result.
// Budget exhaustion (max turns / loop detection) returns the partial
// transcript with a stop-reason marker instead of discarding all the work the
// child completed before running out of budget.
func childResult(msgs []model.Message, rerr error) (string, error) {
	if rerr == nil {
		return lastAssistantText(msgs), nil
	}
	if errors.Is(rerr, agent.ErrMaxTurns) || errors.Is(rerr, agent.ErrLoopDetected) {
		if partial := lastAssistantText(msgs); partial != "" {
			return "[stopped: " + rerr.Error() + " — result may be incomplete]\n\n" + partial, nil
		}
	}
	return "", rerr
}

// childToolRegistry resolves a child's tool registry from its parent's.
// Unknown tool names fail the spawn (so the model can self-correct) instead of
// silently handing the child a near-empty registry it will flail with.
func childToolRegistry(parent *tools.Registry, names []string) (*tools.Registry, error) {
	if len(names) == 0 {
		return parent.Clone(), nil
	}
	sub, unknown := parent.Subset(names)
	if len(unknown) > 0 {
		return nil, fmt.Errorf("unknown tools in spawn request: %s", strings.Join(unknown, ", "))
	}
	return sub, nil
}

// shouldWireChildSpawn reports whether a child built from the parent's requested
// tools subset should receive a (child-wired) spawn_agent tool. An empty request
// means "inherit all" (the child gets everything the parent had, including
// spawn_agent); a non-empty request grants spawn_agent only when it is named.
// This is the change 0034 folded-in fix: a parent can now withhold spawn_agent
// from a child by passing a subset that omits it, and worker allowlists compile
// down to exactly this decision.
func shouldWireChildSpawn(requested []string) bool {
	if len(requested) == 0 {
		return true
	}
	for _, n := range requested {
		if n == "spawn_agent" {
			return true
		}
	}
	return false
}

// wireChildBlackboard registers the blackboard tools on a child's registry with
// provenance bound to childNode (change 0023, Task 4). The tools are ALWAYS
// wired — not spawn-gated — because a child that cannot spawn still shares the
// blackboard. The one exception is an explicit tools subset that omits them: an
// empty requested list is a full clone (force-include, child-bound); a non-empty
// subset includes a blackboard tool only when it names it, and even then the
// version copied from the parent carries the PARENT's provenance, so we
// re-register the child-bound version. Named-but-child-bound and
// unnamed-therefore-absent are both honored by rebuilding from scratch: drop the
// inherited copies, then add back only the ones the subset selects.
func wireChildBlackboard(childToolReg *tools.Registry, bb *agent.Blackboard, childNode *agent.AgentNode, requested []string) {
	childTools := tools.NewBlackboardTools(bb.ForNode(childNode))
	fullClone := len(requested) == 0
	for _, t := range childTools {
		name := t.Name()
		if fullClone || contains(requested, name) {
			// Force-include (clone) or the subset names it: bind to the child.
			childToolReg.Register(t)
		} else {
			// A subset that omits this blackboard tool withholds it from the child.
			childToolReg.Unregister(name)
		}
	}
}

// wireRootBlackboard registers the five blackboard tools on a root tool registry,
// bound to the root node for provenance. It is the root-side counterpart to
// wireChildBlackboard: every agent entry point (main.go run(), shell.go,
// research_probe.go) calls it after building its root registry, so the shared
// call site is the single place root blackboard wiring lives — a dropped call in
// any one entry point is a visible divergence, and the wiring assertion test
// exercises this exact helper (learning patch-every-cloned-child-builder).
func wireRootBlackboard(toolReg *tools.Registry, bb *agent.Blackboard, rootNode *agent.AgentNode) {
	for _, t := range tools.NewBlackboardTools(bb.ForNode(rootNode)) {
		toolReg.Register(t)
	}
}

// pipelinePlanKey is the blackboard key a synthesized pipeline's generated DAG
// is written to before it runs, so a human (or a downstream tool) can inspect
// exactly what was composed (change 0026).
const pipelinePlanKey = "pipeline.plan"

// pipelineWriterID/Label stamp provenance on the pipeline.plan write.
const (
	pipelineWriterID    = "pipeline"
	pipelineWriterLabel = "pipeline"
)

// spawnFuncFrom adapts a *agent.Spawner into the tools.SpawnFunc seam: it spawns
// a child, yields the parent's scheduler slot while blocked on it, and unyields
// on completion (a parent holding a slot while its child queues is a deadlock).
// The spawner-building and slot-yield logic is identical at every entry point, so
// it lives here; each site builds its own spawner (with its own child builder)
// and wraps it through this adapter. Change 0026 extracted it so the same spawner
// can also back the pipeline_run tool.
func spawnFuncFrom(spawner *agent.Spawner, sched *agent.Scheduler, parentNode *agent.AgentNode) tools.SpawnFunc {
	return func(ctx context.Context, req tools.SpawnRequest) (string, error) {
		opts := agent.SpawnOpts{
			Label:        req.Label,
			Task:         req.Task,
			SystemPrompt: req.SystemPrompt,
			ModelID:      req.Model,
			Tools:        req.Tools,
			Worker:       req.Worker,
			Expects:      req.Expects,
		}
		handle, herr := spawner.Spawn(ctx, opts)
		if herr != nil {
			return "", herr
		}
		sched.YieldSlot(parentNode)
		done := handle.Wait()
		if !sched.UnyieldSlot(ctx, parentNode) {
			return "", ctx.Err()
		}
		return done.Result, done.Err
	}
}

// makePipelineRunFn returns the authored-pipeline seam (change 0026): it parses,
// validates (layers 1–2, no synthesis caps), and runs a caller-authored pipeline
// over a spawner built for node, sharing state through bb. It returns a
// human-readable terminal status line; pipeline.status is written by Run.
//
// It yields the CALLING node's scheduler slot around the whole pipeline run,
// mirroring spawnFuncFrom: when pipeline_run is invoked from a depth≥1 child
// (which holds a slot), the pipeline spawns step children from the same pool, so
// a caller that keeps its slot while blocked on those children deadlocks under
// slot pressure (learning: slot-cap-yield-while-blocked-on-children).
func makePipelineRunFn(spawner *agent.Spawner, bb *agent.Blackboard, sched *agent.Scheduler, node *agent.AgentNode) tools.PipelineRunFunc {
	return func(ctx context.Context, definition []byte) (string, error) {
		p, err := pipeline.Parse(definition)
		if err != nil {
			return "", fmt.Errorf("parse: %w", err)
		}
		// Authored path: structural validation only (no synthesis caps).
		if err := pipeline.Validate(p, pipeline.Caps{}); err != nil {
			return "", fmt.Errorf("validate: %w", err)
		}
		sched.YieldSlot(node)
		st, _ := pipeline.Run(ctx, p, spawner, bb)
		if !sched.UnyieldSlot(ctx, node) {
			return "", ctx.Err()
		}
		return pipelineStatusLine(p.Name, st), nil
	}
}

// makePipelineSynthFn returns the synthesis seam (change 0026): it synthesizes a
// pipeline from goal under the config-derived caps, writes the generated DAG to
// pipeline.plan (best-effort trace alongside), then runs it. confirm is advisory
// here — headless there is no interactive confirmer, so a synthesized-and-run
// pipeline proceeds regardless (documented deviation); the flag is plumbed for a
// future gate. workers/toolNames form the synthesis palette.
func makePipelineSynthFn(spawner *agent.Spawner, bb *agent.Blackboard, sched *agent.Scheduler, node *agent.AgentNode, cfg config.Config, workers, toolNames []string, traceW io.Writer) tools.PipelineSynthFunc {
	caps := pipeline.Caps{
		MaxSteps:    cfg.Pipeline.Synthesis.MaxSteps,
		MaxFanout:   cfg.Pipeline.Synthesis.MaxFanout,
		MaxDepth:    cfg.Pipeline.Synthesis.MaxDepth,
		MaxAttempts: cfg.Pipeline.Synthesis.MaxAttempts,
		RequirePool: true,
	}
	return func(ctx context.Context, goal string, confirm bool) (string, error) {
		// Yield the calling node's slot around the whole operation (see
		// makePipelineRunFn): the synthesis child AND the step children spawn from
		// the same pool, so a caller (depth≥1) that holds its slot while blocked on
		// them deadlocks under slot pressure
		// (learning: slot-cap-yield-while-blocked-on-children).
		sched.YieldSlot(node)
		p, err := pipeline.Synthesize(ctx, goal, pipeline.SynthContext{Workers: workers, Tools: toolNames}, spawner, caps)
		if err != nil {
			if !sched.UnyieldSlot(ctx, node) {
				return "", ctx.Err()
			}
			return "", fmt.Errorf("synthesize: %w", err)
		}
		// Publish the generated DAG so it is inspectable before/while it runs.
		if plan, merr := json.Marshal(p); merr == nil {
			var v any
			if json.Unmarshal(plan, &v) == nil {
				bb.Put(pipelinePlanKey, v, pipelineWriterID, pipelineWriterLabel)
			}
			if traceW != nil {
				fmt.Fprintf(traceW, "[pipeline] synthesized plan for goal %q: %s\n", goal, plan)
			}
		}
		// confirm gate is a no-op headless (no interactive confirmer wired): proceed.
		_ = confirm
		st, _ := pipeline.Run(ctx, p, spawner, bb)
		if !sched.UnyieldSlot(ctx, node) {
			return "", ctx.Err()
		}
		return pipelineStatusLine(p.Name, st), nil
	}
}

// pipelineSynthPalette derives the synthesis palette from the resolved config
// and the root tool registry: the worker types declared across all workflows and
// the currently-registered tool names. Both are advisory context the synthesizer
// draws from (change 0026).
func pipelineSynthPalette(cfg config.Config, reg *tools.Registry) (workers, toolNames []string) {
	seen := map[string]bool{}
	for _, wf := range cfg.Workflows {
		for name := range wf.Workers {
			if !seen[name] {
				seen[name] = true
				workers = append(workers, name)
			}
		}
	}
	sort.Strings(workers)
	for _, s := range reg.Schemas() {
		toolNames = append(toolNames, s.Name)
	}
	sort.Strings(toolNames)
	return workers, toolNames
}

// pipelineStatusLine renders a terminal pipeline Status as a single human line.
func pipelineStatusLine(name string, st pipeline.Status) string {
	if st.State == pipeline.StateFailed {
		return fmt.Sprintf("pipeline %s: failed at %s", name, st.FailedStep)
	}
	return fmt.Sprintf("pipeline %s: completed", name)
}

// wirePipelineTool registers the pipeline_run tool on reg, built from the
// authored-run and synthesis seams. Like the blackboard tools it is ALWAYS wired
// unless an explicit tools subset omits it: an empty requested list is a full
// clone (include); a non-empty subset includes it only when it names it. This
// mirrors wireChildBlackboard's include/exclude logic so a narrow child subset
// consistently withholds shared-orchestration tools. Called at both the root and
// every child builder of every agent entry point (change 0026).
func wirePipelineTool(reg *tools.Registry, runFn tools.PipelineRunFunc, synthFn tools.PipelineSynthFunc, requested []string) {
	if len(requested) == 0 || contains(requested, "pipeline_run") {
		reg.Register(tools.NewPipelineRunTool(runFn, synthFn))
	} else {
		reg.Unregister("pipeline_run")
	}
}

// contains reports whether s contains v.
func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// buildAgentCore resolves alias and constructs an Agent bound to renderer r,
// returning the resolved gateway model id.
func buildAgentCore(cfg config.Config, reg *model.Registry, alias string, r agent.Renderer, extra string, traceW io.Writer, traceLabel string, toolReg *tools.Registry, approve permissions.ApprovalFunc, sm *permissions.SessionMode, interactive bool, gate model.RateGate) (*agent.Agent, string, error) {
	mc, err := reg.Resolve(alias)
	if err != nil {
		return nil, "", fmt.Errorf("model %q: %w", alias, err)
	}
	maxTurns := resolveMaxTurns(cfg.MaxTurns, interactive)

	// Models with ID prefix "cli/" bypass the LiteLLM gateway and route through
	// the CLIAdapter, which spawns claude --print with fuse mcp-server attached.
	if strings.HasPrefix(mc.ID, "cli/") {
		fuseExe, err := os.Executable()
		if err != nil {
			return nil, "", fmt.Errorf("cli adapter: resolve binary: %w", err)
		}
		cliAdapter := newCLIAdapter(fuseExe, approve)
		permGate := buildGate(cfg, toolReg, approve, reg, traceW, sm)
		systemPrompt := agent.ComposeSystemPrompt(mc.Persona, mc.SystemPrefix, extra)
		// maxTokens is not forwarded to the CLI; Claude controls its own limits.
		a := agent.New(cliAdapter, permGate, r, mc.ID, systemPrompt, maxTurns, 0)
		return a, mc.ID, nil
	}

	adapter := gatewayAdapter(cfg, gate)
	if traceW != nil {
		adapter = adapter.WithTraceLabel(traceW, traceLabel)
	}
	permGate := buildGate(cfg, toolReg, approve, reg, traceW, sm)
	maxTokens := mc.MaxTokens
	if maxTokens == 0 {
		maxTokens = cfg.MaxTokens
	}
	systemPrompt := agent.ComposeSystemPrompt(mc.Persona, mc.SystemPrefix, extra)
	a := agent.New(adapter, permGate, r, mc.ID, systemPrompt, maxTurns, maxTokens)
	a.ContextWindow = mc.ContextWindow
	a.LoopApproval = loopApprovalFor(approve, interactive)
	installSummarizer(a, cfg, mc.ID, traceW, gate)
	installRelevance(a, cfg, traceW, gate)
	applyToolTimeout(a, cfg)
	return a, mc.ID, nil
}
