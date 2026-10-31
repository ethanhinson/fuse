package tools

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestPipelineRunSchema asserts the advertised name and parameter surface.
func TestPipelineRunSchema(t *testing.T) {
	tool := NewPipelineRunTool(
		func(context.Context, []byte, string) (string, error) { return "", nil },
		func(context.Context, string, bool, string) (string, error) { return "", nil },
	)
	if tool.Name() != "pipeline_run" {
		t.Errorf("Name() = %q, want pipeline_run", tool.Name())
	}
	params := tool.Parameters()
	props, ok := params["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties not a map: %#v", params["properties"])
	}
	for _, key := range []string{"definition", "name", "goal", "confirm"} {
		if _, ok := props[key]; !ok {
			t.Errorf("Parameters missing %q", key)
		}
	}
}

// TestPipelineRunAuthoredRoutesToRunFn asserts a {definition} call invokes the
// authored runFn with the raw bytes and surfaces its status text.
func TestPipelineRunAuthoredRoutesToRunFn(t *testing.T) {
	var gotDef []byte
	tool := NewPipelineRunTool(
		func(_ context.Context, def []byte, _ string) (string, error) {
			gotDef = def
			return "pipeline chain: completed", nil
		},
		func(context.Context, string, bool, string) (string, error) {
			t.Fatal("synthFn must not be called for a {definition} request")
			return "", nil
		},
	)
	res := tool.Execute(context.Background(), `{"definition":"name: chain\nsteps: []"}`)
	if res.IsError {
		t.Fatalf("unexpected error result: %q", res.Output)
	}
	if string(gotDef) != "name: chain\nsteps: []" {
		t.Errorf("runFn got def %q, want the definition bytes", gotDef)
	}
	if res.Output != "pipeline chain: completed" {
		t.Errorf("Output = %q, want the runFn status text", res.Output)
	}
}

// TestPipelineRunSynthesizedRoutesToSynthFn asserts a {goal} call invokes the
// synthFn with the goal and the confirm flag plumbed, surfacing its text.
func TestPipelineRunSynthesizedRoutesToSynthFn(t *testing.T) {
	var gotGoal string
	var gotConfirm bool
	tool := NewPipelineRunTool(
		func(context.Context, []byte, string) (string, error) {
			t.Fatal("runFn must not be called for a {goal} request")
			return "", nil
		},
		func(_ context.Context, goal string, confirm bool, _ string) (string, error) {
			gotGoal = goal
			gotConfirm = confirm
			return "pipeline synth-1: completed", nil
		},
	)
	res := tool.Execute(context.Background(), `{"goal":"build a report","confirm":true}`)
	if res.IsError {
		t.Fatalf("unexpected error result: %q", res.Output)
	}
	if gotGoal != "build a report" {
		t.Errorf("synthFn got goal %q, want 'build a report'", gotGoal)
	}
	if !gotConfirm {
		t.Errorf("confirm not plumbed through; got false")
	}
	if res.Output != "pipeline synth-1: completed" {
		t.Errorf("Output = %q, want the synthFn status text", res.Output)
	}
}

// TestPipelineRunConfirmDefaultsFalse asserts confirm defaults to false
// (headless-safe) when the key is omitted.
func TestPipelineRunConfirmDefaultsFalse(t *testing.T) {
	var gotConfirm = true
	tool := NewPipelineRunTool(
		nil,
		func(_ context.Context, _ string, confirm bool, _ string) (string, error) {
			gotConfirm = confirm
			return "ok", nil
		},
	)
	res := tool.Execute(context.Background(), `{"goal":"x"}`)
	if res.IsError {
		t.Fatalf("unexpected error result: %q", res.Output)
	}
	if gotConfirm {
		t.Errorf("confirm defaulted true; want false when omitted")
	}
}

// TestPipelineRunModeInference asserts none/both are IsError and name-only is a
// clear unsupported error (no registry in v1).
func TestPipelineRunModeInference(t *testing.T) {
	tool := NewPipelineRunTool(
		func(context.Context, []byte, string) (string, error) { return "ran", nil },
		func(context.Context, string, bool, string) (string, error) { return "synthed", nil },
	)
	cases := []struct {
		name string
		args string
		want string // substring the error output must contain
	}{
		{"none", `{}`, "exactly one"},
		{"both", `{"definition":"x","goal":"y"}`, "exactly one"},
		{"name-only", `{"name":"registered"}`, "not supported"},
		{"badjson", `{`, "invalid args"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := tool.Execute(context.Background(), tc.args)
			if !res.IsError {
				t.Fatalf("expected IsError result for %s; got %q", tc.name, res.Output)
			}
			if !strings.Contains(res.Output, tc.want) {
				t.Errorf("Output = %q, want substring %q", res.Output, tc.want)
			}
		})
	}
}

// TestPipelineRunSeamErrorSurfaces asserts an error from either seam surfaces as
// an IsError result carrying the message.
func TestPipelineRunSeamErrorSurfaces(t *testing.T) {
	tool := NewPipelineRunTool(
		func(context.Context, []byte, string) (string, error) {
			return "", errors.New("validate: cycle detected")
		},
		nil,
	)
	res := tool.Execute(context.Background(), `{"definition":"bad"}`)
	if !res.IsError {
		t.Fatalf("expected IsError; got %q", res.Output)
	}
	if !strings.Contains(res.Output, "cycle detected") {
		t.Errorf("Output = %q, want the seam error", res.Output)
	}
}

var _ Tool = (*PipelineRunTool)(nil)

// TestPipelineRunForwardsWorkflow asserts the optional `workflow` argument
// crosses both seams (change 0091).
func TestPipelineRunForwardsWorkflow(t *testing.T) {
	var runWF, synthWF string
	tool := NewPipelineRunTool(
		func(_ context.Context, _ []byte, workflow string) (string, error) {
			runWF = workflow
			return "ok", nil
		},
		func(_ context.Context, _ string, _ bool, workflow string) (string, error) {
			synthWF = workflow
			return "ok", nil
		},
	)
	if res := tool.Execute(context.Background(), `{"definition":"name: x","workflow":"wf"}`); res.IsError {
		t.Fatalf("definition call errored: %q", res.Output)
	}
	if runWF != "wf" {
		t.Errorf("run seam workflow = %q, want wf", runWF)
	}
	if res := tool.Execute(context.Background(), `{"goal":"g","workflow":"wf"}`); res.IsError {
		t.Fatalf("goal call errored: %q", res.Output)
	}
	if synthWF != "wf" {
		t.Errorf("synth seam workflow = %q, want wf", synthWF)
	}
}

// TestPipelineRunWorkflowParamAdvertised asserts the `workflow` property is
// advertised (with an enum of the configured names) only when names are set.
func TestPipelineRunWorkflowParamAdvertised(t *testing.T) {
	base := NewPipelineRunTool(nil, nil)
	if _, ok := base.Parameters()["properties"].(map[string]any)["workflow"]; ok {
		t.Fatal("workflow param must be absent without configured workflows")
	}
	tool := base.WithWorkflows([]string{"wf"})
	w, ok := tool.Parameters()["properties"].(map[string]any)["workflow"].(map[string]any)
	if !ok {
		t.Fatal("workflow param should be present when workflows are configured")
	}
	enum, ok := w["enum"].([]any)
	if !ok || len(enum) != 1 || enum[0] != "wf" {
		t.Fatalf("workflow enum = %v, want [wf]", w["enum"])
	}
	if desc, _ := w["description"].(string); !strings.Contains(desc, "wf") {
		t.Errorf("workflow description missing names: %q", desc)
	}
	if _, ok := base.Parameters()["properties"].(map[string]any)["workflow"]; ok {
		t.Error("WithWorkflows must return a copy, not mutate the receiver")
	}
}
