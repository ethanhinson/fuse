package permissions

import (
	"context"
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/internal/config"
)

// fakeMediator denies any tool whose name is in deny, allows the rest.
type fakeMediator struct{ deny map[string]string }

func (m fakeMediator) MediateTarget(_ context.Context, toolName, _ string) (bool, string) {
	if reason, blocked := m.deny[toolName]; blocked {
		return false, reason
	}
	return true, ""
}

// A tool call to a denied/undeclared target is blocked at the gate even in
// ModeOff (which auto-approves everything) — complete mediation runs BEFORE and
// independent of the approval/mode logic. The tool never executes (its "ran …"
// output never appears).
func TestMediator_DeniesUndeclaredTargetEvenInModeOff(t *testing.T) {
	gate := New(
		config.PermissionsConfig{Mode: "off"},
		newTestRegistry("mcp:srv/do"),
		AlwaysApprove,
		WithTargetMediator(fakeMediator{deny: map[string]string{"mcp:srv/do": "target not on allowlist"}}),
	)

	res := gate.Execute(context.Background(), "mcp:srv/do", `{}`)
	if !res.IsError {
		t.Fatal("a mediation-denied tool call must return an error result")
	}
	if !strings.Contains(res.Output, "not on allowlist") {
		t.Fatalf("denial reason not surfaced: %q", res.Output)
	}
	if strings.Contains(res.Output, "ran ") {
		t.Fatal("a mediation-denied tool must NEVER execute (complete mediation)")
	}
}

// An allowlisted target passes mediation and then follows the ordinary mode path
// (ModeOff → auto-approve → execute).
func TestMediator_AllowsDeclaredTarget(t *testing.T) {
	gate := New(
		config.PermissionsConfig{Mode: "off"},
		newTestRegistry("mcp:srv/do"),
		AlwaysApprove,
		WithTargetMediator(fakeMediator{deny: map[string]string{}}),
	)

	res := gate.Execute(context.Background(), "mcp:srv/do", `{}`)
	if res.IsError {
		t.Fatalf("an allowlisted target must not be blocked: %q", res.Output)
	}
	if !strings.Contains(res.Output, "ran mcp:srv/do") {
		t.Fatalf("an allowed tool should execute; got %q", res.Output)
	}
}

// With no mediator wired, behavior is byte-identical to today — a non-MCP tool
// runs under ModeOff with no mediation gate in the path.
func TestMediator_NilMediatorUnchanged(t *testing.T) {
	gate := New(config.PermissionsConfig{Mode: "off"}, newTestRegistry("read_file"), AlwaysApprove)

	res := gate.Execute(context.Background(), "read_file", `{}`)
	if res.IsError || !strings.Contains(res.Output, "ran read_file") {
		t.Fatalf("no-mediator path must be unchanged; err=%v out=%q", res.IsError, res.Output)
	}
}

// A child gate inherits the parent's complete-mediation ceiling — a denied
// target stays denied for a spawned child agent.
func TestMediator_InheritedByChild(t *testing.T) {
	parent := New(
		config.PermissionsConfig{Mode: "off"},
		newTestRegistry("mcp:srv/do"),
		AlwaysApprove,
		WithTargetMediator(fakeMediator{deny: map[string]string{"mcp:srv/do": "denied"}}),
	)
	child := parent.CloneForChild("child")
	res := child.Execute(context.Background(), "mcp:srv/do", `{}`)
	if !res.IsError || strings.Contains(res.Output, "ran ") {
		t.Fatalf("child must inherit the mediation ceiling; err=%v out=%q", res.IsError, res.Output)
	}
}
