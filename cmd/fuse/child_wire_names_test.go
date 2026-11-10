package main

import (
	"testing"

	"github.com/ethanhinson/fuse/internal/tools"
)

// A spawn_agent request names tools as the model saw them (wire names).
func TestChildToolRegistryAcceptsEncodedMCPName(t *testing.T) {
	reg := tools.NewRegistry()
	for _, tl := range tools.DefaultTools(nil) {
		reg.Register(tl)
	}
	reg.Register(staticTool{name: "mcp:srv/tool"})
	sub, err := childToolRegistry(reg, []string{"read_file", "mcp__srv__tool"})
	if err != nil {
		t.Fatalf("encoded MCP name rejected: %v", err)
	}
	if !sub.Has("mcp:srv/tool") || !sub.Has("read_file") {
		t.Fatalf("child registry missing tools: %v", sub.Tools())
	}
	if _, err := childToolRegistry(reg, []string{"mcp:srv/tool"}); err != nil {
		t.Fatalf("internal name must still be accepted: %v", err)
	}
}
