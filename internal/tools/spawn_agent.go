package tools

import (
	"context"
	"encoding/json"
	"fmt"
)

// SpawnFunc is injected into SpawnAgentTool to break the import cycle between
// the tools registry and the agent package. It accepts primitive arguments and
// returns the child agent's final result text.
// The wiring code in cmd/fuse adapts agent.Spawner.Spawn to this signature.
type SpawnFunc func(
	ctx context.Context,
	label, task, systemPrompt, model string,
	tools []string,
) (result string, err error)

// SpawnAgentTool is the spawn_agent built-in tool, allowing LLMs to spawn
// child agents as part of a task.
type SpawnAgentTool struct {
	spawn SpawnFunc
}

// NewSpawnAgentTool creates a spawn_agent tool with the given spawn function.
func NewSpawnAgentTool(spawn SpawnFunc) *SpawnAgentTool {
	return &SpawnAgentTool{spawn: spawn}
}

// Name returns the tool name.
func (t *SpawnAgentTool) Name() string { return "spawn_agent" }

// Description returns the tool description shown to the model.
func (t *SpawnAgentTool) Description() string {
	return "Spawn a child agent to execute a subtask concurrently. " +
		"The child runs to completion and returns its final result."
}

// Parameters returns the JSON schema for the tool input.
func (t *SpawnAgentTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"label": map[string]any{
				"type":        "string",
				"description": "Short human label shown in the agent tree (required).",
			},
			"task": map[string]any{
				"type":        "string",
				"description": "The child agent's task prompt (required).",
			},
			"system_prompt": map[string]any{
				"type":        "string",
				"description": "Optional system prompt override; fully replaces the inherited one.",
			},
			"tools": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "Optional subset of parent tools to give the child.",
			},
			"model": map[string]any{
				"type":        "string",
				"description": "Optional model ID (defaults to the parent's model).",
			},
		},
		"required": []string{"label", "task"},
	}
}

type spawnAgentInput struct {
	Label        string   `json:"label"`
	Task         string   `json:"task"`
	SystemPrompt string   `json:"system_prompt"`
	Tools        []string `json:"tools"`
	Model        string   `json:"model"`
}

// Execute parses the input, spawns a child agent, and blocks until it completes.
func (t *SpawnAgentTool) Execute(ctx context.Context, args string) Result {
	var input spawnAgentInput
	if err := json.Unmarshal([]byte(args), &input); err != nil {
		return Result{IsError: true, Output: fmt.Sprintf("spawn_agent: invalid args: %v", err)}
	}
	if input.Label == "" || input.Task == "" {
		return Result{IsError: true, Output: "spawn_agent: label and task are required"}
	}

	result, err := t.spawn(
		ctx,
		input.Label, input.Task, input.SystemPrompt, input.Model,
		input.Tools,
	)
	if err != nil {
		return Result{IsError: true, Output: fmt.Sprintf("spawn_agent: %v", err)}
	}
	return Result{Output: result}
}

var _ Tool = (*SpawnAgentTool)(nil)
