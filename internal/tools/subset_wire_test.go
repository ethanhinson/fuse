package tools

import "testing"

// The model only sees wire names, so a spawn request names MCP tools by their
// encoded form; Subset must map them back, and still accept internal names.
func TestRegistrySubset_AcceptsWireNames(t *testing.T) {
	r := NewRegistry()
	r.Register(fakeTool{name: "bash"})
	r.Register(fakeTool{name: "mcp:srv/tool"})
	r.Register(fakeTool{name: "mcp:srv/other"})

	sub, unknown := r.Subset([]string{"mcp__srv__tool", "mcp:srv/other", "mcp__srv__ghost"})
	if len(unknown) != 1 || unknown[0] != "mcp__srv__ghost" {
		t.Fatalf("unknown = %v", unknown)
	}
	if !sub.Has("mcp:srv/tool") || !sub.Has("mcp:srv/other") || sub.Has("bash") {
		t.Fatalf("subset has wrong tools: %v", sub.Tools())
	}
}
