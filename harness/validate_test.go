package harness_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"

	"github.com/ethanhinson/fuse/harness"
	harnessv1 "github.com/ethanhinson/fuse/harness/v1"
	"github.com/ethanhinson/fuse/harness/v1/harnessv1connect"
)

func ptr[T any](v T) *T { return &v }

// byPath indexes violations by field path, failing on a duplicate path (the
// validator reports each field at most once).
func byPath(t *testing.T, vs []harness.Violation) map[string]harness.Violation {
	t.Helper()
	out := map[string]harness.Violation{}
	for _, v := range vs {
		if _, dup := out[v.FieldPath]; dup {
			t.Fatalf("duplicate violation for %q: %+v", v.FieldPath, vs)
		}
		out[v.FieldPath] = v
	}
	return out
}

func TestValidateEmptyAndNilAreValid(t *testing.T) {
	if vs := harness.Validate(nil); len(vs) != 0 {
		t.Fatalf("Validate(nil) = %+v", vs)
	}
	if vs := harness.Validate(&harnessv1.HarnessSpec{}); len(vs) != 0 {
		t.Fatalf("Validate(empty) = %+v", vs)
	}
}

func TestValidateRealisticSpecIsValid(t *testing.T) {
	spec := &harnessv1.HarnessSpec{
		Models: &harnessv1.Models{
			DefaultAlias: "fast",
			Entries:      map[string]*harnessv1.Model{"fast": {Id: "cloud/fast-1", MaxTokens: 4096}},
		},
		Skills: []*harnessv1.SkillRef{{Name: "research"}},
		McpServers: []*harnessv1.McpServer{{
			Name: "rentals", Transport: harnessv1.McpTransport_MCP_TRANSPORT_STREAMABLE_HTTP, Url: "https://mcp.example.com/mcp",
			Auth: &harnessv1.McpAuth{Type: harnessv1.McpAuthType_MCP_AUTH_TYPE_BEARER, ClientSecretRef: "rentals/token"},
		}},
		Permissions: &harnessv1.Permissions{Mode: harnessv1.PermissionMode_PERMISSION_MODE_AUTO, Auto: &harnessv1.AutoPermissions{ClassifierModel: "glm"}},
		Context:     &harnessv1.Context{Summarization: &harnessv1.Summarization{Threshold: 0.8, Model: "fast"}},
		MaxTurns:    ptr(uint32(0)),
	}
	if vs := harness.Validate(spec); len(vs) != 0 {
		t.Fatalf("Validate = %+v", vs)
	}
}

// TestValidateShapeRules covers representative protovalidate (shape) rules: the
// ones every language enforces from the same descriptors.
func TestValidateShapeRules(t *testing.T) {
	spec := &harnessv1.HarnessSpec{
		McpServers: []*harnessv1.McpServer{
			{Name: "a", Transport: harnessv1.McpTransport_MCP_TRANSPORT_HTTP, Url: "not a url"},
			{Name: "a", Url: "https://ok.example.com"}, // duplicate name, missing transport
		},
		Skills:  []*harnessv1.SkillRef{{Name: "Bad Name"}},
		Context: &harnessv1.Context{Summarization: &harnessv1.Summarization{Threshold: 1.5}},
		Research: &harnessv1.Research{Custom: &harnessv1.CustomSearchProvider{
			Headers: map[string]*harnessv1.HeaderValue{"X": {}}, // oneof unset
		}},
		Permissions: &harnessv1.Permissions{Mode: harnessv1.PermissionMode(42)},
	}
	got := byPath(t, harness.Validate(spec))
	want := map[string]string{
		"mcp_servers[0].url":                 "string.uri",
		"mcp_servers[1].transport":           "enum.not_in",
		"mcp_servers":                        "fuse.mcp_servers.unique_names",
		"skills[0].name":                     "string.pattern",
		"context.summarization.threshold":    "double.gte_lte",
		`research.custom.headers["X"].value`: "required",
		"permissions.mode":                   "enum.defined_only",
	}
	for path, rule := range want {
		v, ok := got[path]
		if !ok {
			t.Errorf("missing violation at %q (got %+v)", path, got)
			continue
		}
		if v.RuleID != rule {
			t.Errorf("%s: rule %q, want %q", path, v.RuleID, rule)
		}
		if v.Message == "" {
			t.Errorf("%s: empty message", path)
		}
	}
	if len(got) != len(want) {
		t.Errorf("got %d violations, want %d: %+v", len(got), len(want), got)
	}
}

// TestValidateSemanticRules covers rules that only internal/config knows: they
// arrive through the mapping, with spec field paths.
func TestValidateSemanticRules(t *testing.T) {
	spec := &harnessv1.HarnessSpec{
		Models:       &harnessv1.Models{DefaultAlias: "ghost"},
		Orchestrator: &harnessv1.Orchestrator{Model: "phantom"},
		Context: &harnessv1.Context{Relevance: &harnessv1.Relevance{
			BorderlineLo: 0.9, BorderlineHi: 0.2, // each in range, but out of order
		}},
	}
	got := byPath(t, harness.Validate(spec))
	want := map[string]string{
		"models.default_alias":            "fuse.model_ref",
		"orchestrator.model":              "fuse.model_ref",
		"context.relevance.borderline_lo": "fuse.config",
		"context.relevance.borderline_hi": "fuse.config",
	}
	for path, rule := range want {
		v, ok := got[path]
		if !ok {
			t.Errorf("missing violation at %q (got %+v)", path, got)
			continue
		}
		if v.RuleID != rule {
			t.Errorf("%s: rule %q, want %q", path, v.RuleID, rule)
		}
	}
	if len(got) != len(want) {
		t.Errorf("got %+v", got)
	}
	if got["models.default_alias"].Message != `references unknown model "ghost"` {
		t.Errorf("message = %q", got["models.default_alias"].Message)
	}
}

// TestValidateDedupesShapeAndSemantic: when both layers flag the same field the
// shape violation wins and the field is reported once.
func TestValidateDedupesShapeAndSemantic(t *testing.T) {
	spec := &harnessv1.HarnessSpec{Context: &harnessv1.Context{Relevance: &harnessv1.Relevance{RecencyFloorPct: 150}}}
	got := byPath(t, harness.Validate(spec))
	if len(got) != 1 || got["context.relevance.recency_floor_pct"].RuleID != "uint32.lte" {
		t.Fatalf("got %+v", got)
	}
}

// TestValidateModelAliasDefinedInSpec: an alias the spec itself defines
// resolves, as does a built-in one.
func TestValidateModelAliasDefinedInSpec(t *testing.T) {
	spec := &harnessv1.HarnessSpec{
		Models:  &harnessv1.Models{Entries: map[string]*harnessv1.Model{"mine": {Id: "cloud/m"}}},
		Context: &harnessv1.Context{Summarization: &harnessv1.Summarization{Model: "mine"}, Relevance: &harnessv1.Relevance{ClassifierModel: "glm"}},
	}
	if vs := harness.Validate(spec); len(vs) != 0 {
		t.Fatalf("Validate = %+v", vs)
	}
}

func TestValidateIsDeterministic(t *testing.T) {
	spec := &harnessv1.HarnessSpec{
		Workflows: map[string]*harnessv1.Workflow{"A": {}, "B": {}, "C": {}},
		Models:    &harnessv1.Models{DefaultAlias: "ghost"},
	}
	first := harness.Validate(spec)
	for i := 0; i < 20; i++ {
		again := harness.Validate(spec)
		if len(again) != len(first) {
			t.Fatal("length changed")
		}
		for j := range first {
			if again[j] != first[j] {
				t.Fatalf("order changed: %+v vs %+v", first, again)
			}
		}
	}
}

// TestServiceServesValidateHarness drives the HarnessService handler over a
// real Connect round trip.
func TestServiceServesValidateHarness(t *testing.T) {
	mux := http.NewServeMux()
	mux.Handle(harnessv1connect.NewHarnessServiceHandler(harness.Service{}))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := harnessv1connect.NewHarnessServiceClient(http.DefaultClient, srv.URL)
	resp, err := client.ValidateHarness(context.Background(), connect.NewRequest(&harnessv1.ValidateHarnessRequest{
		Spec: &harnessv1.HarnessSpec{Models: &harnessv1.Models{DefaultAlias: "ghost"}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	vs := resp.Msg.GetViolations()
	if len(vs) != 1 || vs[0].GetFieldPath() != "models.default_alias" || vs[0].GetRuleId() != "fuse.model_ref" {
		t.Fatalf("violations = %v", vs)
	}
}

// TestValidate_WorkerWithoutTools: a worker's allowlist is its whole tool set,
// so an empty one is refused once (shape wins over the semantic duplicate).
func TestValidate_WorkerWithoutTools(t *testing.T) {
	bad := &harnessv1.HarnessSpec{Workflows: map[string]*harnessv1.Workflow{
		"wf": {Workers: map[string]*harnessv1.Worker{"reader": {}}},
	}}
	const path = `workflows["wf"].workers["reader"].tools`
	for name, vs := range map[string][]harness.Violation{
		"Validate":      harness.Validate(bad),
		"ValidateShape": harness.ValidateShape(bad),
	} {
		if len(vs) != 1 || vs[0].FieldPath != path || vs[0].RuleID != "repeated.min_items" {
			t.Errorf("%s = %+v, want one repeated.min_items at %s", name, vs, path)
		}
	}
	ok := &harnessv1.HarnessSpec{Workflows: map[string]*harnessv1.Workflow{
		"wf": {Workers: map[string]*harnessv1.Worker{"reader": {Tools: []string{"read_file"}}}},
	}}
	if vs := harness.Validate(ok); len(vs) != 0 {
		t.Errorf("Validate(tooled) = %+v", vs)
	}
	if vs := harness.ValidateShape(ok); len(vs) != 0 {
		t.Errorf("ValidateShape(tooled) = %+v", vs)
	}
}

// TestValidateInstructionsLength pins the 16384-character limit on the task
// brief: it counts characters, not bytes. The config check flags the same
// path, so an over-long brief also proves the shape violation wins and the
// field is reported once.
func TestValidateInstructionsLength(t *testing.T) {
	for name, text := range map[string]string{
		"ascii at limit":     strings.Repeat("a", 16384),
		"multibyte at limit": strings.Repeat("\u00e9", 16384), // 16384 characters, 32768 bytes
	} {
		if vs := harness.Validate(&harnessv1.HarnessSpec{Instructions: text}); len(vs) != 0 {
			t.Errorf("%s: unexpected violations %+v", name, vs)
		}
	}
	got := byPath(t, harness.Validate(&harnessv1.HarnessSpec{Instructions: strings.Repeat("a", 16385)}))
	if len(got) != 1 {
		t.Fatalf("got %d violations, want 1: %+v", len(got), got)
	}
	v, ok := got["instructions"]
	if !ok {
		t.Fatalf("no violation at instructions: %+v", got)
	}
	if v.RuleID != "string.max_len" {
		t.Errorf("rule = %q, want string.max_len", v.RuleID)
	}
	if v.Message == "" {
		t.Error("empty message")
	}
}

// TestValidateShapeRoles pins the shape rules on named roles: a required
// description of at most 512 characters, instructions of at most 16384
// characters, at most 32 roles under the workflow key pattern, and non-empty
// tool names (an empty tool list inherits and is valid).
func TestValidateShapeRoles(t *testing.T) {
	type want struct{ path, rule string }
	manyRoles := func(n int) map[string]*harnessv1.Role {
		out := map[string]*harnessv1.Role{}
		for i := 0; i < n; i++ {
			out[fmt.Sprintf("r%02d", i)] = &harnessv1.Role{Description: "d"}
		}
		return out
	}
	one := func(r *harnessv1.Role) map[string]*harnessv1.Role { return map[string]*harnessv1.Role{"a": r} }
	cases := []struct {
		name  string
		roles map[string]*harnessv1.Role
		want  []want
	}{
		{"missing description", one(&harnessv1.Role{}), []want{{`roles["a"].description`, "string.min_len"}}},
		{"description too long", one(&harnessv1.Role{Description: strings.Repeat("a", 513)}), []want{{`roles["a"].description`, "string.max_len"}}},
		{"multibyte description at limit", one(&harnessv1.Role{Description: strings.Repeat("é", 512)}), nil},
		{"instructions too long", one(&harnessv1.Role{Description: "d", Instructions: strings.Repeat("a", 16385)}), []want{{`roles["a"].instructions`, "string.max_len"}}},
		{"multibyte instructions at limit", one(&harnessv1.Role{Description: "d", Instructions: strings.Repeat("é", 16384)}), nil},
		{"too many roles", manyRoles(33), []want{{"roles", "map.max_pairs"}}},
		{"roles at limit", manyRoles(32), nil},
		{"bad key", map[string]*harnessv1.Role{"Bad Key": {Description: "d"}}, []want{{`roles["Bad Key"]`, "string.pattern"}}},
		{"empty tool name", one(&harnessv1.Role{Description: "d", Tools: []string{""}}), []want{{`roles["a"].tools[0]`, "string.min_len"}}},
		{"no tools inherits", one(&harnessv1.Role{Description: "d", Tools: nil}), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := byPath(t, harness.ValidateShape(&harnessv1.HarnessSpec{Roles: tc.roles}))
			if len(got) != len(tc.want) {
				t.Fatalf("got %d violations, want %d: %+v", len(got), len(tc.want), got)
			}
			for _, w := range tc.want {
				v, ok := got[w.path]
				if !ok {
					t.Fatalf("no violation at %s: %+v", w.path, got)
				}
				if v.RuleID != w.rule {
					t.Errorf("%s rule = %q, want %q", w.path, v.RuleID, w.rule)
				}
			}
		})
	}
}

// TestValidateRolesAccepted checks that a spec with well-formed roles, and a
// workflow naming a defined role, validates cleanly end to end.
func TestValidateRolesAccepted(t *testing.T) {
	spec := &harnessv1.HarnessSpec{
		Models: &harnessv1.Models{Entries: map[string]*harnessv1.Model{"fast": {Id: "cloud/fast-1", MaxTokens: 4096}}},
		Roles: map[string]*harnessv1.Role{
			"reviewer": {
				Description:  "Reviews a change for correctness.",
				Instructions: "Read the diff and report problems.",
				Tools:        []string{"read_file"},
				Model:        "fast",
			},
			"researcher": {Description: "Looks things up."},
		},
		Workflows: map[string]*harnessv1.Workflow{"review": {Skill: "review", Roles: []string{"reviewer"}}},
	}
	if vs := harness.Validate(spec); len(vs) != 0 {
		t.Fatalf("Validate = %+v", vs)
	}
}

// TestValidateRolesRejected checks that each bad role field is reported exactly
// once, at its spec field path, through the full shape plus semantic verdict.
func TestValidateRolesRejected(t *testing.T) {
	manyRoles := func(n int) map[string]*harnessv1.Role {
		out := map[string]*harnessv1.Role{}
		for i := 0; i < n; i++ {
			out[fmt.Sprintf("r%02d", i)] = &harnessv1.Role{Description: "d"}
		}
		return out
	}
	one := func(r *harnessv1.Role) map[string]*harnessv1.Role { return map[string]*harnessv1.Role{"a": r} }
	cases := []struct {
		name      string
		spec      *harnessv1.HarnessSpec
		path      string
		rule      string
		msgSubstr string
	}{
		{"missing description", &harnessv1.HarnessSpec{Roles: one(&harnessv1.Role{})}, `roles["a"].description`, "string.min_len", ""},
		{"whitespace-only description", &harnessv1.HarnessSpec{Roles: one(&harnessv1.Role{Description: "   "})}, `roles["a"].description`, harness.RuleConfig, "required"},
		{"description too long", &harnessv1.HarnessSpec{Roles: one(&harnessv1.Role{Description: strings.Repeat("a", 513)})}, `roles["a"].description`, "string.max_len", ""},
		{"instructions too long", &harnessv1.HarnessSpec{Roles: one(&harnessv1.Role{Description: "d", Instructions: strings.Repeat("a", 16385)})}, `roles["a"].instructions`, "string.max_len", ""},
		{"too many roles", &harnessv1.HarnessSpec{Roles: manyRoles(33)}, "roles", "map.max_pairs", ""},
		{"bad key", &harnessv1.HarnessSpec{Roles: map[string]*harnessv1.Role{"Bad Key": {Description: "d"}}}, `roles["Bad Key"]`, "string.pattern", ""},
		{"unresolved workflow role", &harnessv1.HarnessSpec{
			Roles:     one(&harnessv1.Role{Description: "d"}),
			Workflows: map[string]*harnessv1.Workflow{"wf": {Skill: "review", Roles: []string{"a", "ghost"}}},
		}, `workflows["wf"].roles[1]`, harness.RuleConfig, "ghost"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := byPath(t, harness.Validate(tc.spec))
			if len(got) != 1 {
				t.Fatalf("got %d violations, want 1: %+v", len(got), got)
			}
			v, ok := got[tc.path]
			if !ok {
				t.Fatalf("no violation at %s: %+v", tc.path, got)
			}
			if v.RuleID != tc.rule {
				t.Errorf("%s rule = %q, want %q", tc.path, v.RuleID, tc.rule)
			}
			if !strings.Contains(v.Message, tc.msgSubstr) {
				t.Errorf("%s message = %q, want it to contain %q", tc.path, v.Message, tc.msgSubstr)
			}
		})
	}
}

// TestValidateRolesDoNotResolveToolsOrModels checks that a role's tools and
// model are carried as written: validation does not resolve them.
func TestValidateRolesDoNotResolveToolsOrModels(t *testing.T) {
	spec := &harnessv1.HarnessSpec{Roles: map[string]*harnessv1.Role{
		"a": {Description: "d", Tools: []string{"no_such_tool"}, Model: "no-such-alias"},
	}}
	if vs := harness.Validate(spec); len(vs) != 0 {
		t.Fatalf("Validate = %+v", vs)
	}
}

// TestValidateCarriedSkillSizeLimits pins the size limits on carried skills:
// a body of at most 32768 bytes, a description of at most 1024 characters and
// at most 64 skill entries.
func TestValidateCarriedSkillSizeLimits(t *testing.T) {
	carried := func(desc, body string) *harnessv1.HarnessSpec {
		return &harnessv1.HarnessSpec{Skills: []*harnessv1.SkillRef{{Name: "a", Description: desc, Body: body}}}
	}
	for name, spec := range map[string]*harnessv1.HarnessSpec{
		"body at limit":        carried("d", strings.Repeat("b", 32768)),
		"description at limit": carried(strings.Repeat("d", 1024), "b"),
	} {
		if vs := harness.Validate(spec); len(vs) != 0 {
			t.Errorf("%s: unexpected violations %+v", name, vs)
		}
	}
	many := make([]*harnessv1.SkillRef, 65)
	for i := range many {
		many[i] = &harnessv1.SkillRef{Name: fmt.Sprintf("s%d", i)}
	}
	for name, tc := range map[string]struct {
		spec       *harnessv1.HarnessSpec
		path, rule string
	}{
		"body over limit":        {carried("d", strings.Repeat("b", 32769)), "skills[0].body", "string.max_bytes"},
		"description over limit": {carried(strings.Repeat("d", 1025), "b"), "skills[0].description", "string.max_len"},
		"too many entries":       {&harnessv1.HarnessSpec{Skills: many}, "skills", "repeated.max_items"},
	} {
		got := byPath(t, harness.Validate(tc.spec))
		if len(got) != 1 {
			t.Errorf("%s: got %d violations, want 1: %+v", name, len(got), got)
			continue
		}
		v, ok := got[tc.path]
		if !ok {
			t.Errorf("%s: no violation at %s: %+v", name, tc.path, got)
			continue
		}
		if v.RuleID != tc.rule {
			t.Errorf("%s: rule = %q, want %q", name, v.RuleID, tc.rule)
		}
	}
}

func TestValidateReportsCarriedBytes(t *testing.T) {
	if harness.RuleSkillsCarriedBytes != "fuse.skills.carried_bytes" {
		t.Fatalf("RuleSkillsCarriedBytes = %q", harness.RuleSkillsCarriedBytes)
	}
	if harness.RuleSkillsCarriedRefused != "fuse.skills.carried_refused" {
		t.Fatalf("RuleSkillsCarriedRefused = %q", harness.RuleSkillsCarriedRefused)
	}
	// Shape-valid (9 <= 16 carried, each body <= 32768 bytes) but 270000
	// bytes carried in total, over the 262144-byte semantic limit.
	spec := &harnessv1.HarnessSpec{}
	body := strings.Repeat("x", 30000)
	for i := 0; i < 9; i++ {
		spec.Skills = append(spec.Skills, &harnessv1.SkillRef{
			Name:        fmt.Sprintf("s%d", i),
			Description: "a carried skill",
			Body:        body,
		})
	}
	vs := harness.Validate(spec)
	if len(vs) != 1 || vs[0].FieldPath != "skills" || vs[0].RuleID != harness.RuleSkillsCarriedBytes {
		t.Fatalf("Validate = %+v, want only skills %s", vs, harness.RuleSkillsCarriedBytes)
	}
}
