package tools

import (
	"context"
	"encoding/json"
	"fmt"
)

// PipelineRunFunc is the authored-pipeline seam (change 0026): it parses,
// validates, and runs a caller-authored pipeline definition, returning a
// terminal human-readable status line and writing pipeline.status to the
// blackboard. The raw definition bytes (YAML or JSON) cross the seam so this
// package never imports internal/pipeline — cmd/fuse supplies the closure that
// calls pipeline.Parse/Validate/Run.
type PipelineRunFunc func(ctx context.Context, definition []byte) (statusText string, err error)

// PipelineSynthFunc is the synthesized-pipeline seam (change 0026): it
// synthesizes a pipeline from a natural-language goal (writing pipeline.plan and
// a synthesis trace), then runs it, returning a terminal status line. confirm is
// plumbed through for a future interactive gate; headless it is advisory (see the
// wiring at cmd/fuse). The goal string crosses the seam so this package stays
// free of internal/pipeline types.
type PipelineSynthFunc func(ctx context.Context, goal string, confirm bool) (statusText string, err error)

// PipelineRunTool is the pipeline_run built-in tool: it runs a multi-step agent
// pipeline either from a caller-authored definition or by synthesizing one from
// a goal. It holds only injected function seams so internal/tools does not
// depend on internal/pipeline (the dependency direction is composed in cmd/fuse).
type PipelineRunTool struct {
	runFn   PipelineRunFunc
	synthFn PipelineSynthFunc
}

// NewPipelineRunTool constructs the tool with the authored-run and synthesis
// seams. Either may be nil if the corresponding mode is unused in a given wiring.
func NewPipelineRunTool(runFn PipelineRunFunc, synthFn PipelineSynthFunc) *PipelineRunTool {
	return &PipelineRunTool{runFn: runFn, synthFn: synthFn}
}

// Name returns the tool name.
func (t *PipelineRunTool) Name() string { return "pipeline_run" }

// Description returns the tool description shown to the model.
func (t *PipelineRunTool) Description() string {
	return "Run a multi-step agent pipeline (a DAG of dependent spawns). Provide " +
		"exactly one of `definition` (an authored pipeline as YAML/JSON) to run it " +
		"directly, or `goal` (a natural-language objective) to have a pipeline " +
		"synthesized and then run. The run completes when the pipeline reaches a " +
		"terminal state; the result is a status line and pipeline.status is written " +
		"to the shared blackboard."
}

// Parameters returns the JSON schema for the tool input.
func (t *PipelineRunTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"definition": map[string]any{
				"type": "string",
				"description": "An authored pipeline definition as YAML or JSON (steps, " +
					"dependencies, workers). Runs the pipeline directly with no synthesis. " +
					"Mutually exclusive with `goal`.",
			},
			"goal": map[string]any{
				"type": "string",
				"description": "A natural-language objective. A pipeline is synthesized to " +
					"achieve it (subject to the configured synthesis caps), its plan written " +
					"to the pipeline.plan blackboard key, then run. Mutually exclusive with " +
					"`definition`.",
			},
			"name": map[string]any{
				"type": "string",
				"description": "Reserved for a future registered-pipeline lookup by name. " +
					"Not supported in this version (there is no pipeline registry yet).",
			},
			"confirm": map[string]any{
				"type": "boolean",
				"description": "For a synthesized pipeline, whether to proceed after " +
					"synthesis. Defaults to false (headless-safe). Advisory when no " +
					"interactive confirmer is wired.",
			},
		},
	}
}

type pipelineRunInput struct {
	Definition string `json:"definition"`
	Goal       string `json:"goal"`
	Name       string `json:"name"`
	Confirm    bool   `json:"confirm"`
}

// Execute parses the args, infers the mode (exactly one of definition/goal), and
// routes to the matching seam. `name` is accepted but unsupported in v1 (no
// registry). Zero or both of definition/goal is a clear IsError result.
func (t *PipelineRunTool) Execute(ctx context.Context, args string) Result {
	var in pipelineRunInput
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		return Result{IsError: true, Output: fmt.Sprintf("pipeline_run: invalid args: %v", err)}
	}

	// `name` is a reserved-but-unsupported v1 param: there is no pipeline
	// registry yet, so a lookup-by-name request fails loudly rather than silently
	// doing nothing.
	if in.Name != "" && in.Definition == "" && in.Goal == "" {
		return Result{IsError: true, Output: "pipeline_run: `name` (registered-pipeline lookup) is not supported in this version; provide `definition` or `goal` instead"}
	}

	hasDef := in.Definition != ""
	hasGoal := in.Goal != ""
	if hasDef == hasGoal {
		// Both set or neither set — the mode is ambiguous.
		return Result{IsError: true, Output: "pipeline_run: provide exactly one of `definition` or `goal`"}
	}

	if hasDef {
		if t.runFn == nil {
			return Result{IsError: true, Output: "pipeline_run: authored pipelines are not available in this context"}
		}
		status, err := t.runFn(ctx, []byte(in.Definition))
		if err != nil {
			return Result{IsError: true, Output: fmt.Sprintf("pipeline_run: %v", err)}
		}
		return Result{Output: status}
	}

	if t.synthFn == nil {
		return Result{IsError: true, Output: "pipeline_run: pipeline synthesis is not available in this context"}
	}
	status, err := t.synthFn(ctx, in.Goal, in.Confirm)
	if err != nil {
		return Result{IsError: true, Output: fmt.Sprintf("pipeline_run: %v", err)}
	}
	return Result{Output: status}
}

var _ Tool = (*PipelineRunTool)(nil)
