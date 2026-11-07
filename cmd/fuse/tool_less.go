package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/tools"
)

// loopBaseRegistry returns the base registry a loop's root is built on
// (change 0116). A tool-less config (no_tools) yields an empty registry and
// build never runs; otherwise it is build().
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

// errToolLessCLIModel fails a tool-less build on a cli/ model closed. The CLI
// adapter's subprocess always attaches fuse's own MCP server, which offers the
// full tool set whatever the request carries, so honouring no_tools there is
// impossible; building anyway would hand tools to an agent told it has none.
func errToolLessCLIModel(cfg config.Config, modelID string) error {
	if cfg.NoTools && strings.HasPrefix(modelID, "cli/") {
		return fmt.Errorf("no_tools is not supported with CLI model %q", modelID)
	}
	return nil
}

// withoutNoTools clears no_tools for an entry point that always runs with
// its tools (the interactive shell, the research probe), saying so once on w,
// so its registry and its system prompt agree.
func withoutNoTools(cfg config.Config, w io.Writer, entry string) config.Config {
	if !cfg.NoTools {
		return cfg
	}
	fmt.Fprintf(w, "warning: no_tools applies to one-shot runs; %s runs with its tools\n", entry)
	cfg.NoTools = false
	return cfg
}
