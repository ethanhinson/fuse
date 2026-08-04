package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/tools"
)

const (
	startTimeout    = 3 * time.Second
	discoverTimeout = 5 * time.Second
)

// MCPToolInfo holds the name and description of a single tool on an MCP server.
type MCPToolInfo struct {
	Name        string
	Description string
}

// ServerInfo describes a connected MCP server (used by MCPProvider to build slash entries).
type ServerInfo struct {
	Name      string
	Transport string
	AuthType  string
	Tools     []MCPToolInfo
}

// ServerStatus is the full status of an MCP server (used by fuse mcps list --live).
type ServerStatus struct {
	Name      string
	Transport string
	AuthType  string
	Connected bool
	Error     string
	Tools     []string
	PID       int      // stdio only
	TokenFile string   // oauth2 only
	LogLines  []string // last N stderr lines, stdio only
}

// managedServer holds runtime state for a single connected server.
type managedServer struct {
	cfg    config.MCPServerConfig
	conn   mcpConn
	tools  []*MCPTool
	connErr string // non-empty if last connect attempt failed
}

// Manager spawns configured MCP server processes, discovers their tools, and
// registers them into the provided tool registry.
type Manager struct {
	mu      sync.Mutex
	servers map[string]*managedServer
	reg     *tools.Registry
}

// NewManager starts all configured MCP servers and registers their tools.
// Servers that fail to start or time out on tools/list are skipped with a
// warning; the session continues with the remaining servers.
func NewManager(servers []config.MCPServerConfig, reg *tools.Registry) (*Manager, error) {
	m := &Manager{
		servers: make(map[string]*managedServer),
		reg:     reg,
	}
	for _, srv := range servers {
		if err := m.Add(srv); err != nil {
			log.Printf("[mcp] skipping server %q: %v", srv.Name, err)
		}
	}
	return m, nil
}

// Add starts a new server, discovers its tools, and registers them. No-op if
// the name is already managed (call Stop first to replace).
func (m *Manager) Add(srv config.MCPServerConfig) error {
	conn, discovered, err := startAndDiscover(srv)
	if err != nil {
		m.mu.Lock()
		m.servers[srv.Name] = &managedServer{cfg: srv, connErr: err.Error()}
		m.mu.Unlock()
		return err
	}
	for _, t := range discovered {
		m.reg.Register(t)
	}
	m.mu.Lock()
	m.servers[srv.Name] = &managedServer{cfg: srv, conn: conn, tools: discovered}
	m.mu.Unlock()
	return nil
}

// Stop terminates the named server's connection and deregisters its tools.
// No-op if the server is not managed.
func (m *Manager) Stop(name string) error {
	m.mu.Lock()
	ms, ok := m.servers[name]
	if !ok {
		m.mu.Unlock()
		return nil
	}
	delete(m.servers, name)
	m.mu.Unlock()

	if ms.conn != nil {
		ms.conn.stop()
	}
	for _, t := range ms.tools {
		m.reg.Unregister(t.Name())
	}
	return nil
}

// Close terminates all managed MCP server processes.
func (m *Manager) Close() {
	m.mu.Lock()
	names := make([]string, 0, len(m.servers))
	for name := range m.servers {
		names = append(names, name)
	}
	m.mu.Unlock()
	for _, name := range names {
		_ = m.Stop(name)
	}
}

// Servers returns a snapshot of ServerInfo for each managed server.
func (m *Manager) Servers() []ServerInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]ServerInfo, 0, len(m.servers))
	for _, ms := range m.servers {
		si := ServerInfo{
			Name:      ms.cfg.Name,
			Transport: ms.cfg.Transport,
			AuthType:  ms.cfg.Auth.Type,
		}
		for _, t := range ms.tools {
			si.Tools = append(si.Tools, MCPToolInfo{
				Name:        t.toolName,
				Description: t.description,
			})
		}
		out = append(out, si)
	}
	return out
}

// Status returns the full status of every managed server.
func (m *Manager) Status() []ServerStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]ServerStatus, 0, len(m.servers))
	for _, ms := range m.servers {
		ss := ServerStatus{
			Name:      ms.cfg.Name,
			Transport: ms.cfg.Transport,
			AuthType:  ms.cfg.Auth.Type,
			Connected: ms.conn != nil && ms.connErr == "",
			Error:     ms.connErr,
		}
		for _, t := range ms.tools {
			ss.Tools = append(ss.Tools, t.toolName)
		}
		if sc, ok := ms.conn.(*StdioClient); ok {
			ss.PID = sc.PID()
			ss.LogLines = sc.LogLines(0)
		}
		if ms.cfg.Auth.Type == "oauth2" {
			ss.TokenFile = ms.cfg.Auth.TokenFile
		}
		out = append(out, ss)
	}
	return out
}

// startAndDiscover spawns one server and returns its discovered MCPTools.
func startAndDiscover(srv config.MCPServerConfig) (mcpConn, []*MCPTool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), startTimeout+discoverTimeout)
	defer cancel()

	var client mcpConn
	var err error

	switch srv.Transport {
	case "http", "sse":
		if srv.URL == "" {
			return nil, nil, fmt.Errorf("mcp server %q: transport %q requires a url", srv.Name, srv.Transport)
		}
		token, authErr := GetAccessToken(srv.Name, srv.URL, srv.Auth)
		if authErr != nil {
			return nil, nil, fmt.Errorf("mcp server %q: auth: %w", srv.Name, authErr)
		}
		client, err = newHTTPClient(srv.Name, srv.URL, token)
		if err != nil {
			return nil, nil, err
		}
	default:
		// "stdio" or "" — existing behavior.
		env := buildEnv(srv.Env)
		client, err = newStdioClient(srv.Name, srv.Command, env)
		if err != nil {
			return nil, nil, err
		}
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
