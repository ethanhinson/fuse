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

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/permissions"
	"github.com/ethanhinson/fuse/internal/skills"
	"github.com/ethanhinson/fuse/internal/tools"
)

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
- The child agent receives its own full tool set and runs to completion; its final assistant message is the result.`

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

// buildAgentWithRendererAndTrace is like buildAgentWithRenderer but also
// writes raw API request/response JSON to traceW (when non-nil), attributing
// blocks to traceLabel. The caller owns traceW's lifecycle; share one
// syncWriter across all agents of a session so concurrent blocks stay whole.
func buildAgentWithRendererAndTrace(cfg config.Config, reg *model.Registry, alias string, r agent.Renderer, verbose bool, extra string, toolReg *tools.Registry, approve permissions.ApprovalFunc, traceW io.Writer, traceLabel string, sm *permissions.SessionMode, interactive bool) (*agent.Agent, error) {
	if alias == "" {
		alias = reg.Default
	}
	_ = verbose
	a, _, err := buildAgentCore(cfg, reg, alias, r, extra, traceW, traceLabel, toolReg, approve, sm, interactive)
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
func buildChildAgent(cfg config.Config, reg *model.Registry, alias string, r agent.Renderer, systemPrompt string, toolReg *tools.Registry, approve permissions.ApprovalFunc, traceW io.Writer, traceLabel string, sm *permissions.SessionMode, interactive bool) (*agent.Agent, error) {
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
		gate := buildGate(cfg, toolReg, approve, reg, traceW, sm)
		return agent.New(newCLIAdapter(fuseExe, approve), gate, r, mc.ID, systemPrompt, maxTurns, 0), nil
	}
	adapter := model.NewAdapter(cfg.Gateway.URL, cfg.Gateway.Key, nil)
	if traceW != nil {
		adapter = adapter.WithTraceLabel(traceW, traceLabel)
	}
	gate := buildGate(cfg, toolReg, approve, reg, traceW, sm)
	maxTokens := mc.MaxTokens
	if maxTokens == 0 {
		maxTokens = cfg.MaxTokens
	}
	a := agent.New(adapter, gate, r, mc.ID, systemPrompt, maxTurns, maxTokens)
	a.ContextWindow = mc.ContextWindow
	a.LoopApproval = loopApprovalFor(approve, interactive)
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

// buildAgentCore resolves alias and constructs an Agent bound to renderer r,
// returning the resolved gateway model id.
func buildAgentCore(cfg config.Config, reg *model.Registry, alias string, r agent.Renderer, extra string, traceW io.Writer, traceLabel string, toolReg *tools.Registry, approve permissions.ApprovalFunc, sm *permissions.SessionMode, interactive bool) (*agent.Agent, string, error) {
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
		gate := buildGate(cfg, toolReg, approve, reg, traceW, sm)
		systemPrompt := agent.ComposeSystemPrompt(mc.Persona, mc.SystemPrefix, extra)
		// maxTokens is not forwarded to the CLI; Claude controls its own limits.
		a := agent.New(cliAdapter, gate, r, mc.ID, systemPrompt, maxTurns, 0)
		return a, mc.ID, nil
	}

	adapter := model.NewAdapter(cfg.Gateway.URL, cfg.Gateway.Key, nil)
	if traceW != nil {
		adapter = adapter.WithTraceLabel(traceW, traceLabel)
	}
	gate := buildGate(cfg, toolReg, approve, reg, traceW, sm)
	maxTokens := mc.MaxTokens
	if maxTokens == 0 {
		maxTokens = cfg.MaxTokens
	}
	systemPrompt := agent.ComposeSystemPrompt(mc.Persona, mc.SystemPrefix, extra)
	a := agent.New(adapter, gate, r, mc.ID, systemPrompt, maxTurns, maxTokens)
	a.ContextWindow = mc.ContextWindow
	a.LoopApproval = loopApprovalFor(approve, interactive)
	return a, mc.ID, nil
}
