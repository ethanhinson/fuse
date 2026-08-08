package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/tools"
)

// blackboardToolNames is the set the wiring must produce, mirroring
// tools.NewBlackboardTools.
var blackboardToolNames = []string{
	"blackboard_write",
	"blackboard_read",
	"blackboard_wait",
	"blackboard_keys",
	"blackboard_delete",
}

func regNames(reg *tools.Registry) map[string]bool {
	out := map[string]bool{}
	for _, s := range reg.Schemas() {
		out[s.Name] = true
	}
	return out
}

// TestRootBlackboardToolsWired asserts a root registry built the production way
// (spawn tools + NewBlackboardTools(bb.ForNode(rootNode))) exposes all five
// blackboard tools.
func TestRootBlackboardToolsWired(t *testing.T) {
	tree := agent.NewAgentTree("root", "m")
	rootNode := tree.Node(tree.RootID())
	bb := agent.NewBlackboard(tree)

	reg := tools.NewRegistry()
	for _, tl := range tools.DefaultTools() {
		reg.Register(tl)
	}
	// Exercise the real production helper the entry points call.
	wireRootBlackboard(reg, bb, rootNode)

	names := regNames(reg)
	for _, n := range blackboardToolNames {
		if !names[n] {
			t.Errorf("root registry missing blackboard tool %q", n)
		}
	}
}

// buildChildReg builds a child registry the production way: childToolRegistry
// from the parent, then wireChildBlackboard with the same subset. spawnWithheld
// simulates a child that cannot spawn (depth strip / shouldWireChildSpawn false):
// the blackboard tools must still be present.
func buildChildReg(t *testing.T, requested []string, spawnWithheld bool) *tools.Registry {
	t.Helper()
	tree := agent.NewAgentTree("root", "m")
	rootNode := tree.Node(tree.RootID())
	bb := agent.NewBlackboard(tree)

	// Parent registry as it would exist at spawn time: default tools plus the
	// root-bound blackboard tools (so a subset can select them by name).
	parent := tools.NewRegistry()
	for _, tl := range tools.DefaultTools() {
		parent.Register(tl)
	}
	for _, tl := range tools.NewBlackboardTools(bb.ForNode(rootNode)) {
		parent.Register(tl)
	}

	childReg, err := childToolRegistry(parent, requested)
	if err != nil {
		t.Fatalf("childToolRegistry(%v): %v", requested, err)
	}
	// Mirror the production spawn-gate: withhold spawn_agent when it cannot spawn.
	if spawnWithheld {
		childReg.Unregister("spawn_agent")
	}
	childNode := &agent.AgentNode{ID: "child-1", Label: "child"}
	wireChildBlackboard(childReg, bb, childNode, requested)
	return childReg
}

// TestChildBlackboardAlwaysWiredNoSubset asserts a child built with NO subset
// (full clone) has all five blackboard tools even when spawning is withheld.
func TestChildBlackboardAlwaysWiredNoSubset(t *testing.T) {
	reg := buildChildReg(t, nil, true /* spawn withheld */)
	names := regNames(reg)
	for _, n := range blackboardToolNames {
		if !names[n] {
			t.Errorf("no-subset child missing blackboard tool %q (must be wired even when spawn withheld)", n)
		}
	}
	if names["spawn_agent"] {
		t.Error("spawn_agent should be withheld in this scenario")
	}
}

// TestChildBlackboardAbsentWhenSubsetExcludes asserts an explicit subset that
// does not name any blackboard tool yields a child with none of them.
func TestChildBlackboardAbsentWhenSubsetExcludes(t *testing.T) {
	reg := buildChildReg(t, []string{"read_file"}, false)
	names := regNames(reg)
	for _, n := range blackboardToolNames {
		if names[n] {
			t.Errorf("subset excluding blackboard must not contain %q", n)
		}
	}
	if !names["read_file"] {
		t.Error("subset should still contain the named read_file tool")
	}
}

// TestChildBlackboardPresentWhenSubsetIncludes asserts a subset naming a
// blackboard tool yields exactly that tool (child-bound), and not the others.
func TestChildBlackboardPresentWhenSubsetIncludes(t *testing.T) {
	reg := buildChildReg(t, []string{"read_file", "blackboard_write", "blackboard_read"}, false)
	names := regNames(reg)
	if !names["blackboard_write"] || !names["blackboard_read"] {
		t.Errorf("subset naming blackboard_write/read must contain them: %v", sortedKeys(names))
	}
	for _, n := range []string{"blackboard_wait", "blackboard_keys", "blackboard_delete"} {
		if names[n] {
			t.Errorf("subset did not name %q, it must be absent", n)
		}
	}
}

// TestSpawnBlockWarnsAboutBlackboardSubset guards the prompting fix: because a
// narrow tools subset silently strips blackboard access from a child (see
// TestChildBlackboardAbsentWhenSubsetExcludes), the spawn system-prompt block
// MUST warn the model to omit `tools` or include blackboard_* when children need
// shared memory — otherwise a debate/ensemble produces an empty board. This
// pins that guidance so it can't silently regress.
func TestSpawnBlockWarnsAboutBlackboardSubset(t *testing.T) {
	for _, want := range []string{"blackboard", "subset", "shared memory"} {
		if !strings.Contains(strings.ToLower(spawnAgentBlock), strings.ToLower(want)) {
			t.Errorf("spawnAgentBlock must mention %q so models don't strip blackboard from children", want)
		}
	}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestEveryAgentEntryPointWiresBlackboard is the regression net the spec asked
// for: the blackboard tool must be wired at every agent entry point (root +
// child builder). The child-builder closures and root registration live inline
// in three files, so a helper-level registry test cannot catch a site that
// simply never calls the wiring. This test enumerates the entry-point sources by
// grepping the tree for the child-builder marker (never a frozen list — learning
// patch-every-cloned-child-builder) and asserts each such file calls BOTH
// wireRootBlackboard and wireChildBlackboard. Dropping either call from any
// entry point — or adding a fourth entry point that forgets the wiring — fails
// here.
func TestEveryAgentEntryPointWiresBlackboard(t *testing.T) {
	entries, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	sites := 0
	for _, f := range entries {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		s := string(src)
		// A child-builder closure is the signature of an agent entry point.
		if !strings.Contains(s, "agent.WithChildBuilder(") {
			continue
		}
		sites++
		if !strings.Contains(s, "wireRootBlackboard(") {
			t.Errorf("%s builds an agent (WithChildBuilder) but never calls wireRootBlackboard — root blackboard wiring missing", f)
		}
		if !strings.Contains(s, "wireChildBlackboard(") {
			t.Errorf("%s builds an agent (WithChildBuilder) but never calls wireChildBlackboard — child blackboard wiring missing", f)
		}
	}
	// Guard against the marker changing out from under the test (which would make
	// it silently pass by finding zero sites). Today there are three entry points:
	// main.go, shell.go, research_probe.go.
	if sites < 3 {
		t.Errorf("expected >=3 agent entry-point files with WithChildBuilder, found %d — re-check the marker", sites)
	}
}
