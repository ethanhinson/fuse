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

// BudgetFunc reports the tree-global spawn budget at call time: how many child
// agents have been created so far and the ceiling. A max of 0 means no budget
// is configured. The runtime supplies this so the model never counts its own
// spawns — the count is machine-authored, injected fresh into every result.
type BudgetFunc func() (used, max int)

// SpawnAgentTool is the spawn_agent built-in tool, allowing LLMs to spawn
// child agents as part of a task.
type SpawnAgentTool struct {
	spawn  SpawnFunc
	budget BudgetFunc // optional; when set and max>0, a budget line is injected
}

// NewSpawnAgentTool creates a spawn_agent tool with the given spawn function
// and no budget injection.
func NewSpawnAgentTool(spawn SpawnFunc) *SpawnAgentTool {
	return &SpawnAgentTool{spawn: spawn}
}

// NewSpawnAgentToolWithBudget creates a spawn_agent tool that appends a
// machine-generated budget line to every successful spawn result, computed from
// budget at result time. This is the injection that steers a model to stop
// spawning as the budget nears exhaustion — visible at exactly the decision
// point where it chooses whether to spawn again.
func NewSpawnAgentToolWithBudget(spawn SpawnFunc, budget BudgetFunc) *SpawnAgentTool {
	return &SpawnAgentTool{spawn: spawn, budget: budget}
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
		// Error results carry the error verbatim (which, for a budget-exhausted
		// spawn, already tells the model to stop) — never a budget line.
		return Result{IsError: true, Output: fmt.Sprintf("spawn_agent: %v", err)}
	}
	return Result{Output: result + t.budgetLine()}
}

// budgetLine returns the machine-generated budget suffix to append to a
// successful spawn result, or "" when no budget is configured. The count is
// read fresh at result time so it is accurate at the model's next decision.
func (t *SpawnAgentTool) budgetLine() string {
	if t.budget == nil {
		return ""
	}
	used, max := t.budget()
	if max <= 0 {
		return ""
	}
	remaining := max - used
	if remaining < 0 {
		remaining = 0
	}
	return fmt.Sprintf("\n\nagent budget: %d/%d used (%d remaining)", used, max, remaining)
}

// TighterBudget composes two BudgetFuncs into one that, at call time, reports
// whichever has FEWER remaining (max-used) — the binding constraint (change
// 0034 Acceptance 5). A workflow child reads the tighter of its workflow-total
// and the tree-global budget, so the injected budget line always shows the wall
// it will actually hit first. An operand whose max <= 0 is unset and ignored; if
// both are unset the result is unset (0,0) and budgetLine emits nothing.
func TighterBudget(a, b BudgetFunc) BudgetFunc {
	return func() (used, max int) {
		type cand struct{ used, max int }
		best := cand{}
		bestSet := false
		consider := func(f BudgetFunc) {
			if f == nil {
				return
			}
			u, m := f()
			if m <= 0 {
				return // unset
			}
			rem := m - u
			if !bestSet || rem < best.max-best.used {
				best = cand{u, m}
				bestSet = true
			}
		}
		consider(a)
		consider(b)
		return best.used, best.max
	}
}

var _ Tool = (*SpawnAgentTool)(nil)
