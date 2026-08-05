package main

import (
	"errors"
	"fmt"
	"io"
	"os"
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

// defaultToolRegistry builds the full built-in tool registry. skillLookup is
// optional — when non-nil, a skill tool is added to the registry.
func defaultToolRegistry(skillLookup func(string) (skills.Skill, bool)) *tools.Registry {
	r := tools.NewRegistry()
	for _, t := range tools.DefaultTools() {
		r.Register(t)
	}
	for _, t := range tools.CodeindexTools() {
		r.Register(t)
	}
	if skillLookup != nil {
		r.Register(tools.NewSkillTool(skillLookup))
	}
	return r
}

// buildSessionRegistryNoMCP builds a tool registry without starting MCP
// servers. Used by runShell where MCPProvider owns the server lifecycle.
func buildSessionRegistryNoMCP(cfg config.Config, skillLookup func(string) (skills.Skill, bool)) (*tools.Registry, error) {
	_ = cfg
	return defaultToolRegistry(skillLookup), nil
}

// buildAgentWithRendererAndTrace is like buildAgentWithRenderer but also
// writes raw API request/response JSON to traceW (when non-nil), attributing
// blocks to traceLabel. The caller owns traceW's lifecycle; share one
// syncWriter across all agents of a session so concurrent blocks stay whole.
func buildAgentWithRendererAndTrace(cfg config.Config, reg *model.Registry, alias string, r agent.Renderer, verbose bool, extra string, toolReg *tools.Registry, approve permissions.ApprovalFunc, traceW io.Writer, traceLabel string) (*agent.Agent, error) {
	if alias == "" {
		alias = reg.Default
	}
	_ = verbose
	a, _, err := buildAgentCore(cfg, reg, alias, r, extra, traceW, traceLabel, toolReg, approve)
	return a, err
}

// buildChildAgent builds an agent with an explicitly provided system prompt,
// bypassing persona composition. Used when spawn_agent sets system_prompt.
func buildChildAgent(cfg config.Config, reg *model.Registry, alias string, r agent.Renderer, systemPrompt string, toolReg *tools.Registry, approve permissions.ApprovalFunc, traceW io.Writer, traceLabel string) (*agent.Agent, error) {
	if alias == "" {
		alias = reg.Default
	}
	mc, err := reg.Resolve(alias)
	if err != nil {
		return nil, fmt.Errorf("model %q: %w", alias, err)
	}
	if strings.HasPrefix(mc.ID, "cli/") {
		fuseExe, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("cli adapter: resolve binary: %w", err)
		}
		gate := permissions.New(cfg.Permissions, toolReg, approve)
		return agent.New(newCLIAdapter(fuseExe, approve), gate, r, mc.ID, systemPrompt, cfg.MaxTurns, 0), nil
	}
	adapter := model.NewAdapter(cfg.Gateway.URL, cfg.Gateway.Key, nil)
	if traceW != nil {
		adapter = adapter.WithTraceLabel(traceW, traceLabel)
	}
	gate := permissions.New(cfg.Permissions, toolReg, approve)
	maxTokens := mc.MaxTokens
	if maxTokens == 0 {
		maxTokens = cfg.MaxTokens
	}
	a := agent.New(adapter, gate, r, mc.ID, systemPrompt, cfg.MaxTurns, maxTokens)
	a.ContextWindow = mc.ContextWindow
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
func buildAgentCore(cfg config.Config, reg *model.Registry, alias string, r agent.Renderer, extra string, traceW io.Writer, traceLabel string, toolReg *tools.Registry, approve permissions.ApprovalFunc) (*agent.Agent, string, error) {
	mc, err := reg.Resolve(alias)
	if err != nil {
		return nil, "", fmt.Errorf("model %q: %w", alias, err)
	}

	// Models with ID prefix "cli/" bypass the LiteLLM gateway and route through
	// the CLIAdapter, which spawns claude --print with fuse mcp-server attached.
	if strings.HasPrefix(mc.ID, "cli/") {
		fuseExe, err := os.Executable()
		if err != nil {
			return nil, "", fmt.Errorf("cli adapter: resolve binary: %w", err)
		}
		cliAdapter := newCLIAdapter(fuseExe, approve)
		gate := permissions.New(cfg.Permissions, toolReg, approve)
		systemPrompt := agent.ComposeSystemPrompt(mc.Persona, mc.SystemPrefix, extra)
		// maxTokens is not forwarded to the CLI; Claude controls its own limits.
		a := agent.New(cliAdapter, gate, r, mc.ID, systemPrompt, cfg.MaxTurns, 0)
		return a, mc.ID, nil
	}

	adapter := model.NewAdapter(cfg.Gateway.URL, cfg.Gateway.Key, nil)
	if traceW != nil {
		adapter = adapter.WithTraceLabel(traceW, traceLabel)
	}
	gate := permissions.New(cfg.Permissions, toolReg, approve)
	maxTokens := mc.MaxTokens
	if maxTokens == 0 {
		maxTokens = cfg.MaxTokens
	}
	systemPrompt := agent.ComposeSystemPrompt(mc.Persona, mc.SystemPrefix, extra)
	a := agent.New(adapter, gate, r, mc.ID, systemPrompt, cfg.MaxTurns, maxTokens)
	a.ContextWindow = mc.ContextWindow
	return a, mc.ID, nil
}
