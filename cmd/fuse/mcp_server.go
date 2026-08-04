package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/hitl"
	"github.com/ethanhinson/fuse/internal/mcp"
	"github.com/ethanhinson/fuse/internal/permissions"
)

// runMCPServer implements the `fuse mcp-server` subcommand: a stdio MCP server
// that exposes Fuse's built-in tool registry to external AI assistants (Claude,
// Cursor, Codex, etc.) via the MCP JSON-RPC 2.0 protocol.
//
// If FUSE_HITL_SOCKET is set, tool-approval requests are relayed to the parent
// Fuse process (which holds the TUI) via that Unix socket. If unset, all tools
// are auto-approved — safe for use in MCP config without a parent process.
//
// Note: MCP servers configured in ~/.fuse/config.yml are NOT spawned here to
// avoid recursive server chains. Only Fuse's native tools are exposed.
func runMCPServer(_ []string, cfg config.Config, _ io.Writer, stderr io.Writer) int {
	toolReg := defaultToolRegistry(nil) // native tools only; no skill tool in MCP server mode

	var approve permissions.ApprovalFunc
	if socketPath := os.Getenv("FUSE_HITL_SOCKET"); socketPath != "" {
		approve = hitl.ClientApprovalFunc(socketPath)
	} else {
		approve = permissions.AlwaysApprove
	}

	gate := permissions.New(cfg.Permissions, toolReg, approve)
	srv := mcp.NewServer(os.Stdin, os.Stdout, toolReg, gate)

	if err := srv.Serve(context.Background()); err != nil {
		fmt.Fprintf(stderr, "mcp-server: %v\n", err)
		return 1
	}
	return 0
}
