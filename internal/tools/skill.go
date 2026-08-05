package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/ethanhinson/fuse/internal/skills"
)

type skillTool struct {
	lookup func(name string) (skills.Skill, bool)
}

// NewSkillTool returns a Tool that delivers any installed skill's body into
// the model's context. lookup is typically (*skills.Set).Lookup.
func NewSkillTool(lookup func(string) (skills.Skill, bool)) Tool {
	return &skillTool{lookup: lookup}
}

func (t *skillTool) Name() string { return "skill" }
func (t *skillTool) Description() string {
	return "Load an installed skill by name and return its full body."
}
func (t *skillTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name": map[string]any{
				"type":        "string",
				"description": `Skill name (e.g. "docket-convention")`,
			},
		},
		"required": []string{"name"},
	}
}

func (t *skillTool) Execute(_ context.Context, args string) Result {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(args), &req); err != nil {
		return Result{IsError: true, Output: fmt.Sprintf("bad arguments: %v", err)}
	}
	if req.Name == "" {
		return Result{IsError: true, Output: "name is required"}
	}
	sk, ok := t.lookup(req.Name)
	if !ok {
		return Result{IsError: true, Output: fmt.Sprintf("skill %q not found", req.Name)}
	}
	return Result{Output: sk.Body}
}
