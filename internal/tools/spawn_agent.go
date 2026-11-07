package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// SpawnRequest carries the model-supplied spawn arguments across the tools→agent
// seam. A struct (rather than a positional list) keeps the seam stable as new
// spawn options are added without touching every
// call site's argument order.
type SpawnRequest struct {
	Label        string
	Task         string
	SystemPrompt string
	Model        string
	Tools        []string
	// Role names a top-level role (change 0114). Empty for an ordinary spawn.
	Role string
	// Expects, when non-nil, is a JSON Schema (a decoded object) describing the
	// shape the child's final result should take (change 0024). The agent seam
	// injects it into the child's system prompt and validates the child's output
	// against it. Nil ⇒ no schema requested (prose result). As of change 0042 (D7)
	// the freeform spawn_agent tool never populates this — it is prose-only for the
	// model. Only Go callers set it (via agent.SpawnOpts.Expects).
	Expects any
}

// RoleOption is one role the spawn tool offers (change 0114): its name and the
// one-line description the model reads when choosing whom to hand work to.
type RoleOption struct {
	Name        string
	Description string
}

// SpawnResult is the tools-visible completion of a spawned child agent: the prose
// result text and the run error. It deliberately omits the structured-delegation
// value — only Go callers consume
// that, via *agent.AgentHandle directly. The tool needs only the text to build the
// model-facing result (change 0044).
type SpawnResult struct {
	Result string
	Err    error
}

// SpawnHandle is the agent-free handle the tools spawn seam awaits (change 0044).
// It is the location-transparent shape of a spawn: a caller gets a handle back and
// awaits the result, whether the child ran in-process or (later) remotely.
// *agent.AgentHandle satisfies this through a thin cmd/fuse adapter, which keeps
// internal/tools free of an internal/agent import — the very cycle SpawnFunc was
// created to break. The tool blocks on WaitResult() internally (D1), so the
// model-facing contract is byte-unchanged; the async handle is exposed only to Go
// callers of SpawnFunc.
type SpawnHandle interface {
	// WaitResult blocks until the child completes and returns its prose result and
	// run error. It is the tools-side analogue of agent.AgentHandle.Wait().
	WaitResult() SpawnResult
}

// SpawnFunc is injected into SpawnAgentTool to break the import cycle between the
// tools registry and the agent package. As of change 0044 it is handle-returning:
// it starts a spawn and returns a SpawnHandle the caller awaits (the tool awaits it
// internally). The wiring code in
// cmd/fuse adapts agent.Spawner.Spawn — which already returns agent.AgentHandle — to
// this seam. A non-nil error means the spawn was refused before it started
// (depth/budget/quota); a nil-error handle always completes via WaitResult().
type SpawnFunc func(ctx context.Context, req SpawnRequest) (SpawnHandle, error)

// BudgetFunc reports the tree-global spawn budget at call time: how many child
// agents have been created so far and the ceiling. A max of 0 means no budget
// is configured. The runtime supplies this so the model never counts its own
// spawns — the count is machine-authored, injected fresh into every result.
type BudgetFunc func() (used, max int)

// QuotaFunc reports the machine-authored token-quota warning line to append to a
// successful spawn result, or "" when no hard token quota is exhausted in the
// tool's scope (change 0036). The runtime supplies it computed fresh at result
// time and pre-formatted (leading blank line, no trailing newline), mirroring
// budgetLine. Nil = no warning ever.
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
	// roles, when non-empty, enumerates the offered roles in the `role` param
	// schema, one description line each (change 0114). Sorted by name.
	roles []RoleOption
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

// WithRoles returns a copy of the tool that advertises the given roles in its
// `role` param enum (change 0114), sorted by name, each description collapsed
// to a single line. An empty/nil list leaves the param absent.
func (t *SpawnAgentTool) WithRoles(opts []RoleOption) *SpawnAgentTool {
	cp := *t
	cp.roles = normalizeRoleOptions(opts)
	return &cp
}

// normalizeRoleOptions returns opts sorted by name with each description
// collapsed to a single line; nil for an empty list.
func normalizeRoleOptions(opts []RoleOption) []RoleOption {
	var out []RoleOption
	for _, o := range opts {
		out = append(out, RoleOption{Name: o.Name, Description: strings.Join(strings.Fields(o.Description), " ")})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
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
		"The child runs to completion and returns its findings as prose."
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
			"type":        "array",
			"items":       map[string]any{"type": "string"},
			"description": "Optional subset of parent tools to give the child. Omit it to inherit all of them.",
		},
		"model": map[string]any{
			"type":        "string",
			"description": "Optional model ID (defaults to the parent's model).",
		},
	}
	// Offer a `role` param enumerating the roles available here, one
	// description line per role (change 0114).
	if len(t.roles) > 0 {
		enum := make([]any, len(t.roles))
		var lines strings.Builder
		for i, r := range t.roles {
			enum[i] = r.Name
			lines.WriteString("\n- " + r.Name + " — " + r.Description)
		}
		props["role"] = map[string]any{
			"type": "string",
			"enum": enum,
			"description": "Optional named role for the child. The role supplies the child's system prompt, " +
				"and its tool set and model when it defines them (tools may only narrow a role's tool set; " +
				"model overrides its model). Do not pass system_prompt with a role. Roles:" +
				lines.String(),
		}
	}
	return map[string]any{
		"type":       "object",
		"properties": props,
		"required":   []string{"label", "task"},
	}
}

type spawnAgentInput struct {
	Label        string   `json:"label"`
	Task         string   `json:"task"`
	SystemPrompt string   `json:"system_prompt"`
	Tools        []string `json:"tools"`
	Model        string   `json:"model"`
	Role         string   `json:"role"`
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
		Role:         input.Role,
	}
	// Freeform model-driven spawns are prose-only: the tool no longer accepts an
	// `expects` schema, so req.Expects stays zero here and any `expects` key in the
	// raw args is ignored (change 0042, D7). A Go caller may still set
	// SpawnOpts.Expects directly for structured delegation.
	//
	// Change 0044: SpawnFunc is now handle-returning. The tool blocks INTERNALLY on
	// handle.WaitResult() and returns the same result string (prose + budget line +
	// quota line) to the model — the model-facing contract is byte-unchanged. The
	// async handle is exposed only to the Go caller of SpawnFunc;
	// the model never sees it. A spawn refused before it started (depth/budget/quota)
	// surfaces the error verbatim exactly as before.
	handle, err := t.spawn(ctx, req)
	if err != nil {
		// Error results carry the error verbatim (which, for a budget-exhausted
		// spawn, already tells the model to stop) — never a budget line.
		return Result{IsError: true, Output: fmt.Sprintf("spawn_agent: %v", err)}
	}
	done := handle.WaitResult()
	if done.Err != nil {
		return Result{IsError: true, Output: fmt.Sprintf("spawn_agent: %v", done.Err)}
	}
	return Result{Output: done.Result + t.budgetLine() + t.quotaWarning()}
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

var _ Tool = (*SpawnAgentTool)(nil)
