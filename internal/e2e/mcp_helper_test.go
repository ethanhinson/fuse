package e2e

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

const (
	mcpModeEnv    = "E2E_MCP_MODE"
	mcpPidFileEnv = "E2E_MCP_PIDFILE"
	mcpEnvDumpEnv = "E2E_MCP_ENVDUMP"
)

// runMCPHelper turns this test binary into a fake stdio MCP server. Every mode
// exits by itself within two minutes, so a leaked helper can't outlive the suite
// for long even if a test's process-group kill misses it.
//
// Modes: normal, crash (dies during tools/call), noinit (never answers
// initialize), oversize (30MB result), hangcall (never answers tools/call),
// ignoreeof (normal, but outlives stdin closing, like a badly behaved server).
func runMCPHelper(mode string) {
	go func() {
		time.Sleep(2 * time.Minute)
		os.Exit(9)
	}()
	if p := os.Getenv(mcpPidFileEnv); p != "" {
		_ = os.WriteFile(p, []byte(fmt.Sprint(os.Getpid())), 0o644)
	}
	if p := os.Getenv(mcpEnvDumpEnv); p != "" {
		_ = os.WriteFile(p, []byte(strings.Join(os.Environ(), "\n")), 0o644)
	}
	out := bufio.NewWriter(os.Stdout)
	reply := func(id json.RawMessage, result any) {
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
		out.Write(b)
		out.WriteByte('\n')
		out.Flush()
	}
	dec := json.NewDecoder(os.Stdin)
	for {
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := dec.Decode(&msg); err != nil {
			break
		}
		switch msg.Method {
		case "initialize":
			if mode == "noinit" {
				continue
			}
			reply(msg.ID, map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "h", "version": "1"}})
		case "tools/list":
			reply(msg.ID, map[string]any{"tools": []any{map[string]any{"name": "echo", "description": "echo", "inputSchema": map[string]any{"type": "object"}}}})
		case "tools/call":
			switch mode {
			case "crash":
				os.Exit(3)
			case "hangcall":
				continue
			case "oversize":
				reply(msg.ID, map[string]any{"content": []any{map[string]any{"type": "text", "text": strings.Repeat("O", 30<<20)}}})
			default:
				reply(msg.ID, map[string]any{"content": []any{map[string]any{"type": "text", "text": "echoed"}}})
			}
		}
	}
	if mode == "ignoreeof" {
		time.Sleep(2 * time.Minute)
	}
}
