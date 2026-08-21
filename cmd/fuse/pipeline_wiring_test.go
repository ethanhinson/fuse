package main

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/internal/tools"
)

// noopRunFn / noopSynthFn are inert seams for wiring-only assertions.
func noopRunFn(context.Context, []byte) (string, error)         { return "", nil }
func noopSynthFn(context.Context, string, bool) (string, error) { return "", nil }

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
// patch-every-cloned-child-builder): every agent entry point (a file with a
// child-builder closure) must call BOTH wirePipelineTool at the root and inside
// its child builder. Dropping either call from any entry point — or adding a new
// entry point that forgets it — fails here. The marker is grepped, never a frozen
// list.
func TestEveryAgentEntryPointWiresPipeline(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	// Since change 0045 (binding #1) the three cloned child builders are consolidated
	// as three child-builder CLOSURES through one runtime.Deps seam in
	// runtime_binding.go: count closures, not files. Each of the three builders wires
	// pipeline_run at BOTH the root and inside the child builder, so a file holding N
	// child-builder closures must have >= 2*N wirePipelineTool calls.
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
		// Two calls are expected per builder: one at the root, one in the child
		// builder — so require at least 2*n occurrences of the helper.
		if strings.Count(s, "wirePipelineTool(") < 2*n {
			t.Errorf("%s holds %d child-builder closures but has fewer than %d wirePipelineTool calls — a builder omits root or child pipeline wiring", f, n, 2*n)
		}
	}
	if sites < 3 {
		t.Errorf("expected >=3 child-builder closures with WithChildBuilder, found %d — re-check the marker", sites)
	}
}
