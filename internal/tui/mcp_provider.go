package tui

import (
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/mcp"
	"github.com/ethanhinson/fuse/internal/tools"
)

const (
	mcpDebounce = 200 * time.Millisecond
)

// MCPProvider watches ~/.fuse/config.yml for changes and incrementally
// reconnects/disconnects MCP servers, exposing their tools as slash entries.
type MCPProvider struct {
	configPath string
	toolReg    *tools.Registry
	mu         sync.RWMutex
	entries    []SlashEntry
	mgr        *mcp.Manager
	servers    []mcp.ServerInfo
	ch         chan struct{}
	watcher    *fsnotify.Watcher
	done       chan struct{}
}

// NewMCPProvider creates the initial Manager from cfg, builds entries, and
// starts watching configPath for changes.
//
// opts are forwarded verbatim to mcp.NewManager — notably mcp.WithCredentialSource
// (change #52), which stamps every discovered/reconnected MCP tool with the
// identity-propagation egress seam so a per-call, audience-bound credential is
// minted from the loop initiator's identity. Passing no opts leaves MCP tools on
// their pre-#52 static-token path (byte-identical behavior).
func NewMCPProvider(configPath string, cfg config.Config, toolReg *tools.Registry, opts ...mcp.ManagerOption) (*MCPProvider, error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	mgr, err := mcp.NewManager(cfg.MCPServers, toolReg, opts...)
	if err != nil {
		w.Close()
		return nil, err
	}

	p := &MCPProvider{
		configPath: configPath,
		toolReg:    toolReg,
		mgr:        mgr,
		ch:         make(chan struct{}, 1),
		watcher:    w,
		done:       make(chan struct{}),
	}
	p.rebuildEntries()

	// Watch the config file (best-effort; missing file is not fatal).
	_ = w.Add(configPath)

	go p.watch()
	return p, nil
}

// Manager returns the underlying MCP manager (e.g. for external status queries).
func (p *MCPProvider) Manager() *mcp.Manager { return p.mgr }

// rebuildEntries refreshes the slash entry list from the current manager.
func (p *MCPProvider) rebuildEntries() {
	servers := p.mgr.Servers()
	entries := make([]SlashEntry, 0, 8)
	for _, srv := range servers {
		srv := srv
		for _, t := range srv.Tools {
			t := t
			entries = append(entries, SlashEntry{
				Command:     fmt.Sprintf("/mcp:%s/%s", srv.Name, t.Name),
				Description: t.Description,
				Kind:        KindMCP,
				Server:      srv.Name,
				expand: func() string {
					return fmt.Sprintf(
						"Use the %s tool from the %s MCP server. %s Arguments: ",
						t.Name, srv.Name, t.Description,
					)
				},
			})
		}
	}
	p.mu.Lock()
	p.servers = servers
	p.entries = entries
	p.mu.Unlock()
}

// applyConfigDiff reads cfg from disk, diffs against current servers, and
// incrementally reconnects/disconnects.
func (p *MCPProvider) applyConfigDiff() {
	cfg, err := config.Load()
	if err != nil {
		log.Printf("[mcp_provider] reload config: %v", err)
		return
	}

	p.mu.RLock()
	current := p.servers
	p.mu.RUnlock()

	// Index current servers by name.
	currentMap := make(map[string]bool, len(current))
	for _, s := range current {
		currentMap[s.Name] = true
	}

	// Index desired servers by name.
	desiredMap := make(map[string]config.MCPServerConfig, len(cfg.MCPServers))
	for _, s := range cfg.MCPServers {
		desiredMap[s.Name] = s
	}

	// Remove servers no longer in config.
	for _, s := range current {
		if _, ok := desiredMap[s.Name]; !ok {
			if err := p.mgr.Stop(s.Name); err != nil {
				log.Printf("[mcp_provider] stop %q: %v", s.Name, err)
			}
		}
	}

	// Add new servers.
	for _, srv := range cfg.MCPServers {
		if !currentMap[srv.Name] {
			if err := p.mgr.Add(srv); err != nil {
				log.Printf("[mcp_provider] add %q: %v", srv.Name, err)
			}
		}
	}

	p.rebuildEntries()
}

func (p *MCPProvider) signal() {
	select {
	case p.ch <- struct{}{}:
	default:
	}
}

func (p *MCPProvider) watch() {
	timer := time.NewTimer(0)
	timer.Stop()
	pending := false

	for {
		select {
		case ev, ok := <-p.watcher.Events:
			if !ok {
				return
			}
			_ = ev
			if !pending {
				timer.Reset(mcpDebounce)
				pending = true
			}
		case err, ok := <-p.watcher.Errors:
			if !ok {
				return
			}
			log.Printf("[mcp_provider] watcher error: %v", err)
		case <-timer.C:
			pending = false
			p.applyConfigDiff()
			p.signal()
		case <-p.done:
			timer.Stop()
			return
		}
	}
}

func (p *MCPProvider) Commands() []SlashEntry {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]SlashEntry, len(p.entries))
	copy(out, p.entries)
	return out
}

func (p *MCPProvider) Changes() <-chan struct{} { return p.ch }

func (p *MCPProvider) Close() {
	close(p.done)
	p.watcher.Close()
	p.mgr.Close()
}
