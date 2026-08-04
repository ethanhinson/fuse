package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/mcp"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/permissions"
	"github.com/ethanhinson/fuse/internal/skills"
	"github.com/ethanhinson/fuse/internal/tools"
	"github.com/ethanhinson/fuse/internal/tui"
)

// registryFromConfig builds a model.Registry, starting from the built-in
// default and overlaying any config-defined entries.
func registryFromConfig(cfg config.Config) *model.Registry {
	reg := model.DefaultRegistry()
	if cfg.Models.Default != "" {
		reg.Default = cfg.Models.Default
	}
	for alias, mc := range cfg.Models.Entries {
		reg = mergeEntry(reg, alias, model.ModelConfig{ID: mc.ID, MaxTokens: mc.MaxTokens, Persona: mc.Persona, SystemPrefix: mc.SystemPrefix})
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

// buildSessionRegistry builds a tool registry for a session, starting MCP
// servers if configured. skillLookup wires the skill tool; pass nil to omit
// it. The caller is responsible for calling mgr.Close().
func buildSessionRegistry(cfg config.Config, skillLookup func(string) (skills.Skill, bool)) (*tools.Registry, *mcp.Manager, error) {
	toolReg := defaultToolRegistry(skillLookup)
	mgr, err := mcp.NewManager(cfg.MCPServers, toolReg)
	if err != nil {
		return nil, nil, err
	}
	return toolReg, mgr, nil
}

// buildSessionRegistryNoMCP builds a tool registry without starting MCP
// servers. Used by runShell where MCPProvider owns the server lifecycle.
func buildSessionRegistryNoMCP(cfg config.Config, skillLookup func(string) (skills.Skill, bool)) (*tools.Registry, error) {
	_ = cfg
	return defaultToolRegistry(skillLookup), nil
}

// buildAgent resolves a model alias and constructs a ready-to-run Agent along
// with the resolved gateway model id. The persona system prompt is always
// prepended; extra is appended (e.g. a skill listing block from shell mode).
// One-shot mode: always uses AlwaysApprove (no interactive user available).
func buildAgent(cfg config.Config, reg *model.Registry, alias string, out io.Writer, verbose bool, extra string, traceW io.Writer) (*agent.Agent, string, error) {
	if alias == "" {
		alias = reg.Default
	}
	// One-shot mode: no skill tool (nil lookup), MCP servers started inline and
	// leaked (process exits immediately after).
	toolReg := defaultToolRegistry(nil)
	_, _ = mcp.NewManager(cfg.MCPServers, toolReg)
	return buildAgentCore(cfg, reg, alias, tui.NewRenderer(out, verbose), extra, traceW, toolReg, permissions.AlwaysApprove)
}

// buildAgentWithRenderer builds an agent that renders through r (used by the
// bubbletea shell, which injects a TeaRenderer).
func buildAgentWithRenderer(cfg config.Config, reg *model.Registry, alias string, r agent.Renderer, verbose bool, extra string, toolReg *tools.Registry, approve permissions.ApprovalFunc) (*agent.Agent, error) {
	if alias == "" {
		alias = reg.Default
	}
	_ = verbose
	a, _, err := buildAgentCore(cfg, reg, alias, r, extra, nil, toolReg, approve)
	return a, err
}

// buildAgentCore resolves alias and constructs an Agent bound to renderer r,
// returning the resolved gateway model id.
func buildAgentCore(cfg config.Config, reg *model.Registry, alias string, r agent.Renderer, extra string, traceW io.Writer, toolReg *tools.Registry, approve permissions.ApprovalFunc) (*agent.Agent, string, error) {
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

	adapter := model.NewAdapter(cfg.Gateway.URL, cfg.Gateway.Key, http.DefaultClient)
	if traceW != nil {
		adapter = adapter.WithTrace(traceW)
	}
	gate := permissions.New(cfg.Permissions, toolReg, approve)
	maxTokens := mc.MaxTokens
	if maxTokens == 0 {
		maxTokens = cfg.MaxTokens
	}
	systemPrompt := agent.ComposeSystemPrompt(mc.Persona, mc.SystemPrefix, extra)
	a := agent.New(adapter, gate, r, mc.ID, systemPrompt, cfg.MaxTurns, maxTokens)
	return a, mc.ID, nil
}
