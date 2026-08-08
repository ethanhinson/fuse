package main

import (
	"sort"
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
	for _, tl := range tools.NewBlackboardTools(bb.ForNode(rootNode)) {
		reg.Register(tl)
	}

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

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
