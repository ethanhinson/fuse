//go:build integration

package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/config"
)

// deepwikiBaseURL is a reputable public MCP server that speaks the v2025-03-26
// Streamable HTTP transport with no auth. Unlike the Docker reference servers
// (localhost:3001, gated by requireServices), this is a live internet endpoint,
// so the test network-gates itself with a skip rather than requireServices.
const deepwikiBaseURL = "https://mcp.deepwiki.com/mcp"

func skipIfUnreachable(t *testing.T, url string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Skipf("public MCP server %s unreachable (offline?); skipping: %v", url, err)
	}
	resp.Body.Close()
}

// TestIntegration_StreamableHTTP_DeepWiki drives the real fuse Streamable HTTP
// client against DeepWiki: the full init handshake + tool discovery (which flows
// through the text/event-stream response pump, since DeepWiki streams), then a
// live tools/call round-trip. DeepWiki is stateless (no Mcp-Session-Id), so this
// also exercises the header-absent path end-to-end.
func TestIntegration_StreamableHTTP_DeepWiki(t *testing.T) {
	skipIfUnreachable(t, deepwikiBaseURL)

	client, err := newStreamableHTTPClient("deepwiki", deepwikiBaseURL, "", config.MCPAuthConfig{})
	if err != nil {
		t.Fatalf("newStreamableHTTPClient: %v", err)
	}
	defer client.stop()

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	// Full handshake (initialize -> initialized -> tools/list) + discovery,
	// transport-agnostic, driven exactly as the manager drives it.
	tools, caps, proto, err := handshakeAndDiscover(ctx, client, "deepwiki")
	if err != nil {
		t.Fatalf("handshakeAndDiscover: %v", err)
	}
	if proto != "2025-03-26" {
		t.Errorf("negotiated proto = %q, want 2025-03-26", proto)
	}
	names := mcpToolNames(tools)
	if !containsStr(names, "read_wiki_structure") {
		t.Fatalf("expected read_wiki_structure among discovered tools; got %v", names)
	}
	t.Logf("DeepWiki: proto=%s, %d tools, caps.tools=%v", proto, len(tools), caps.Supports("tools"))

	// Live tools/call round-trip — read_wiki_structure over a well-known repo.
	raw, err := client.call(ctx, "tools/call", map[string]any{
		"name":      "read_wiki_structure",
		"arguments": map[string]any{"repoName": "modelcontextprotocol/servers"},
	})
	if err != nil {
		t.Fatalf("tools/call read_wiki_structure: %v", err)
	}
	if !strings.Contains(string(raw), "content") {
		t.Fatalf("tools/call result missing content block: %s", truncate(raw, 300))
	}
}

func containsStr(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func mcpToolNames(tools []*MCPTool) []string {
	out := make([]string, 0, len(tools))
	for _, tl := range tools {
		out = append(out, tl.toolName)
	}
	return out
}

func truncate(b json.RawMessage, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}
