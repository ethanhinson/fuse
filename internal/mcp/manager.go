package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/tools"
)

const (
	startTimeout   = 3 * time.Second
	discoverTimeout = 5 * time.Second
)

// Manager spawns configured MCP server processes, discovers their tools, and
// registers them into the provided tool registry.
type Manager struct {
	clients []*StdioClient
}

// NewManager starts all configured MCP servers and registers their tools.
// Servers that fail to start or time out on tools/list are skipped with a
// warning; the session continues with the remaining servers.
func NewManager(servers []config.MCPServerConfig, reg *tools.Registry) (*Manager, error) {
	m := &Manager{}
	for _, srv := range servers {
		client, discovered, err := startAndDiscover(srv)
		if err != nil {
			log.Printf("[mcp] skipping server %q: %v", srv.Name, err)
			continue
		}
		for _, t := range discovered {
			reg.Register(t)
		}
		m.clients = append(m.clients, client)
	}
	return m, nil
}

// Close terminates all managed MCP server processes.
func (m *Manager) Close() {
	for _, c := range m.clients {
		c.stop()
	}
}

// startAndDiscover spawns one server and returns its discovered MCPTools.
func startAndDiscover(srv config.MCPServerConfig) (*StdioClient, []*MCPTool, error) {
	env := buildEnv(srv.Env)
	ctx, cancel := context.WithTimeout(context.Background(), startTimeout+discoverTimeout)
	defer cancel()

	client, err := newStdioClient(srv.Name, srv.Command, env)
	if err != nil {
		return nil, nil, err
	}

	discCtx, discCancel := context.WithTimeout(ctx, discoverTimeout)
	defer discCancel()

	raw, err := client.call(discCtx, "tools/list", nil)
	if err != nil {
		client.stop()
		return nil, nil, fmt.Errorf("tools/list: %w", err)
	}

	var resp struct {
		Tools []mcpToolDef `json:"tools"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		client.stop()
		return nil, nil, fmt.Errorf("parse tools/list: %w", err)
	}

	out := make([]*MCPTool, 0, len(resp.Tools))
	for _, td := range resp.Tools {
		out = append(out, &MCPTool{
			client:      client,
			serverName:  srv.Name,
			toolName:    td.Name,
			description: td.Description,
			inputSchema: td.InputSchema,
		})
	}
	return client, out, nil
}

// buildEnv merges the current process environment with the per-server overrides,
// performing ${VAR} substitution from the process environment.
func buildEnv(overrides map[string]string) []string {
	base := os.Environ()
	for k, v := range overrides {
		base = append(base, k+"="+expandEnv(v))
	}
	return base
}

// expandEnv replaces ${VAR} references with their values from os.Getenv.
func expandEnv(s string) string {
	return strings.NewReplacer().Replace(os.ExpandEnv(s))
}
