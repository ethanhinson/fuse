package tools

import (
	"context"
	"encoding/json"
	"fmt"
)

// SpawnRequest carries the model-supplied spawn arguments across the tools→agent
// seam. A struct (rather than a positional list) keeps the seam stable as new
// spawn options are added — change 0034 added Worker without touching every
// call site's argument order.
type SpawnRequest struct {
	Label        string
	Task         string
	SystemPrompt string
	Model        string
	Tools        []string
	// Worker names a workflow worker type (change 0034). Empty for freeform
	// spawns and outside a workflow. When set, the child's registry is built
	// from that worker's allowlist (the Tools subset may only narrow it).
	Worker string
	// Expects, when non-nil, is the model-supplied JSON Schema (a decoded object)
	// describing the shape the child's final result should take (change 0024). The
	// agent seam injects it into the child's system prompt and validates the
	// child's output against it. Nil ⇒ no schema requested (prior behavior).
	Expects any
}

// SpawnFunc is injected into SpawnAgentTool to break the import cycle between
// the tools registry and the agent package. It returns the child agent's final
// result text. The wiring code in cmd/fuse adapts agent.Spawner.Spawn to it.
type SpawnFunc func(ctx context.Context, req SpawnRequest) (result string, err error)

// BudgetFunc reports the tree-global spawn budget at call time: how many child
// agents have been created so far and the ceiling. A max of 0 means no budget
// is configured. The runtime supplies this so the model never counts its own
// spawns — the count is machine-authored, injected fresh into every result.
type BudgetFunc func() (used, max int)

// QuotaFunc reports the machine-authored token-quota warning line to append to a
// successful spawn result, or "" when no hard token quota is exhausted in the
// tool's scope (change 0036). The runtime supplies it computed fresh at result
// time and pre-formatted (leading blank line, no trailing newline), mirroring
// budgetLine. Scope lives in the wiring: a workflow child's QuotaFunc reports its
// subtree quota, a session-scoped one reports the global ceiling, so the warning
// appears only where the exhausted quota governs. Nil = no warning ever.
type QuotaFunc func() string

// SpawnAgentTool is the spawn_agent built-in tool, allowing LLMs to spawn
// child agents as part of a task.
type SpawnAgentTool struct {
	spawn  SpawnFunc
	budget BudgetFunc // optional; when set and max>0, a budget line is injected
	// quota, when set, supplies the token-quota warning line (change 0036): a
	// machine-authored line appended to a successful result once a hard token
	// quota is exhausted in this tool's scope, so the agent concludes with what it
	// has. Returns "" until exhausted; nil means no warning is ever emitted.
	quota QuotaFunc
	// workers, when non-empty, enumerates the workflow's worker names in the
	// `worker` param schema so the model picks a typed worker instead of
	// hand-assembling a toolset (change 0034). Empty outside a workflow subtree.
	workers []string
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

// WithWorkers returns a copy of the tool that advertises the given worker names
// in its `worker` param enum (change 0034). Used inside a workflow subtree so
// the model selects a typed worker. An empty/nil list leaves the param absent.
func (t *SpawnAgentTool) WithWorkers(names []string) *SpawnAgentTool {
	cp := *t
	cp.workers = append([]string(nil), names...)
	return &cp
}

// WithQuotaWarning returns a copy of the tool that appends the token-quota
// warning line (change 0036) supplied by quota to each successful spawn result.
// The line is computed fresh at result time and empty until a hard token quota
// is exhausted in this tool's scope, so it is absent before exhaustion and never
// rides on an error result. A nil quota is a no-op (no warning ever). It rides
// alongside any budget line already configured — the injection seam is singular.
func (t *SpawnAgentTool) WithQuotaWarning(quota QuotaFunc) *SpawnAgentTool {
	cp := *t
	cp.quota = quota
	return &cp
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
	props := map[string]any{
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
			"type":  "array",
			"items": map[string]any{"type": "string"},
			"description": "Optional subset of parent tools to give the child. " +
				"IMPORTANT: a non-empty subset that omits the blackboard_* tools " +
				"WITHHOLDS shared-memory access from the child — it then cannot read " +
				"or write the shared blackboard. If you are spawning children to " +
				"collaborate through the blackboard (debate, ensemble, producer/" +
				"consumer), either omit this field (the child inherits all tools) or " +
				"include the blackboard_* tools you need (e.g. blackboard_write, " +
				"blackboard_read, blackboard_keys) in the subset.",
		},
		"model": map[string]any{
			"type":        "string",
			"description": "Optional model ID (defaults to the parent's model).",
		},
		"expects": map[string]any{
			"type": "object",
			"description": "Optional JSON Schema describing the shape you want the child's " +
				"final result to take. When set, the child is asked to emit only conforming " +
				"JSON and its output is validated against this schema; the result is annotated " +
				"with whether it matched. A mismatch never fails the spawn.",
		},
	}
	// Inside a workflow subtree, offer a typed `worker` param enumerating the
	// workflow's worker names; the child's registry is then the worker's
	// allowlist exactly (tools may only narrow it). Change 0034.
	if len(t.workers) > 0 {
		enum := make([]any, len(t.workers))
		for i, w := range t.workers {
			enum[i] = w
		}
		props["worker"] = map[string]any{
			"type":        "string",
			"enum":        enum,
			"description": "Workflow worker type for the child. Its allowlist defines the child's tools; a worker without spawn_agent cannot itself spawn.",
		}
	}
	return map[string]any{
		"type":       "object",
		"properties": props,
		"required":   []string{"label", "task"},
	}
}

type spawnAgentInput struct {
	Label        string         `json:"label"`
	Task         string         `json:"task"`
	SystemPrompt string         `json:"system_prompt"`
	Tools        []string       `json:"tools"`
	Model        string         `json:"model"`
	Worker       string         `json:"worker"`
	Expects      map[string]any `json:"expects"`
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

	req := SpawnRequest{
		Label:        input.Label,
		Task:         input.Task,
		SystemPrompt: input.SystemPrompt,
		Model:        input.Model,
		Tools:        input.Tools,
		Worker:       input.Worker,
	}
	// Thread the expected-result schema only when the model supplied one, so an
	// absent `expects` leaves req.Expects nil (a typed-nil map would read as
	// non-nil downstream and trigger empty-schema validation). Change 0024.
	if input.Expects != nil {
		req.Expects = input.Expects
	}
	result, err := t.spawn(ctx, req)
	if err != nil {
		// Error results carry the error verbatim (which, for a budget-exhausted
		// spawn, already tells the model to stop) — never a budget line.
		return Result{IsError: true, Output: fmt.Sprintf("spawn_agent: %v", err)}
	}
	return Result{Output: result + t.budgetLine() + t.quotaWarning()}
}

// quotaWarning returns the token-quota warning suffix to append to a successful
// spawn result, or "" when no QuotaFunc is configured or the quota is not yet
// exhausted in scope. The QuotaFunc supplies the fully-formatted line (leading
// blank line, no trailing newline), read fresh at result time so the warning is
// accurate at the model's next decision. This is the singular injection seam for
// the token-quota warning line (change 0036), mirroring budgetLine.
func (t *SpawnAgentTool) quotaWarning() string {
	if t.quota == nil {
		return ""
	}
	return t.quota()
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
