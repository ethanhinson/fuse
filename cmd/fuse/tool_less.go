package main

import (
	"fmt"
	"io"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/tools"
)

// loopBaseRegistry returns the base registry a loop's root is built on
// (change 0116). A tool-less config (no_tools) yields an empty registry and
// build never runs; otherwise it is build(). The one-shot CLI and the loop
// server both compose through here, so the two entry points cannot drift.
func loopBaseRegistry(cfg config.Config, build func() *tools.Registry) *tools.Registry {
	if cfg.NoTools {
		return tools.NewRegistry()
	}
	return build()
}

// loopMCPServers is the MCP servers a loop starts: none for a tool-less
// config, whose model is offered no tools to call them with.
func loopMCPServers(cfg config.Config) []config.MCPServerConfig {
	if cfg.NoTools {
		return nil
	}
	return cfg.MCPServers
}

// withoutNoTools clears no_tools for an entry point that always runs with
// its tools (the interactive shell, the research probe), saying so once on w,
// so its registry and its system prompt agree.
func withoutNoTools(cfg config.Config, w io.Writer, entry string) config.Config {
	if !cfg.NoTools {
		return cfg
	}
	fmt.Fprintf(w, "warning: no_tools applies to one-shot runs and the loop server; %s runs with its tools\n", entry)
	cfg.NoTools = false
	return cfg
}
