package tools

import (
	"context"
	"testing"

	"github.com/ethanhinson/fuse/internal/skills"
)

func stubLookup(m map[string]string) func(string) (skills.Skill, bool) {
	return func(name string) (skills.Skill, bool) {
		body, ok := m[name]
		if !ok {
			return skills.Skill{}, false
		}
		return skills.Skill{Name: name, Body: body}, true
	}
}

func TestSkillToolReturnsBody(t *testing.T) {
	tool := NewSkillTool(stubLookup(map[string]string{
		"docket-convention": "# Convention\nAll the rules.",
	}))
	res := tool.Execute(context.Background(), `{"name":"docket-convention"}`)
	if res.IsError {
		t.Fatalf("expected success: %s", res.Output)
	}
	if res.Output != "# Convention\nAll the rules." {
		t.Errorf("unexpected body: %q", res.Output)
	}
}

func TestSkillToolNotFound(t *testing.T) {
	tool := NewSkillTool(stubLookup(nil))
	res := tool.Execute(context.Background(), `{"name":"missing"}`)
	if !res.IsError {
		t.Fatal("expected error for unknown skill")
	}
}

func TestSkillToolBadJSON(t *testing.T) {
	tool := NewSkillTool(stubLookup(nil))
	res := tool.Execute(context.Background(), `not-json`)
	if !res.IsError {
		t.Fatal("expected error for bad JSON")
	}
}

func TestSkillToolEmptyName(t *testing.T) {
	tool := NewSkillTool(stubLookup(map[string]string{"x": "body"}))
	res := tool.Execute(context.Background(), `{"name":""}`)
	if !res.IsError {
		t.Fatal("expected error for empty name")
	}
}

func TestSkillToolSchema(t *testing.T) {
	tool := NewSkillTool(stubLookup(nil))
	if tool.Name() != "skill" {
		t.Errorf("name = %q", tool.Name())
	}
	params := tool.Parameters()
	props, _ := params["properties"].(map[string]any)
	if _, ok := props["name"]; !ok {
		t.Error("parameters should have 'name' property")
	}
}
