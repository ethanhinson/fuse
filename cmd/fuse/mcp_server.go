package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/hitl"
	"github.com/ethanhinson/fuse/internal/mcp"
	"github.com/ethanhinson/fuse/internal/permissions"
	"github.com/ethanhinson/fuse/internal/tools"
	"github.com/ethanhinson/fuse/internal/tools/sandbox"
)

// mcpServerConfigDebounce mirrors the TUI provider's debounce so a burst of
// fsnotify events on a single save coalesces into one registry rebuild.
const mcpServerConfigDebounce = 200 * time.Millisecond

// runMCPServer implements the `fuse mcp-server` subcommand: a stdio MCP server
// that exposes Fuse's built-in tool registry to external AI assistants (Claude,
// Cursor, Codex, etc.) via the MCP JSON-RPC 2.0 protocol.
//
// If FUSE_HITL_SOCKET is set, tool-approval requests are relayed to the parent
// Fuse process (which holds the TUI) via that Unix socket. If unset, all tools
// are auto-approved — safe for use in MCP config without a parent process.
//
// The server also exposes a fuse://tools resource (change 0021): a client can
// resources/read the live tool catalog and resources/subscribe to be pushed a
// notifications/resources/updated whenever a config live-reload changes the tool
// set. A config-watch (parity with the TUI's applyConfigDiff) rebuilds the native
// registry in place and pushes the update to subscribed connections.
//
// Note: MCP servers configured in ~/.fuse/config.yml are NOT spawned here to
// avoid recursive server chains. Only Fuse's native tools are exposed.
func runMCPServer(_ []string, cfg config.Config, _ io.Writer, stderr io.Writer) int {
	// Sandbox substrate (ADR-0044, change 0063): resolved ONCE at startup and
	// threaded into the reload path too, so a config live-reload can never
	// re-resolve containment. hosted=false — `fuse mcp-server` is launched by the
	// operator on their own machine over stdio (the same trust model as the
	// shell), so their file-only off-switch applies.
	sb, closeEgress := newSandboxService(cfg, false, stderr)
	defer closeEgress()
	toolReg := defaultToolRegistry(sb, cfg.Research, nil) // native tools only; no skill tool in MCP server mode

	approve := mcpServerApprover(os.Getenv("FUSE_HITL_SOCKET"), os.Getenv("FUSE_HITL_PROVENANCE"))

	gate := buildMCPServerGate(cfg, toolReg, approve)
	srv := mcp.NewServer(os.Stdin, os.Stdout, toolReg, gate)

	// Live-reload the tool catalog on config change and push fuse://tools updates
	// to subscribed clients (change 0021). Best-effort: a watcher failure leaves
	// the server serving the initial catalog. The watch stops when Serve returns.
	stopWatch := watchMCPToolRegistry(sb, config.Path(), toolReg, srv, stderr)
	defer stopWatch()

	if err := srv.Serve(context.Background()); err != nil {
		fmt.Fprintf(stderr, "mcp-server: %v\n", err)
		return 1
	}
	return 0
}

// mcpServerApprover picks the mcp-server's approver. With no socket it is the
// headless AlwaysApprove policy stand-in. With a socket it dials the parent;
// the parent says who answers on its end via provenance (FUSE_HITL_PROVENANCE):
// unset means an older parent that only relayed to a human, so human; "human"
// is human; anything else (including "policy" and unknown values) is policy,
// the fail-safe, because a policy-labeled approver denies explicit asks at the
// rules layer instead of letting a stand-in approve them.
func mcpServerApprover(socketPath, provenance string) permissions.Approver {
	if socketPath == "" {
		return permissions.AlwaysApprove
	}
	client := hitl.ClientApprovalFunc(socketPath)
	if provenance == "" || provenance == permissions.DecidedByHuman {
		return client
	}
	return permissions.Policy(client.Answer)
}

// buildMCPServerGate constructs the headless mcp-server's permission gate,
// scoped like every other binding's gate (change 0093): the workspace root is
// the server's cwd and the write roots are the session scratch dir plus the
// trusted permissions.auto.write_roots. The root is bound once for the
// process, which is correct: the config-reload path rebuilds tools in place
// and never the gate, and the server's cwd does not move. The binding has no
// model registry, so it gets no classifier (unchanged). runMCPServer and its
// test share this function so the test proves the binding's own construction.
func buildMCPServerGate(cfg config.Config, toolReg *tools.Registry, approve permissions.Approver) *permissions.PermissionGate {
	return permissions.New(cfg.Permissions, toolReg, approve,
		permissions.WithWorkspaceRoot(mcpServerWorkspaceRoot()),
		permissions.WithWriteRoots(gateWriteRoots(cfg)))
}

// mcpServerWorkspaceRoot is workspaceRoot() unless the cwd is "/" or the user's
// home directory. An MCP client often spawns the server with such a cwd, and
// binding it as the workspace would make everything beneath it "in-root" for
// auto-mode heuristics. In that case no root is bound (the catastrophic floor
// still catches "/" and "~" themselves).
func mcpServerWorkspaceRoot() string {
	root := workspaceRoot()
	if root == "" || root == string(filepath.Separator) {
		return ""
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if rh, err := filepath.EvalSymlinks(home); err == nil {
			home = rh
		}
		if filepath.Clean(home) == root {
			return ""
		}
	}
	return root
}

// mcpServerToolSet builds the native tool set the MCP server exposes for a given
// config. Kept as one function so the initial registry (defaultToolRegistry) and
// the reload reconcile draw from the SAME source of truth — a divergence would
// make a reload silently add or drop tools relative to startup.
func mcpServerToolSet(sb *sandbox.Service, cfg config.Config) []tools.Tool {
	return defaultToolRegistry(sb, cfg.Research, nil).Tools()
}

// reconcileMCPToolRegistry diffs the desired tool set against the live registry,
// applies the add/remove delta IN PLACE (so the server's shared *tools.Registry
// pointer — and the permission gate's inner registry, the same pointer — observe
// the change without a swap), and invokes push exactly once when the tool set
// actually changed. An unchanged reconcile mutates nothing and never pushes.
func reconcileMCPToolRegistry(reg *tools.Registry, desired []tools.Tool, push func()) {
	current := map[string]bool{}
	for _, sc := range reg.Schemas() {
		current[sc.Name] = true
	}
	desiredSet := map[string]bool{}
	for _, t := range desired {
		desiredSet[t.Name()] = true
	}

	changed := false
	// Remove tools no longer desired.
	for name := range current {
		if !desiredSet[name] {
			reg.Unregister(name)
			changed = true
		}
	}
	// Add newly desired tools.
	for _, t := range desired {
		if !current[t.Name()] {
			reg.Register(t)
			changed = true
		}
	}
	if changed {
		push()
	}
}

// watchMCPToolRegistry watches configPath and, on a debounced change, reloads the
// config, reconciles the tool registry in place, and pushes a fuse://tools update
// to subscribed connections when the catalog changed. It returns a stop func;
// call it (deferred) when the server exits. A watcher-construction failure logs
// and returns a no-op stop — the server keeps serving the initial catalog.
func watchMCPToolRegistry(sb *sandbox.Service, configPath string, reg *tools.Registry, srv *mcp.Server, stderr io.Writer) func() {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		fmt.Fprintf(stderr, "mcp-server: config watch disabled: %v\n", err)
		return func() {}
	}
	_ = w.Add(configPath) // best-effort; a missing file is not fatal

	done := make(chan struct{})
	go func() {
		timer := time.NewTimer(0)
		timer.Stop()
		pending := false
		for {
			select {
			case _, ok := <-w.Events:
				if !ok {
					return
				}
				if !pending {
					timer.Reset(mcpServerConfigDebounce)
					pending = true
				}
			case err, ok := <-w.Errors:
				if !ok {
					return
				}
				log.Printf("[mcp-server] watcher error: %v", err)
			case <-timer.C:
				pending = false
				cfg, err := config.Load()
				if err != nil {
					log.Printf("[mcp-server] reload config: %v", err)
					continue
				}
				reconcileMCPToolRegistry(reg, mcpServerToolSet(sb, cfg), func() {
					srv.PushResourceUpdated(mcp.ToolsResourceURI)
				})
			case <-done:
				timer.Stop()
				return
			}
		}
	}()

	return func() {
		close(done)
		w.Close()
	}
}
