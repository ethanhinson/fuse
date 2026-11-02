package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// writeRoleLayers writes a trusted home config and a repo-local
// .fuse.local.yml into a fresh temp dir and points HOME at it.
func writeRoleLayers(t *testing.T, home, local string) {
	t.Helper()
	cwd := chdirTemp(t)
	homeDir := filepath.Join(cwd, "home")
	if err := os.MkdirAll(filepath.Join(homeDir, ".fuse"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(homeDir, ".fuse", "config.yml"), []byte(home), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", homeDir)
	os.Unsetenv("LLM_GATEWAY_URL")
	os.Unsetenv("LLM_GATEWAY_KEY")
	if err := os.WriteFile(filepath.Join(cwd, ".fuse.local.yml"), []byte(local), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRolesFromYAML(t *testing.T) {
	doc := `
roles:
  reviewer:
    description: "Reviews a diff: correctness first."
    instructions: |
      You review code.
      Flag: correctness, then style.
    tools: [read_file, grep]
    model: fast
  researcher:
    description: Finds sources.
workflows:
  review: {skill: review, roles: [reviewer]}
`
	c, err := FromYAML([]byte(doc), "test")
	if err != nil {
		t.Fatal(err)
	}
	want := RoleConfig{
		Description:  "Reviews a diff: correctness first.",
		Instructions: "You review code.\nFlag: correctness, then style.\n",
		Tools:        []string{"read_file", "grep"},
		Model:        "fast",
	}
	if got := c.Roles["reviewer"]; !reflect.DeepEqual(got, want) {
		t.Errorf("reviewer = %#v, want %#v", got, want)
	}
	r := c.Roles["researcher"]
	if r.Description != "Finds sources." || r.Tools != nil || r.Model != "" {
		t.Errorf("researcher = %#v, want description only", r)
	}
	if got := c.Workflows["review"].Roles; !reflect.DeepEqual(got, []string{"reviewer"}) {
		t.Errorf("workflows.review.roles = %v, want [reviewer]", got)
	}

	d := Default()
	if len(d.Roles) != 0 {
		t.Errorf("Default().Roles = %v, want empty", d.Roles)
	}
	if got := d.Workflows["research"].Roles; len(got) != 0 {
		t.Errorf("Default().Workflows[research].Roles = %v, want empty", got)
	}
}

func TestRolesHomeLayerReplacesByName(t *testing.T) {
	c := Default()
	first := "roles:\n  reviewer: {description: home, tools: [read_file]}\n  other: {description: kept}\n"
	if err := mergeData(&c, []byte(first), "home", true, nil); err != nil {
		t.Fatal(err)
	}
	second := "roles:\n  reviewer: {description: again}\n"
	if err := mergeData(&c, []byte(second), "home2", true, nil); err != nil {
		t.Fatal(err)
	}
	if got := c.Roles["reviewer"]; !reflect.DeepEqual(got, RoleConfig{Description: "again"}) {
		t.Errorf("reviewer = %#v, want whole entry replaced", got)
	}
	if got := c.Roles["other"].Description; got != "kept" {
		t.Errorf("other.description = %q, want kept", got)
	}
}

func TestRolesIgnoredFromLocalLayer(t *testing.T) {
	writeRoleLayers(t,
		"roles:\n  reviewer: {description: home, tools: [read_file], model: fast}\n",
		"roles:\n  reviewer: {description: local}\n  sneaky: {description: planted}\n")
	warnings := captureWarnings(t)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]RoleConfig{"reviewer": {Description: "home", Tools: []string{"read_file"}, Model: "fast"}}
	if !reflect.DeepEqual(c.Roles, want) {
		t.Errorf("Roles = %#v, want %#v", c.Roles, want)
	}
	w := warnings()
	if !strings.Contains(w, "ignores permission-loosening keys") || !strings.Contains(w, "roles") {
		t.Errorf("warning = %q, want the permission-loosening warning naming roles", w)
	}
}

func TestWorkflowRolesFromLocalLayerAreInert(t *testing.T) {
	writeRoleLayers(t,
		"roles:\n  reviewer: {description: home}\nworkflows:\n  review: {skill: review, roles: [reviewer]}\n",
		"workflows:\n  review: {roles: [other]}\n")
	warnings := captureWarnings(t)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Workflows["review"].Roles; !reflect.DeepEqual(got, []string{"reviewer"}) {
		t.Errorf("workflows.review.roles = %v, want [reviewer]", got)
	}
	w := warnings()
	if !strings.Contains(w, "workflow pool-loosening keys") || !strings.Contains(w, "review") {
		t.Errorf("warning = %q, want the workflow warning naming review", w)
	}
}

func TestOverlaySpecRoles(t *testing.T) {
	base := Default()
	base.Roles = map[string]RoleConfig{
		"ops":      {Description: "operator"},
		"reviewer": {Description: "operator reviewer", Tools: []string{"read_file"}},
	}
	base.Workflows["review"] = WorkflowConfig{Skill: "review", Roles: []string{"ops"}}

	doc := "roles:\n  reviewer: {description: spec}\nworkflows:\n  review: {roles: [reviewer]}\n"
	c, refused, err := OverlaySpec(base, []byte(doc), "spec")
	if err != nil {
		t.Fatal(err)
	}
	if len(refused) != 0 {
		t.Errorf("refused = %v, want none", refused)
	}
	if got := c.Roles["reviewer"]; !reflect.DeepEqual(got, RoleConfig{Description: "spec"}) {
		t.Errorf("reviewer = %#v, want the spec entry", got)
	}
	if got := c.Roles["ops"].Description; got != "operator" {
		t.Errorf("ops.description = %q, want operator role kept", got)
	}
	if got := c.Workflows["review"].Roles; !reflect.DeepEqual(got, []string{"reviewer"}) {
		t.Errorf("workflows.review.roles = %v, want [reviewer]", got)
	}

	// base is untouched: clone() copied Roles and each workflow's Roles.
	if got := base.Roles["reviewer"].Description; got != "operator reviewer" {
		t.Errorf("base reviewer mutated: %q", got)
	}
	if got := base.Workflows["review"].Roles; !reflect.DeepEqual(got, []string{"ops"}) {
		t.Errorf("base workflows.review.roles mutated: %v", got)
	}
}

func TestOverlaySpecDoesNotWriteThroughRoleMap(t *testing.T) {
	base := Default()
	base.Roles = map[string]RoleConfig{"ops": {Description: "operator"}}
	doc := "roles:\n  added: {description: spec}\n"
	if _, _, err := OverlaySpec(base, []byte(doc), "spec"); err != nil {
		t.Fatal(err)
	}
	if _, ok := base.Roles["added"]; ok {
		t.Errorf("OverlaySpec wrote a spec role into the operator's Roles map")
	}
}

func TestProblemsRoles(t *testing.T) {
	roles := func(n int) map[string]RoleConfig {
		m := map[string]RoleConfig{}
		for i := 0; i < n; i++ {
			m[fmt.Sprintf("r%02d", i)] = RoleConfig{Description: "d"}
		}
		return m
	}
	type tc struct {
		name     string
		mutate   func(c *Config)
		wantPath string // "" means no role problem expected
		wantMsg  string
	}
	cases := []tc{
		{"empty description", func(c *Config) { c.Roles = map[string]RoleConfig{"a": {}} }, "roles.a.description", "required"},
		{"blank description", func(c *Config) { c.Roles = map[string]RoleConfig{"a": {Description: "   "}} }, "roles.a.description", "required"},
		{"description too long", func(c *Config) {
			c.Roles = map[string]RoleConfig{"a": {Description: strings.Repeat("x", 513)}}
		}, "roles.a.description", "512"},
		{"description at limit in characters", func(c *Config) {
			c.Roles = map[string]RoleConfig{"a": {Description: strings.Repeat("é", 512)}}
		}, "", ""},
		{"instructions too long", func(c *Config) {
			c.Roles = map[string]RoleConfig{"a": {Description: "d", Instructions: strings.Repeat("x", MaxInstructionsChars+1)}}
		}, "roles.a.instructions", "16384"},
		{"too many roles", func(c *Config) { c.Roles = roles(33) }, "roles", "32"},
		{"roles at limit", func(c *Config) { c.Roles = roles(32) }, "", ""},
		{"unresolved workflow role", func(c *Config) {
			c.Roles = map[string]RoleConfig{"a": {Description: "d"}}
			c.Workflows["wf"] = WorkflowConfig{Skill: "wf", Roles: []string{"a", "ghost"}}
		}, "workflows.wf.roles[1]", `"ghost"`},
		{"valid two-role config", func(c *Config) {
			c.Roles = map[string]RoleConfig{"a": {Description: "d"}, "b": {Description: "e", Instructions: "brief"}}
			c.Workflows["wf"] = WorkflowConfig{Skill: "wf", Roles: []string{"b"}}
		}, "", ""},
		{"uppercase role name", func(c *Config) { c.Roles = map[string]RoleConfig{"Reviewer": {Description: "d"}} }, "roles.Reviewer", "^[a-z0-9][a-z0-9_-]*$"},
		{"leading dash role name", func(c *Config) { c.Roles = map[string]RoleConfig{"-a": {Description: "d"}} }, "roles.-a", "must match"},
		{"dotted role name maps to roles", func(c *Config) { c.Roles = map[string]RoleConfig{"a.b": {Description: "d"}} }, "roles", `"a.b"`},
		{"empty role name maps to roles", func(c *Config) { c.Roles = map[string]RoleConfig{"": {Description: "d"}} }, "roles", "must match"},
		{"role name too long", func(c *Config) {
			c.Roles = map[string]RoleConfig{strings.Repeat("a", 65): {Description: "d"}}
		}, "roles." + strings.Repeat("a", 65), "64"},
		{"role name at limit", func(c *Config) {
			c.Roles = map[string]RoleConfig{strings.Repeat("a", 64): {Description: "d"}}
		}, "", ""},
		{"role name with underscore and dash", func(c *Config) { c.Roles = map[string]RoleConfig{"0_a-b": {Description: "d"}} }, "", ""},
		{"empty tool name", func(c *Config) {
			c.Roles = map[string]RoleConfig{"a": {Description: "d", Tools: []string{"read_file", ""}}}
		}, "roles.a.tools[1]", "empty"},
		{"unknown tool and model are not resolved", func(c *Config) {
			c.Roles = map[string]RoleConfig{"a": {Description: "d", Tools: []string{"no_such_tool"}, Model: "no-such-alias"}}
		}, "", ""},
	}
	baseline := len(Default().Problems())
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			c := Default()
			tt.mutate(&c)
			ps := c.Problems()
			var roleProblems []Problem
			for _, p := range ps {
				if p.Path == "roles" || strings.HasPrefix(p.Path, "roles.") || strings.Contains(p.Path, ".roles[") {
					roleProblems = append(roleProblems, p)
				}
			}
			if tt.wantPath == "" {
				if len(ps) != baseline || len(roleProblems) != 0 {
					t.Fatalf("problems = %+v, want none from roles", ps)
				}
				return
			}
			if len(roleProblems) != 1 {
				t.Fatalf("role problems = %+v, want exactly one at %s", roleProblems, tt.wantPath)
			}
			if roleProblems[0].Path != tt.wantPath {
				t.Errorf("path = %q, want %q", roleProblems[0].Path, tt.wantPath)
			}
			if !strings.Contains(roleProblems[0].Message, tt.wantMsg) {
				t.Errorf("message = %q, want it to mention %s", roleProblems[0].Message, tt.wantMsg)
			}
		})
	}
}

func TestRoleLimitsAreConstants(t *testing.T) {
	if MaxRoles != 32 {
		t.Errorf("MaxRoles = %d, want 32", MaxRoles)
	}
	if MaxRoleDescriptionChars != 512 {
		t.Errorf("MaxRoleDescriptionChars = %d, want 512", MaxRoleDescriptionChars)
	}
}
