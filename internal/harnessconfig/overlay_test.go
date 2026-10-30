package harnessconfig

import (
	"reflect"
	"testing"

	harnessv1 "github.com/ethanhinson/fuse/harness/v1"
	"github.com/ethanhinson/fuse/internal/config"
)

func TestOverlayLayersSpecOverBase(t *testing.T) {
	base, err := config.FromYAML([]byte(`
models: {default: fast, fast: {id: cloud/fast}}
throughput: {requests_per_minute: 100}
skill_paths: [/srv/skills]
`), "operator")
	if err != nil {
		t.Fatal(err)
	}
	turns := uint32(2)
	spec := &harnessv1.HarnessSpec{
		Models:     &harnessv1.Models{DefaultAlias: "glm", Entries: map[string]*harnessv1.Model{"glm": {Id: "cloud/glm"}}},
		MaxTurns:   &turns,
		Throughput: &harnessv1.Throughput{RequestsPerMinute: 500, SessionTokens: 10},
		McpServers: []*harnessv1.McpServer{{
			Name: "s", Transport: harnessv1.McpTransport_MCP_TRANSPORT_HTTP, Url: "https://mcp.example.com",
			Auth: &harnessv1.McpAuth{Type: harnessv1.McpAuthType_MCP_AUTH_TYPE_BEARER, ClientSecretRef: "tok"},
		}},
	}
	got, refused, err := Overlay(base, spec, func(name string) (string, error) { return "value-of-" + name, nil })
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(refused, []string{"throughput.requests_per_minute", "throughput.session_tokens"}) {
		t.Errorf("refused = %v", refused)
	}
	if got.Models.Default != "glm" || got.Models.Entries["fast"].ID != "cloud/fast" || got.Models.Entries["glm"].ID != "cloud/glm" {
		t.Errorf("models = %+v", got.Models)
	}
	if got.MaxTurns == nil || *got.MaxTurns != 2 || got.Throughput.RequestsPerMinute != 100 {
		t.Errorf("max_turns/throughput = %v %+v", got.MaxTurns, got.Throughput)
	}
	if len(got.SkillPaths) != 1 || got.SkillPaths[0] != "/srv/skills" {
		t.Errorf("operator skill_paths lost: %v", got.SkillPaths)
	}
	if len(got.MCPServers) != 1 || got.MCPServers[0].Auth.ClientSecret != "value-of-tok" {
		t.Errorf("mcp = %+v", got.MCPServers)
	}
}

func TestSemanticProblemsUsesSpecPaths(t *testing.T) {
	cfg := config.Default()
	cfg.Models.Default = "ghost"
	cfg.Context.Summarization.Threshold = 2
	cfg.LoopServer.LeaseTTL = "nope"
	got := SemanticProblems(cfg)
	want := []Problem{
		{FieldPath: "context.summarization.threshold", Message: "must be in [0,1], got 2", RuleID: RuleConfig},
		{FieldPath: "", Message: `"nope" is not a valid duration (e.g. "30s", "2m")`, RuleID: RuleConfig},
		{FieldPath: "models.default_alias", Message: `references unknown model "ghost"`, RuleID: RuleModelRef},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SemanticProblems =\n %+v\nwant\n %+v", got, want)
	}
}

func TestCombineShapeWinsAndSorts(t *testing.T) {
	shape := []Problem{{FieldPath: "b", RuleID: "x"}, {FieldPath: "b", RuleID: "a"}}
	got := Combine(shape,
		[]Problem{{FieldPath: "b", RuleID: RuleConfig}, {FieldPath: "a", RuleID: RuleConfig}, {FieldPath: "", RuleID: RuleMapping}},
		[]Problem{{FieldPath: "a", RuleID: RuleModelRef}, {FieldPath: "", RuleID: RuleInternal}},
	)
	want := []Problem{
		{FieldPath: "", RuleID: RuleInternal}, {FieldPath: "", RuleID: RuleMapping},
		{FieldPath: "a", RuleID: RuleConfig},
		{FieldPath: "b", RuleID: "a"}, {FieldPath: "b", RuleID: "x"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Combine =\n %+v\nwant\n %+v", got, want)
	}
}
