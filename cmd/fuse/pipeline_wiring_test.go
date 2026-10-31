package main

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/internal/tools"
)

// noopRunFn / noopSynthFn are inert seams for wiring-only assertions.
func noopRunFn(context.Context, []byte, string) (string, error)         { return "", nil }
func noopSynthFn(context.Context, string, bool, string) (string, error) { return "", nil }

// TestRootPipelineToolWired asserts wirePipelineTool with an empty subset (a full
// clone, as the root always is) registers pipeline_run.
func TestRootPipelineToolWired(t *testing.T) {
	reg := tools.NewRegistry()
	for _, tl := range tools.DefaultTools(nil) {
		reg.Register(tl)
	}
	wirePipelineTool(reg, noopRunFn, noopSynthFn, nil)
	if !regNames(reg)["pipeline_run"] {
		t.Errorf("root registry missing pipeline_run")
	}
}

// TestChildPipelineToolWiredNoSubset asserts a child built with no subset (full
// clone) carries pipeline_run.
func TestChildPipelineToolWiredNoSubset(t *testing.T) {
	reg := tools.NewRegistry()
	for _, tl := range tools.DefaultTools(nil) {
		reg.Register(tl)
	}
	wirePipelineTool(reg, noopRunFn, noopSynthFn, nil)
	if !regNames(reg)["pipeline_run"] {
		t.Errorf("no-subset child missing pipeline_run")
	}
}

// TestChildPipelineToolAbsentWhenSubsetExcludes asserts an explicit subset that
// does not name pipeline_run yields a child without it.
func TestChildPipelineToolAbsentWhenSubsetExcludes(t *testing.T) {
	reg := tools.NewRegistry()
	// Simulate an inherited copy already present, then a narrow subset excludes it.
	reg.Register(tools.NewPipelineRunTool(noopRunFn, noopSynthFn))
	wirePipelineTool(reg, noopRunFn, noopSynthFn, []string{"read_file"})
	if regNames(reg)["pipeline_run"] {
		t.Errorf("subset excluding pipeline_run must not contain it")
	}
}

// TestChildPipelineToolPresentWhenSubsetIncludes asserts a subset naming
// pipeline_run yields it.
func TestChildPipelineToolPresentWhenSubsetIncludes(t *testing.T) {
	reg := tools.NewRegistry()
	wirePipelineTool(reg, noopRunFn, noopSynthFn, []string{"read_file", "pipeline_run"})
	if !regNames(reg)["pipeline_run"] {
		t.Errorf("subset naming pipeline_run must contain it")
	}
}

// TestEveryAgentEntryPointWiresPipeline is the regression net (learning
// patch-every-cloned-child-builder): every agent entry point must wire
// pipeline_run at BOTH the root and inside its child builder. Since change 0091
// every binding does that through loopWiring, so a file holding N child-builder
// closures must call .childTools( and .rootTools( at least N times, and
// loop_wiring.go must wire pipeline_run from both (w.wirePipeline in childTools
// and rootTools, registered through wireBuiltPipelineTool). Dropping either path,
// or adding an entry point that bypasses loopWiring, fails here. The marker is
// grepped, never a frozen list.
func TestEveryAgentEntryPointWiresPipeline(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	sites := 0
	for _, e := range entries {
		f := e.Name()
		if !strings.HasSuffix(f, ".go") || strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		s := string(src)
		n := strings.Count(s, "agent.WithChildBuilder(")
		if n == 0 {
			continue
		}
		sites += n
		if strings.Count(s, ".childTools(") < n || strings.Count(s, ".rootTools(") < n {
			t.Errorf("%s holds %d child-builder closures but does not reach .childTools( and .rootTools( for each - a builder omits root or child pipeline wiring", f, n)
		}
	}
	if sites < 4 {
		t.Errorf("expected >=4 child-builder closures with WithChildBuilder, found %d - re-check the marker", sites)
	}
	src, err := os.ReadFile("loop_wiring.go")
	if err != nil {
		t.Fatal(err)
	}
	lw := string(src)
	// One call in childTools, one in rootTools.
	if got := strings.Count(lw, "w.wirePipeline("); got < 2 {
		t.Errorf("loop_wiring.go calls w.wirePipeline( %d times, want >= 2 (child and root)", got)
	}
	if !strings.Contains(lw, "wireBuiltPipelineTool(") {
		t.Error("loop_wiring.go never registers pipeline_run through wireBuiltPipelineTool")
	}
}
