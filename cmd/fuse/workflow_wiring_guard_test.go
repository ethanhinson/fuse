package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/probe"
)

// sharedWiringFiles are the files that DEFINE the shared child and root wiring
// (change 0091). Every other non-test file is a composition root and must reach
// the per-child tool plumbing only through loopWiring. A new composition root
// must either add its file here (if it legitimately defines shared wiring) or
// go through loopWiring; otherwise the guard below fails it.
var sharedWiringFiles = map[string]bool{
	"loop_wiring.go": true,
	"workflow.go":    true,
	"run.go":         true,
}

// directWiringCalls are the per-child plumbing calls a composition root must not
// make itself: each one bypasses the workflow scope (pool registration, worker
// allowlist, scoped spawn_agent) that loopWiring applies.
var directWiringCalls = []string{
	"childToolRegistry(",
	"wireChildBlackboard(",
	"wirePipelineTool(",
	"NewSpawnAgentToolWithBudget(",
	"resolveWorkerTools(",
}

// nonTestGoFiles returns the package's non-test .go sources, keyed by file name.
func nonTestGoFiles(t *testing.T) map[string]string {
	t.Helper()
	entries, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, f := range entries {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		out[f] = string(src)
	}
	return out
}

// TestEveryChildBuilderUsesSharedWorkflowWiring is the composition-root guard for
// change 0091 (learnings patch-every-cloned-child-builder,
// security-knob-inert-at-composition-root): a workflow's pool and worker
// allowlist are only real if every binding builds its child and root tools
// through loopWiring. A file holding N child-builder closures must call
// .childTools( and .rootTools( at least N times and install the scope backstop
// on each spawner, and no composition root may call the per-child plumbing
// directly. The marker is grepped, never a frozen list.
func TestEveryChildBuilderUsesSharedWorkflowWiring(t *testing.T) {
	sites := 0
	for f, src := range nonTestGoFiles(t) {
		n := strings.Count(src, "agent.WithChildBuilder(")
		if n > 0 {
			sites += n
			if got := strings.Count(src, ".childTools("); got < n {
				t.Errorf("%s holds %d child-builder closures but calls .childTools( %d times - a builder bypasses the shared workflow wiring", f, n, got)
			}
			if got := strings.Count(src, ".rootTools("); got < n {
				t.Errorf("%s holds %d child-builder closures but calls .rootTools( %d times - a root bypasses the shared workflow wiring", f, n, got)
			}
			if got := strings.Count(src, "agent.WithSpawnBackstop("); got < n {
				t.Errorf("%s holds %d child-builder closures but installs agent.WithSpawnBackstop( %d times - a spawner leaves the workflow pool total unenforced", f, n, got)
			}
		}
		if sharedWiringFiles[f] {
			continue
		}
		for _, call := range directWiringCalls {
			if strings.Contains(src, call) {
				t.Errorf("%s calls %s directly - composition roots must go through loopWiring.childTools/rootTools", f, call)
			}
		}
	}
	// One-shot, research probe, shell (runtime_binding.go) and the loop-server
	// factory (loop_server.go). Fewer means the marker moved and the guard would
	// pass vacuously.
	if sites < 4 {
		t.Errorf("expected >=4 child-builder closures with agent.WithChildBuilder, found %d - re-check the marker", sites)
	}
}

// TestResearchProbeRootActivatesThroughScope pins the probe's root-level
// activation (change 0091): the probe hands its deps builder a workflow scope
// whose root was activated via activateRoot, and the root's own spawn_agent then
// advertises the research workers (and never workflow, since the root is already
// inside one) with the pool registered on the scheduler.
func TestResearchProbeRootActivatesThroughScope(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := config.Default()
	if _, ok := cfg.Workflows["research"]; !ok {
		t.Fatal("default config has no research workflow")
	}
	reg := registryFromConfig(cfg)
	toolReg := defaultToolRegistry(nil, cfg.Research, nil)
	tree := agent.NewAgentTreeWithConcurrency(reg.Default, reg.Default, cfg.Agents.MaxConcurrent)
	rootNode := tree.Node(tree.RootID())
	scope := newWorkflowScope(tree, cfg.Workflows)
	if _, err := scope.activateRoot(rootNode, "research"); err != nil {
		t.Fatalf("activateRoot: %v", err)
	}

	_ = buildResearchProbeRuntimeDeps(researchProbeDepsInput{
		cfg: cfg, reg: reg, alias: reg.Default, toolReg: toolReg, tree: tree,
		scope: scope, logSink: probe.NewLog(),
	})

	if _, ok := tree.Scheduler().Pool(rootNode.ID); !ok {
		t.Fatal("research pool not registered for the probe root")
	}
	props := toolProps(t, toolReg, "spawn_agent")
	if props == nil {
		t.Fatal("probe root registry has no spawn_agent")
	}
	if _, ok := props["workflow"]; ok {
		t.Fatal("probe root spawn_agent advertises workflow, but the root already roots research")
	}
	w, ok := props["worker"].(map[string]any)
	if !ok {
		t.Fatal("probe root spawn_agent lacks a worker property")
	}
	if got := fmt.Sprint(w["enum"]); got != "[facet-researcher]" {
		t.Fatalf("worker enum = %s, want [facet-researcher]", got)
	}
}
