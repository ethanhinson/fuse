package permissions

import (
	"context"
	"testing"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/tools"
)

// stubTool is a minimal tools.Tool for testing.
type stubTool struct{ name string }

func (s stubTool) Name() string              { return s.name }
func (s stubTool) Description() string       { return "stub" }
func (s stubTool) Parameters() map[string]any { return map[string]any{"type": "object"} }
func (s stubTool) Execute(_ context.Context, _ string) tools.Result {
	return tools.Result{Output: "ran " + s.name}
}

func newTestRegistry(names ...string) *tools.Registry {
	r := tools.NewRegistry()
	for _, n := range names {
		r.Register(stubTool{name: n})
	}
	return r
}

func TestModeOff_AlwaysAutoApproves(t *testing.T) {
	prompted := false
	approve := func(_ context.Context, _ ApprovalRequest) (bool, bool, error) {
		prompted = true
		return true, false, nil
	}
	cfg := config.PermissionsConfig{Mode: "off"}
	g := New(cfg, newTestRegistry("bash"), approve)
	res := g.Execute(context.Background(), "bash", `{"command":"rm -rf /"}`)
	if res.IsError {
		t.Fatalf("expected success in off mode, got: %s", res.Output)
	}
	if prompted {
		t.Fatal("approval func should not be called in off mode")
	}
}

func TestSafeListAutoApproves(t *testing.T) {
	prompted := false
	approve := func(_ context.Context, _ ApprovalRequest) (bool, bool, error) {
		prompted = true
		return false, false, nil
	}
	cfg := config.PermissionsConfig{Mode: "smart"}
	g := New(cfg, newTestRegistry("read_file", "list_directory", "codeindex_callers"), approve)

	for _, name := range []string{"read_file", "list_directory", "codeindex_callers"} {
		res := g.Execute(context.Background(), name, `{}`)
		if res.IsError {
			t.Errorf("safe tool %q returned error: %s", name, res.Output)
		}
	}
	if prompted {
		t.Fatal("approval func should not be called for safe-list tools")
	}
}

func TestAlwaysPromptDemotesSafeList(t *testing.T) {
	prompted := false
	approve := func(_ context.Context, _ ApprovalRequest) (bool, bool, error) {
		prompted = true
		return true, false, nil
	}
	cfg := config.PermissionsConfig{
		Mode:         "smart",
		AlwaysPrompt: []string{"read_file"},
	}
	g := New(cfg, newTestRegistry("read_file"), approve)
	g.Execute(context.Background(), "read_file", `{}`)
	if !prompted {
		t.Fatal("always_prompt should have triggered approval for read_file")
	}
}

func TestAutoApprovePatternPromotes(t *testing.T) {
	prompted := false
	approve := func(_ context.Context, _ ApprovalRequest) (bool, bool, error) {
		prompted = true
		return false, false, nil
	}
	cfg := config.PermissionsConfig{
		Mode:        "smart",
		AutoApprove: []string{"bash:go"},
	}
	g := New(cfg, newTestRegistry("bash"), approve)
	res := g.Execute(context.Background(), "bash", `{"command":"go test ./..."}`)
	if res.IsError {
		t.Fatalf("auto_approve pattern should have approved: %s", res.Output)
	}
	if prompted {
		t.Fatal("should not have prompted for auto_approve pattern")
	}
}

func TestDisabledToolReturnsError(t *testing.T) {
	cfg := config.PermissionsConfig{
		Mode:     "smart",
		Disabled: []string{"bash"},
	}
	g := New(cfg, newTestRegistry("bash"), AlwaysApprove)
	res := g.Execute(context.Background(), "bash", `{}`)
	if !res.IsError {
		t.Fatal("disabled tool should return error result")
	}
}

func TestDenialReturnsError(t *testing.T) {
	approve := func(_ context.Context, _ ApprovalRequest) (bool, bool, error) {
		return false, false, nil
	}
	cfg := config.PermissionsConfig{Mode: "smart"}
	g := New(cfg, newTestRegistry("bash"), approve)
	res := g.Execute(context.Background(), "bash", `{"command":"rm -rf /tmp/x"}`)
	if !res.IsError {
		t.Fatal("denied tool call should return error result")
	}
}

func TestSessionCacheSkipsSubsequentPrompts(t *testing.T) {
	calls := 0
	approve := func(_ context.Context, _ ApprovalRequest) (bool, bool, error) {
		calls++
		return true, true, nil // allow for session
	}
	cfg := config.PermissionsConfig{Mode: "smart"}
	g := New(cfg, newTestRegistry("bash"), approve)

	args := `{"command":"rm -rf /tmp/build"}`
	for i := 0; i < 3; i++ {
		res := g.Execute(context.Background(), "bash", args)
		if res.IsError {
			t.Fatalf("call %d failed: %s", i, res.Output)
		}
	}
	if calls != 1 {
		t.Fatalf("expected exactly 1 prompt, got %d", calls)
	}
}

func TestPromptAll_PromptsEvenSafeTools(t *testing.T) {
	prompted := false
	approve := func(_ context.Context, _ ApprovalRequest) (bool, bool, error) {
		prompted = true
		return true, false, nil
	}
	cfg := config.PermissionsConfig{Mode: "prompt-all"}
	g := New(cfg, newTestRegistry("read_file"), approve)
	g.Execute(context.Background(), "read_file", `{}`)
	if !prompted {
		t.Fatal("prompt-all mode should prompt even for safe-list tools")
	}
}
