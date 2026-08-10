package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPolicyFree_NoRendererTUIMCPInDeps proves the seam is renderer/TUI/MCP-free:
// internal/runtime's transitive dependency graph must NOT contain internal/tui or
// internal/mcp — those are each binding's business (a renderer/TUI/MCP type never
// appears at the seam). internal/permissions is intentionally NOT asserted here
// because internal/agent legitimately pulls it transitively (the gate lives inside
// the engine the seam composes); the direct-import guard below is what enforces the
// seam never itself imports the approval gate.
func TestPolicyFree_NoRendererTUIMCPInDeps(t *testing.T) {
	deps := goListDeps(t, "github.com/ethanhinson/fuse/internal/runtime")
	for _, forbidden := range []string{"internal/tui", "internal/mcp"} {
		if containsDep(deps, forbidden) {
			t.Errorf("internal/runtime transitively depends on %s — the seam must be renderer/TUI/MCP-free", forbidden)
		}
	}
}

// TestPolicyFree_NoForbiddenDirectImports greps the internal/runtime non-test source
// for a DIRECT import of a renderer/TUI/MCP/gate package. The seam must import none
// of internal/tui, internal/mcp, or internal/permissions by name — those policy
// types are wired by each binding AROUND the Runtime, never INTO it (package doc).
func TestPolicyFree_NoForbiddenDirectImports(t *testing.T) {
	forbidden := []string{
		"github.com/ethanhinson/fuse/internal/tui",
		"github.com/ethanhinson/fuse/internal/mcp",
		"github.com/ethanhinson/fuse/internal/permissions",
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue // guards only the shipped seam source, not test scaffolding
		}
		b, rerr := os.ReadFile(f)
		if rerr != nil {
			t.Fatalf("read %s: %v", f, rerr)
		}
		src := string(b)
		for _, imp := range forbidden {
			if strings.Contains(src, `"`+imp+`"`) {
				t.Errorf("%s directly imports %s — the Runtime seam must stay policy-free", f, imp)
			}
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no internal/runtime non-test source files were checked — glob or package layout changed")
	}
}
