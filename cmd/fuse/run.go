package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/configcheck"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/observe"
	"github.com/ethanhinson/fuse/internal/permissions"
	"github.com/ethanhinson/fuse/internal/ratelimit"
	"github.com/ethanhinson/fuse/internal/skills"
	"github.com/ethanhinson/fuse/internal/tools"
	"github.com/ethanhinson/fuse/internal/tools/sandbox"
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

// spawnRoleHint is the one sentence added after spawnAgentBlock when roles are
// defined. It is a separate string so spawnAgentBlock stays the exact constant
// withoutUnavailableSpawnBlock matches. Roles themselves are listed only in the
// spawn_agent schema, never a second time here.
const spawnRoleHint = "\n\nPrefer a named role when one fits the task."

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
- By default (no ` + "`tools`" + ` argument) the child inherits your FULL tool set and runs to completion; its final assistant message is the result.`

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

// loopApprovalFor adapts an Approver into the agent's doom-loop
// force-through callback, but only in the interactive posture — a non-TTY run
// has no human to answer, so a nil hook keeps the loop's abort-on-trip
// behavior. The synthetic ApprovalRequest carries the "possible loop" preview
// through the same channel a tool-call prompt uses. See change 0038.
func loopApprovalFor(approve permissions.Approver, interactive bool) func(context.Context, string) (bool, error) {
	if !interactive || approve.Answer == nil {
		return nil
	}
	return func(ctx context.Context, preview string) (bool, error) {
		// ToolName tags this as a loop check (not a real tool call) so the TUI
		// labels the prompt and drops the meaningless session option; the
		// session bool is discarded — "always for this loop" makes no sense.
		approved, _, err := approve.Answer(ctx, permissions.ApprovalRequest{
			ToolName: permissions.LoopApprovalToolName,
			Preview:  preview,
		})
		return approved, err
	}
}

// registryFromConfig builds a model.Registry, starting from the built-in
// default and overlaying any config-defined entries. The rule lives in
// internal/configcheck (change 0084) so every validator resolves
// aliases exactly as the CLI does.
func registryFromConfig(cfg config.Config) *model.Registry {
	return configcheck.Registry(cfg)
}

// validateModelRefs checks that every model alias referenced in cfg resolves in
// reg, returning the first dangling reference as the startup error. The rules
// live in configcheck.ModelRefProblems (change 0084); see there for which
// references are checked.
func validateModelRefs(cfg config.Config, reg *model.Registry) error {
	if ps := configcheck.ModelRefProblems(cfg, reg); len(ps) > 0 {
		return ps[0].Err
	}
	return nil
}

// defaultToolRegistry builds the full built-in tool registry. research supplies
// the web_search/web_fetch backends (provider resolution is lazy, so a missing
// key only surfaces when those tools are actually called). skillLookup is
// optional — when non-nil, a skill tool is added to the registry.
//
// sb is the process's sandbox substrate selection (ADR-0044, change 0063),
// resolved ONCE by newSandboxService at the entry point: the bash tool runs
// every command through it. It is threaded explicitly, as a value, rather than
// resolved here — this function is called on config reloads and per loop, and
// re-resolving containment on any of those paths is exactly the fail-open this
// change exists to prevent. A nil sb yields a bash tool that REFUSES; it never
// means "run on the host with an inherited environment".
func defaultToolRegistry(sb *sandbox.Service, research config.ResearchConfig, skillLookup func(string) (skills.Skill, bool)) *tools.Registry {
	r := tools.NewRegistry()
	for _, t := range tools.DefaultTools(sb) {
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
// servers. Used by runShell where MCPProvider owns the server lifecycle. sb is
// the entry point's once-resolved sandbox substrate (see defaultToolRegistry).
func buildSessionRegistryNoMCP(sb *sandbox.Service, cfg config.Config, skillLookup func(string) (skills.Skill, bool)) (*tools.Registry, error) {
	return defaultToolRegistry(sb, cfg.Research, skillLookup), nil
}

// gatewayAdapter builds the LiteLLM gateway adapter for a session, decorating it
// with the session rate gate (change 0036) when one is configured. gate is the
// single shared *ratelimit.Bucket for the whole session — every agent (and the
// auto-mode classifier) shares one budget, so N agents in tight loops cannot
// collectively outrun the configured rpm/tpm. A nil gate is the unlimited fast
// path: WithRateGate(nil) leaves the adapter's gate nil, adding zero latency.
func gatewayAdapter(cfg config.Config, gate model.RateGate) *model.Adapter {
	a := model.NewAdapter(cfg.Gateway.URL, cfg.Gateway.Key, nil)
	// gateway.request_timeout (validated by the loader) replaces the adapter's
	// built-in per-attempt deadline; empty keeps the default.
	if d, err := time.ParseDuration(cfg.Gateway.RequestTimeout); err == nil && d > 0 {
		a.RequestTimeout = d
	}
	if cfg.Gateway.MaxAttempts > 0 {
		a.MaxAttempts = cfg.Gateway.MaxAttempts
	}
	if d, err := time.ParseDuration(cfg.Gateway.RetryBackoff); err == nil {
		a.RetryBackoff = d
	}
	if gate != nil {
		a = a.WithRateGate(gate)
	}
	return a
}

// installSummarizer wires Tier 2 anchored summarization (change 0027) onto a
// after agent.New. When cfg.Context.Summarization.Enabled it builds a bounded
// adapter (reusing the session rate gate) decorated with a distinct "summarizer"
// trace label — the bound-every-model-call learning — resolves the summarizer
// model (empty ⇒ the session's main model id, D4), and installs it with segSink.
// Disabled ⇒ no-op, so the agent stays byte-identical to the pre-0027 Tier-1 path.
//
// segSink is the loop's own SegmentSink (change 0046 — threaded as a value instead
// of the retired process-global currentSegmentSink()); nil ⇒ the agent's no-op
// default (persists nothing, omits the recovery pointer). The interactive shell
// installs a real sink; one-shot and probe pass nil.
func installSummarizer(a *agent.Agent, cfg config.Config, mainModelID string, traceW io.Writer, gate model.RateGate, segSink agent.SegmentSink) {
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
	// #0030: hand the loop's concrete SegmentSink (keyed by the root AgentNode.ID)
	// so a compaction persists its raw region and the injected summary carries a
	// recovery pointer. Nil ⇒ the agent's no-op default.
	a.EnableSummarization(adapter, modelID, s.MaxOutput, segSink)
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
	if adapter.RequestTimeout == 0 {
		adapter.RequestTimeout = model.HelperRequestTimeout // optional scoring, with a heuristic to fall back on
	}
	if traceW != nil {
		adapter = adapter.WithTraceLabel(traceW, "relevance-classifier")
	}
	a.EnableRelevanceClassifier(adapter, rc.ClassifierModel, rc.ClassifierBatchSize, rc.BorderlineLo, rc.BorderlineHi)
}

// applyRunGuards installs the configured limits on a run that spends without
// producing anything.
func applyRunGuards(a *agent.Agent, cfg config.Config) {
	a.SetNoProgressCalls(cfg.NoProgressCalls)
	a.SetMaxRunTokens(cfg.MaxRunTokens)
	a.SetFinishCheck(cfg.FinishCheck)
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
// instructions is forwarded to buildAgentCore unchanged: root builds pass
// cfg.Instructions, child builds pass "".
func buildAgentWithRendererAndTrace(cfg config.Config, reg *model.Registry, alias string, r agent.Renderer, verbose bool, extra, instructions string, toolReg *tools.Registry, approve permissions.Approver, traceW io.Writer, traceLabel string, sm *permissions.SessionMode, interactive bool, gate model.RateGate, segSink agent.SegmentSink, observer observe.Observer) (*agent.Agent, error) {
	if alias == "" {
		alias = reg.Default
	}
	_ = verbose
	a, _, err := buildAgentCore(cfg, reg, alias, r, extra, instructions, traceW, traceLabel, toolReg, approve, sm, interactive, gate, segSink, observer)
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
	// A tool call is blocked on this verdict, so it gets a limit on total time.
	clsAdapter.RequestTimeout = model.HelperRequestTimeout
	if traceW != nil {
		clsAdapter = clsAdapter.WithTraceLabel(traceW, permissions.ClassifierTraceLabel)
	}
	// No user-facing warning writer is cleanly in scope at these builders; the
	// constructor is nil-safe, so pass nil rather than plumb a new parameter
	// through several signatures just for the one-time startup fallback warning.
	cls := permissions.NewClassifier(clsAdapter, reg, cfg.Permissions.Auto, nil)
	// Give the classifier the session's writable geography so its pending-call
	// prompt can distinguish "inside the workspace" from "outside" (#0069).
	// These are deliberately the SAME inputs the gate is scoped with —
	// WithWorkspaceRoot(workspaceRoot()) and WithWriteRoots(
	// gateWriteRoots(cfg)) in buildGate — which together form the gate's
	// allowedRoots(). Naming only workspace+scratch here would tell the
	// classifier a narrower geography than the gate enforces, so a configured
	// permissions.auto.write_roots entry would look out-of-bounds to the model
	// the moment a path arg escapes pathArgs' recognition.
	cls = cls.WithWorkspaceContext(workspaceRoot(), gateWriteRoots(cfg))
	return []permissions.Option{
		permissions.WithClassifier(cls),
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
func buildGate(cfg config.Config, toolReg *tools.Registry, approve permissions.Approver, reg *model.Registry, traceW io.Writer, sm *permissions.SessionMode) *permissions.PermissionGate {
	opts := autoModeOptions(cfg, reg, traceW)
	opts = append(opts, permissions.WithMode(sessionGateMode(cfg, sm)))
	// Extra write roots (change 0068): the per-session scratch dir + trusted
	// config write_roots. Applied unconditionally (outside autoModeOptions) so
	// degraded auto mode — no constructible classifier — still scopes them.
	opts = append(opts, permissions.WithWriteRoots(gateWriteRoots(cfg)))
	// The workspace root is likewise unconditional: it drives the rules-layer
	// floor (rm -rf <ancestor>), which must hold with no gateway/classifier.
	opts = append(opts, permissions.WithWorkspaceRoot(workspaceRoot()))
	if sm != nil {
		opts = append(opts, permissions.WithSessionMode(sm))
	}
	return permissions.New(cfg.Permissions, toolReg, approve, opts...)
}

// buildChildAgent builds an agent with an explicitly provided system prompt,
// bypassing persona composition. Used when spawn_agent sets system_prompt.
func buildChildAgent(cfg config.Config, reg *model.Registry, alias string, r agent.Renderer, systemPrompt string, toolReg *tools.Registry, approve permissions.Approver, traceW io.Writer, traceLabel string, sm *permissions.SessionMode, interactive bool, gate model.RateGate, segSink agent.SegmentSink) (*agent.Agent, error) {
	if alias == "" {
		alias = reg.Default
	}
	mc, err := reg.Resolve(alias)
	if err != nil {
		return nil, fmt.Errorf("model %q: %w", alias, err)
	}
	if err := errToolLessCLIModel(cfg, mc.ID); err != nil {
		return nil, err
	}

	// Wire a per-agent file tracker so write_file refuses to overwrite files
	// this agent has not read, and edit_file detects staleness.
	toolReg.WithFileTracker(tools.NewFileTracker())

	maxTurns := resolveMaxTurns(cfg.MaxTurns, interactive)
	if strings.HasPrefix(mc.ID, "cli/") {
		fuseExe, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("cli adapter: resolve binary: %w", err)
		}
		permGate := buildGate(cfg, toolReg, approve, reg, traceW, sm)
		// Same expression buildGate seeds the gate with, read once per build, so a
		// live /mode off reaches the subprocess at the next turn as it reaches the gate.
		cliAdapter := newCLIAdapter(fuseExe, approve, sessionGateMode(cfg, sm) == permissions.ModeOff)
		return agent.New(cliAdapter, permGate, r, mc.ID, systemPrompt, maxTurns, 0), nil
	}
	adapter := gatewayAdapter(cfg, gate)
	if traceW != nil {
		adapter = adapter.WithTraceLabel(traceW, traceLabel)
	}
	permGate := buildGate(cfg, toolReg, approve, reg, traceW, sm)
	maxTokens := effectiveMaxTokens(mc, cfg)
	a := agent.New(adapter, permGate, r, mc.ID, systemPrompt, maxTurns, maxTokens)
	a.SetOutputCeiling(mc.MaxTokensCeiling)
	a.SetMaxTurnsExplicit(cfg.MaxTurns != nil)
	a.ContextWindow = mc.ContextWindow
	a.ReasoningEffort = mc.ReasoningEffort
	a.LoopApproval = loopApprovalFor(approve, interactive)
	installSummarizer(a, cfg, mc.ID, traceW, gate, segSink)
	installRelevance(a, cfg, traceW, gate)
	applyToolTimeout(a, cfg)
	applyRunGuards(a, cfg)
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
// Budget exhaustion (max turns / loop detection / a valve-paused auto mode)
// returns the partial transcript with a stop-reason marker instead of
// discarding all the work the child completed before running out of budget.
func childResult(msgs []model.Message, rerr error) (string, error) {
	if rerr == nil {
		return lastAssistantText(msgs), nil
	}
	if errors.Is(rerr, agent.ErrMaxTurns) || errors.Is(rerr, agent.ErrLoopDetected) || errors.Is(rerr, agent.ErrAutoModePaused) {
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
// from a child by passing a subset that omits it.
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

// cmdSpawnHandle adapts an agent.AgentHandle to the tools.SpawnHandle seam (change
// 0044): it holds the handle plus the slot-yield context, and performs the
// parent's scheduler-slot yield/unyield AROUND the actual block in WaitResult() —
// so the parent yields its slot only while genuinely blocked on the child (a
// parent holding a slot while its child queues is a deadlock, learning
// slot-cap-yield-while-blocked-on-children). The tool (or any Go caller) awaits the
// handle by calling WaitResult(); the async agent.AgentHandle never crosses into
// internal/tools, keeping that package free of an internal/agent import.
type cmdSpawnHandle struct {
	ctx        context.Context
	handle     agent.AgentHandle
	sched      *agent.Scheduler
	parentNode *agent.AgentNode
}

// WaitResult yields the parent's slot, blocks on the child, then unyields — the
// exact timing spawnFuncFrom performed inline before 0044, now moved to await
// time. An unyield that cannot re-acquire (context cancelled) surfaces the ctx
// error, matching the prior behavior.
func (h cmdSpawnHandle) WaitResult() tools.SpawnResult {
	h.sched.YieldSlot(h.parentNode)
	done := h.handle.Wait()
	if !h.sched.UnyieldSlot(h.ctx, h.parentNode) {
		return tools.SpawnResult{Err: h.ctx.Err()}
	}
	return tools.SpawnResult{Result: done.Result, Err: done.Err}
}

// spawnFuncFrom adapts a *agent.Spawner into the handle-returning tools.SpawnFunc
// seam (change 0044): it starts the spawn and returns a cmdSpawnHandle the caller
// awaits, rather than pre-awaiting and collapsing the handle to a string. The
// spawner-building is identical at every entry point, so it lives here; each site
// builds its own spawner (with its own child builder) and wraps it through this
// adapter.
func spawnFuncFrom(spawner *agent.Spawner, sched *agent.Scheduler, parentNode *agent.AgentNode) tools.SpawnFunc {
	return func(ctx context.Context, req tools.SpawnRequest) (tools.SpawnHandle, error) {
		opts := agent.SpawnOpts{
			Label:        req.Label,
			Task:         req.Task,
			SystemPrompt: req.SystemPrompt,
			ModelID:      req.Model,
			Tools:        req.Tools,
			Role:         req.Role,
			Expects:      req.Expects,
		}
		handle, herr := spawner.Spawn(ctx, opts)
		if herr != nil {
			return nil, herr
		}
		return cmdSpawnHandle{ctx: ctx, handle: handle, sched: sched, parentNode: parentNode}, nil
	}
}

// effectiveMaxTokens is the output cap a gateway-backed agent runs with: the
// registry entry's max_tokens, else the operator's top-level max_tokens.
func effectiveMaxTokens(mc model.ModelConfig, cfg config.Config) int {
	if mc.MaxTokens != 0 {
		return mc.MaxTokens
	}
	return cfg.MaxTokens
}

// buildAgentCore resolves alias and constructs an Agent bound to renderer r,
// returning the resolved gateway model id. instructions is the harness's task
// brief, composed after the persona prompt and before extra. It is an explicit
// parameter so every call site decides: root builds pass cfg.Instructions,
// child builds pass "" (subagents keep their own prompts).
func buildAgentCore(cfg config.Config, reg *model.Registry, alias string, r agent.Renderer, extra, instructions string, traceW io.Writer, traceLabel string, toolReg *tools.Registry, approve permissions.Approver, sm *permissions.SessionMode, interactive bool, gate model.RateGate, segSink agent.SegmentSink, observer observe.Observer) (*agent.Agent, string, error) {
	mc, err := reg.Resolve(alias)
	if err != nil {
		return nil, "", fmt.Errorf("model %q: %w", alias, err)
	}
	if err := errToolLessCLIModel(cfg, mc.ID); err != nil {
		return nil, "", err
	}

	// Wire a per-agent file tracker so write_file refuses to overwrite files
	// this agent has not read, and edit_file detects staleness.
	toolReg.WithFileTracker(tools.NewFileTracker())

	maxTurns := resolveMaxTurns(cfg.MaxTurns, interactive)
	// buildAgentCore is the single chokepoint both gate construction and
	// prompt composition flow through: drop the spawn_agent instructions for an
	// agent that cannot spawn (see prompt_blocks.go), then advertise the
	// auto-approved scratch directory (change 0068) on every binding's prompt.
	//
	// A tool-less agent (no_tools on the loop, or a tool-less role
	// child built from its tool-less config) is offered nothing. Every block
	// extra can carry (skills listing, spawn guidance, scratch directory)
	// describes a tool, so it is dropped, and the persona is its one-line
	// tool-less form. The caller's instructions are unchanged.
	var systemPrompt string
	if cfg.NoTools {
		systemPrompt = agent.ComposeToolLessSystemPrompt(mc.Persona, mc.SystemPrefix, instructions)
	} else {
		extra = withoutUnavailableSpawnBlock(extra, cfg, toolReg)
		if scratchBlockWanted(cfg) {
			extra = appendScratchBlock(extra)
		}
		systemPrompt = agent.ComposeSystemPrompt(mc.Persona, mc.SystemPrefix, instructions, extra)
	}

	// Models with ID prefix "cli/" bypass the LiteLLM gateway and route through
	// the CLIAdapter, which spawns claude --print with fuse mcp-server attached.
	if strings.HasPrefix(mc.ID, "cli/") {
		fuseExe, err := os.Executable()
		if err != nil {
			return nil, "", fmt.Errorf("cli adapter: resolve binary: %w", err)
		}
		// Same expression buildGate seeds the gate with, read once per build, so a
		// live /mode off reaches the subprocess at the next turn as it reaches the gate.
		cliAdapter := newCLIAdapter(fuseExe, approve, sessionGateMode(cfg, sm) == permissions.ModeOff)
		permGate := buildGate(cfg, toolReg, approve, reg, traceW, sm)
		// maxTokens is not forwarded to the CLI; Claude controls its own limits.
		a := agent.New(cliAdapter, permGate, r, mc.ID, systemPrompt, maxTurns, 0)
		a.SetMaxTurnsExplicit(cfg.MaxTurns != nil)
		// Session observer (change 0061). SetObserver maps nil to NoopObserver, so
		// a caller that does not opt in keeps today's behavior exactly.
		a.SetObserver(observer)
		return a, mc.ID, nil
	}

	adapter := gatewayAdapter(cfg, gate)
	if traceW != nil {
		adapter = adapter.WithTraceLabel(traceW, traceLabel)
	}
	permGate := buildGate(cfg, toolReg, approve, reg, traceW, sm)
	maxTokens := effectiveMaxTokens(mc, cfg)
	a := agent.New(adapter, permGate, r, mc.ID, systemPrompt, maxTurns, maxTokens)
	a.SetOutputCeiling(mc.MaxTokensCeiling)
	a.SetMaxTurnsExplicit(cfg.MaxTurns != nil)
	a.ContextWindow = mc.ContextWindow
	a.ReasoningEffort = mc.ReasoningEffort
	a.LoopApproval = loopApprovalFor(approve, interactive)
	installSummarizer(a, cfg, mc.ID, traceW, gate, segSink)
	installRelevance(a, cfg, traceW, gate)
	applyToolTimeout(a, cfg)
	applyRunGuards(a, cfg)
	// Session observer (change 0061). Installed AFTER installSummarizer /
	// installRelevance so SetObserver can propagate it into the auxiliary
	// model callers too. Nil maps to NoopObserver inside SetObserver.
	a.SetObserver(observer)
	return a, mc.ID, nil
}
