package harness_test

import (
	"context"
	"net/http"
	"net/http/httptest"
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
