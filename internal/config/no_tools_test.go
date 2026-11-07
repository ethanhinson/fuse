package config

import (
	"reflect"
	"strings"
	"testing"
)

func TestNoToolsFromYAML(t *testing.T) {
	doc := `
no_tools: true
roles:
  quiet: {description: d, no_tools: true}
`
	c, err := FromYAML([]byte(doc), "test")
	if err != nil {
		t.Fatal(err)
	}
	if !c.NoTools {
		t.Error("Config.NoTools = false, want true")
	}
	if !c.Roles["quiet"].NoTools {
		t.Error("Roles[quiet].NoTools = false, want true")
	}
	if Default().NoTools {
		t.Error("Default().NoTools = true, want false")
	}
}

func TestNoToolsHonoredFromLocalLayer(t *testing.T) {
	writeRoleLayers(t, "max_turns: 3\n", "no_tools: true\n")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !c.NoTools {
		t.Error("NoTools = false, want true from .fuse.local.yml")
	}
}

func TestNoToolsLaterLayerWithoutKeyKeepsTrue(t *testing.T) {
	writeRoleLayers(t, "no_tools: true\n", "max_turns: 3\n")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !c.NoTools {
		t.Error("NoTools = false, want home's true kept")
	}
	c2 := Default()
	if err := mergeData(&c2, []byte("no_tools: true\n"), "a", true, nil); err != nil {
		t.Fatal(err)
	}
	if err := mergeData(&c2, []byte("no_tools: false\n"), "b", true, nil); err != nil {
		t.Fatal(err)
	}
	if !c2.NoTools {
		t.Error("NoTools = false after a later explicit false, want true (it only ever removes tools)")
	}
}

func TestNoToolsLocalLayerRolesStayInert(t *testing.T) {
	writeRoleLayers(t,
		"roles:\n  reviewer: {description: home}\n",
		"roles:\n  reviewer: {description: home, no_tools: true}\n")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]RoleConfig{"reviewer": {Description: "home"}}; !reflect.DeepEqual(c.Roles, want) {
		t.Errorf("Roles = %#v, want %#v", c.Roles, want)
	}
}

func TestProblemsNoTools(t *testing.T) {
	type tc struct {
		name     string
		mutate   func(c *Config)
		wantPath string // "" means no problem beyond the default baseline
		wantMsg  string
	}
	cases := []tc{
		{"role names tools and no_tools", func(c *Config) {
			c.Roles = map[string]RoleConfig{"x": {Description: "d", Tools: []string{"read_file"}, NoTools: true}}
		}, "roles.x.no_tools", "names tools and no_tools; choose one"},
		{"tool-less role", func(c *Config) {
			c.Roles = map[string]RoleConfig{"x": {Description: "d", NoTools: true}}
		}, "", ""},
		{"top-level no_tools beside everything", func(c *Config) {
			c.NoTools = true
			c.Permissions.Disabled = []string{"bash"}
			c.MCPServers = []MCPServerConfig{{Name: "m", Transport: "stdio", Command: []string{"true"}}}
			c.Roles = map[string]RoleConfig{"x": {Description: "d", Tools: []string{"read_file"}}}
		}, "", ""},
	}
	baseline := len(Default().Problems())
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			c := Default()
			tt.mutate(&c)
			ps := c.Problems()
			if tt.wantPath == "" {
				if len(ps) != baseline {
					t.Fatalf("problems = %+v, want baseline %d", ps, baseline)
				}
				return
			}
			if len(ps) != baseline+1 {
				t.Fatalf("problems = %+v, want exactly one new at %s", ps, tt.wantPath)
			}
			var found bool
			for _, p := range ps {
				if p.Path == tt.wantPath {
					found = true
					if !strings.Contains(p.Message, tt.wantMsg) {
						t.Errorf("message = %q, want it to contain %q", p.Message, tt.wantMsg)
					}
				}
			}
			if !found {
				t.Errorf("problems = %+v, want one at %s", ps, tt.wantPath)
			}
		})
	}
}
