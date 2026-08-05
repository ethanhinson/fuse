package permissions

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/tools"
)

// ApprovalRequest describes a tool call awaiting user approval.
type ApprovalRequest struct {
	ToolName string
	Args     string
	Preview  string // human-readable one-liner shown in the TUI prompt
}

// ApprovalFunc blocks until the user decides. Returns (approved,
// allowForSession, error). A cancelled context returns an error.
type ApprovalFunc func(ctx context.Context, req ApprovalRequest) (approved bool, allowForSession bool, err error)

// AlwaysApprove is an ApprovalFunc for non-interactive (one-shot) mode.
func AlwaysApprove(_ context.Context, _ ApprovalRequest) (bool, bool, error) {
	return true, false, nil
}

// PermissionGate wraps a tool registry and applies HITL approval before every
// Execute call. It satisfies agent.ToolExecutor so it can be passed directly
// to agent.New in place of the raw registry.
type PermissionGate struct {
	mode    PermissionMode
	cfg     config.PermissionsConfig
	cache   *ApprovalCache
	approve ApprovalFunc
	inner   *tools.Registry
}

// New builds a PermissionGate. approve is called when user input is needed;
// pass AlwaysApprove for non-interactive (one-shot) sessions.
func New(cfg config.PermissionsConfig, inner *tools.Registry, approve ApprovalFunc) *PermissionGate {
	return &PermissionGate{
		mode:    ParseMode(cfg.Mode),
		cfg:     cfg,
		cache:   newApprovalCache(),
		approve: approve,
		inner:   inner,
	}
}

// Schemas delegates to the inner registry so agent.New receives the full
// schema list.
func (g *PermissionGate) Schemas() []model.ToolSchema { return g.inner.Schemas() }

// Execute resolves the merged ToolPolicy and either auto-approves, prompts,
// or returns a denial/error result.
func (g *PermissionGate) Execute(ctx context.Context, name, args string) tools.Result {
	policy, err := g.resolve(ctx, name, args)
	if err != nil {
		return tools.Result{IsError: true, Output: fmt.Sprintf("approval cancelled: %v", err)}
	}
	if !policy.Enabled {
		return tools.Result{IsError: true, Output: fmt.Sprintf("tool %q is disabled", name)}
	}
	if !policy.AutoApprove {
		return tools.Result{IsError: true, Output: "tool call denied by user"}
	}
	return g.inner.Execute(ctx, name, args)
}

// resolve applies the 3-source policy merge and returns the final ToolPolicy.
func (g *PermissionGate) resolve(ctx context.Context, name, args string) (ToolPolicy, error) {
	// Disabled list overrides everything.
	for _, d := range g.cfg.Disabled {
		if d == name {
			return ToolPolicy{Enabled: false}, nil
		}
	}

	policy := ToolPolicy{Enabled: true}

	if g.mode == ModeOff {
		policy.AutoApprove = true
		return policy, nil
	}

	// Session cache — highest precedence, covers both smart and prompt-all.
	if g.cache.Check(name, args) {
		policy.AutoApprove = true
		return policy, nil
	}

	if g.mode == ModeSmart {
		// always_prompt demotes even safe-list entries — check first.
		if !matchesAny(g.cfg.AlwaysPrompt, name, args) {
			// auto_approve config patterns promote beyond the safe list.
			if matchesAny(g.cfg.AutoApprove, name, args) || onSafeList(name) {
				policy.AutoApprove = true
				return policy, nil
			}
		}
	}

	// Human approval required.
	req := ApprovalRequest{
		ToolName: name,
		Args:     args,
		Preview:  makePreview(name, args),
	}
	approved, allowSession, err := g.approve(ctx, req)
	if err != nil {
		return ToolPolicy{Enabled: true}, err
	}
	if !approved {
		return ToolPolicy{Enabled: true, AutoApprove: false}, nil
	}
	if allowSession {
		g.cache.Allow(name, args)
	}
	policy.AutoApprove = true
	return policy, nil
}

// CloneForChild returns a child permission gate seeded from a snapshot of this
// gate's approval cache. The child's approval prompts will be prefixed [label].
// Child approvals do not propagate back to the parent gate.
func (g *PermissionGate) CloneForChild(label string) *PermissionGate {
	return &PermissionGate{
		mode:    g.mode,
		cfg:     g.cfg,
		cache:   g.cache.Clone(),
		approve: prefixedApprove(label, g.approve),
		inner:   g.inner,
	}
}

// prefixedApprove wraps an ApprovalFunc so that prompts are prefixed with
// [label] to identify which child agent is asking.
func prefixedApprove(label string, fn ApprovalFunc) ApprovalFunc {
	return func(ctx context.Context, req ApprovalRequest) (bool, bool, error) {
		req.Preview = "[" + label + "] " + req.Preview
		return fn(ctx, req)
	}
}

// makePreview builds the human-readable one-liner for the approval block.
func makePreview(name, args string) string {
	if name == "bash" {
		var v struct {
			Command string `json:"command"`
		}
		if err := json.Unmarshal([]byte(args), &v); err == nil {
			cmd := strings.TrimSpace(v.Command)
			if len(cmd) > 80 {
				cmd = cmd[:80] + "…"
			}
			return "bash: " + cmd
		}
	}
	if len(args) > 80 {
		return name + ": " + args[:80] + "…"
	}
	return name + ": " + args
}
