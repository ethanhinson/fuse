package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/mcp"
	"github.com/ethanhinson/fuse/internal/tools"
)

// runMCPs dispatches fuse mcps [subcommand].
func runMCPs(args []string, cfg config.Config, stdout, stderr io.Writer) int {
	sub := "list"
	if len(args) > 0 {
		sub = args[0]
		args = args[1:]
	}

	switch sub {
	case "list":
		return mcpList(args, cfg, stdout, stderr)
	case "add":
		return mcpAdd(args, cfg, stdout, stderr)
	case "remove":
		return mcpRemove(args, stdout, stderr)
	case "tools":
		return mcpTools(args, cfg, stdout, stderr)
	case "logs":
		return mcpLogs(args, cfg, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown mcps subcommand %q\n", sub)
		fmt.Fprintln(stderr, "usage: fuse mcps [list|add|remove|tools|logs]")
		return 2
	}
}

func mcpList(args []string, cfg config.Config, stdout, stderr io.Writer) int {
	live := false
	for _, a := range args {
		if a == "--live" {
			live = true
		}
	}

	if !live {
		// Static list from config.
		fmt.Fprintf(stdout, "%-20s %-10s %-8s\n", "NAME", "TRANSPORT", "AUTH")
		for _, s := range cfg.MCPServers {
			t := s.Transport
			if t == "" {
				t = "stdio"
			}
			auth := s.Auth.Type
			if auth == "" {
				auth = "none"
			}
			fmt.Fprintf(stdout, "%-20s %-10s %-8s\n", s.Name, t, auth)
		}
		return 0
	}

	// Live: dial fresh connections and check status.
	toolReg := tools.NewRegistry()
	mgr, err := mcp.NewManager(cfg.MCPServers, toolReg)
	if err != nil {
		fmt.Fprintf(stderr, "mcp manager: %v\n", err)
		return 1
	}
	defer mgr.Close()

	statuses := mgr.Status()
	fmt.Fprintf(stdout, "%-20s %-10s %-8s %-10s %-6s\n", "NAME", "TRANSPORT", "AUTH", "STATUS", "TOOLS")
	for _, s := range statuses {
		status := "ok"
		if !s.Connected {
			status = "error"
		}
		t := s.Transport
		if t == "" {
			t = "stdio"
		}
		auth := s.AuthType
		if auth == "" {
			auth = "none"
		}
		toolCount := "-"
		if s.Connected {
			toolCount = fmt.Sprintf("%d", len(s.Tools))
		}
		fmt.Fprintf(stdout, "%-20s %-10s %-8s %-10s %-6s\n", s.Name, t, auth, status, toolCount)
	}
	return 0
}

func mcpAdd(args []string, cfg config.Config, stdout, stderr io.Writer) int {
	srv := config.MCPServerConfig{}
	var authType, token, clientID, clientSecret, scopesStr string

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--name":
			if i+1 < len(args) {
				srv.Name = args[i+1]
				i++
			}
		case "--transport":
			if i+1 < len(args) {
				srv.Transport = args[i+1]
				i++
			}
		case "--command":
			if i+1 < len(args) {
				srv.Command = strings.Fields(args[i+1])
				i++
			}
		case "--url":
			if i+1 < len(args) {
				srv.URL = args[i+1]
				i++
			}
		case "--auth":
			if i+1 < len(args) {
				authType = args[i+1]
				i++
			}
		case "--token":
			if i+1 < len(args) {
				token = args[i+1]
				i++
			}
		case "--client-id":
			if i+1 < len(args) {
				clientID = args[i+1]
				i++
			}
		case "--client-secret":
			if i+1 < len(args) {
				clientSecret = args[i+1]
				i++
			}
		case "--scopes":
			if i+1 < len(args) {
				scopesStr = args[i+1]
				i++
			}
		}
	}

	if srv.Name == "" {
		fmt.Fprintln(stderr, "fuse mcps add: --name is required")
		return 2
	}
	if srv.Transport == "" {
		srv.Transport = "stdio"
	}

	if authType != "" {
		srv.Auth.Type = authType
		srv.Auth.ClientSecret = token
		srv.Auth.ClientID = clientID
		if clientSecret != "" {
			srv.Auth.ClientSecret = clientSecret
		}
		if scopesStr != "" {
			srv.Auth.Scopes = strings.Split(scopesStr, ",")
		}
	}

	if err := config.AddMCPServer(srv); err != nil {
		fmt.Fprintf(stderr, "fuse mcps add: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "added MCP server %q\n", srv.Name)
	return 0
}

func mcpRemove(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "fuse mcps remove: NAME is required")
		return 2
	}
	name := args[0]
	if err := config.RemoveMCPServer(name); err != nil {
		fmt.Fprintf(stderr, "fuse mcps remove: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "removed MCP server %q\n", name)
	return 0
}

func mcpTools(args []string, cfg config.Config, stdout, stderr io.Writer) int {
	var filter string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		filter = args[0]
	}

	toolReg := tools.NewRegistry()
	mgr, err := mcp.NewManager(cfg.MCPServers, toolReg)
	if err != nil {
		fmt.Fprintf(stderr, "mcp manager: %v\n", err)
		return 1
	}
	defer mgr.Close()

	for _, srv := range mgr.Servers() {
		if filter != "" && srv.Name != filter {
			continue
		}
		fmt.Fprintf(stdout, "%s:\n", srv.Name)
		for _, t := range srv.Tools {
			fmt.Fprintf(stdout, "  %s — %s\n", t.Name, t.Description)
		}
	}
	return 0
}

func mcpLogs(args []string, cfg config.Config, stdout, stderr io.Writer) int {
	var filter string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		filter = args[0]
	}

	toolReg := tools.NewRegistry()
	mgr, err := mcp.NewManager(cfg.MCPServers, toolReg)
	if err != nil {
		fmt.Fprintf(stderr, "mcp manager: %v\n", err)
		return 1
	}
	defer mgr.Close()

	for _, s := range mgr.Status() {
		if filter != "" && s.Name != filter {
			continue
		}
		if s.Transport != "stdio" && s.Transport != "" {
			continue // logs only available for stdio servers
		}
		fmt.Fprintf(stdout, "=== %s ===\n", s.Name)
		for _, line := range s.LogLines {
			fmt.Fprintln(stdout, line)
		}
	}
	return 0
}
