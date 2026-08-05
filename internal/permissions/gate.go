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

	// classifier is the auto-mode probabilistic layer. It is nil outside auto
	// mode and nil in auto mode when no classifier was injected — a nil
	// classifier makes the residual gray area fail closed to a human ask.
	classifier *Classifier
	// workspaceRoot is the pre-canonicalized (filepath.EvalSymlinks) workspace
	// directory used by the auto-mode heuristic for path scoping. The gate
	// canonicalizes once at construction and passes it to classifyHeuristic.
	workspaceRoot string
}

// Option configures optional auto-mode dependencies on a PermissionGate. The
// three-argument New(...) form stays behaviour-identical for smart/off/
// prompt-all; auto-mode wiring is purely additive via options.
type Option func(*PermissionGate)

// WithClassifier injects the auto-mode classifier. Passing nil (or omitting the
// option) leaves the gate's residual gray area to fail closed to a human ask.
func WithClassifier(c *Classifier) Option {
	return func(g *PermissionGate) { g.classifier = c }
}

// WithWorkspaceRoot sets the pre-canonicalized workspace root used for auto-mode
// path scoping. The caller MUST canonicalize with filepath.EvalSymlinks before
// passing it (the heuristic compares against the real, symlink-resolved path).
func WithWorkspaceRoot(root string) Option {
	return func(g *PermissionGate) { g.workspaceRoot = root }
}

// New builds a PermissionGate. approve is called when user input is needed;
// pass AlwaysApprove for non-interactive (one-shot) sessions. Auto-mode
// dependencies (a classifier, a workspace root) are supplied additively via
// opts so existing three-argument callers compile and behave unchanged.
func New(cfg config.PermissionsConfig, inner *tools.Registry, approve ApprovalFunc, opts ...Option) *PermissionGate {
	g := &PermissionGate{
		mode:    ParseMode(cfg.Mode),
		cfg:     cfg,
		cache:   newApprovalCache(),
		approve: approve,
		inner:   inner,
	}
	for _, opt := range opts {
		opt(g)
	}
	return g
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
		msg := "tool call denied by user"
		if policy.DenyReason != "" {
			msg = policy.DenyReason
		}
		return tools.Result{IsError: true, Output: msg}
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

	if g.mode == ModeAuto {
		verdict, reason := g.resolveAuto(ctx, name, args)
		switch verdict {
		case VerdictAllow:
			policy.AutoApprove = true
			return policy, nil
		case VerdictDeny:
			// A deny is not an error: return a non-auto-approve policy carrying
			// the layer-named reason so the model can retry with a different call.
			return ToolPolicy{Enabled: true, AutoApprove: false, DenyReason: reason}, nil
		default:
			// VerdictAsk ⇒ fall through to the shared human-approval block below.
		}
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

// resolveAuto runs the auto-mode pipeline and returns a Verdict plus, on a
// deny, a layer-named denial reason (empty otherwise). Layer order:
//
//  1. Split segments (bash only). ErrUnparseable ⇒ ask (fail toward the human;
//     an unparseable command is not routed to the classifier).
//  2. evalRules: Deny ⇒ deny (terminal); Allow ⇒ allow; Ask ⇒ continue.
//  3. allSegmentsReadOnlySafe ⇒ allow.
//  4. classifyHeuristic: Deny ⇒ deny; Allow ⇒ allow; Ask ⇒ continue.
//  5. classifier (if wired): its verdict is final. If nil ⇒ ask (fail closed).
//
// Non-bash tools carry no shell command: an onSafeList tool ⇒ allow; otherwise
// route to the classifier with the raw args as the command (or ask if none).
func (g *PermissionGate) resolveAuto(ctx context.Context, name, args string) (Verdict, string) {
	if name != "bash" {
		if onSafeList(name) {
			return VerdictAllow, ""
		}
		return g.classifyOrAsk(ctx, name, args)
	}

	command := bashCommand(args)

	// 1. Static floor: parse into segments. Unparseable fails toward the human.
	segments, err := splitSegments(command)
	if err != nil {
		return VerdictAsk, ""
	}

	// 2. Deterministic rules. Deny is terminal; allow is a positive auto-approve.
	switch evalRules(segments, g.cfg.Auto, g.cfg.AutoApprove, g.cfg.AlwaysPrompt) {
	case VerdictDeny:
		return VerdictDeny, "denied by auto-mode rules layer: " + command
	case VerdictAllow:
		return VerdictAllow, ""
	}

	// 3. Read-only safe list short-circuit.
	if allSegmentsReadOnlySafe(segments) {
		return VerdictAllow, ""
	}

	// 4. Heuristics: egress boundary / path scoping. Ask ⇒ continue to classifier.
	switch classifyHeuristic(segments, g.workspaceRoot) {
	case VerdictDeny:
		return VerdictDeny, "denied by auto-mode heuristic layer: " + command
	case VerdictAllow:
		return VerdictAllow, ""
	}

	// 5. Classifier (final) or fail-closed ask.
	return g.classifyOrAsk(ctx, name, command)
}

// classifyOrAsk consults the injected classifier for a final verdict, or fails
// closed to VerdictAsk when no classifier is wired. A classifier deny carries a
// layer-named reason.
func (g *PermissionGate) classifyOrAsk(ctx context.Context, name, command string) (Verdict, string) {
	if g.classifier == nil {
		return VerdictAsk, ""
	}
	// User-message history is not plumbed into the gate for Task 7; the
	// classifier still functions from the pending-call turn alone.
	switch g.classifier.Classify(ctx, nil, name, command) {
	case VerdictAllow:
		return VerdictAllow, ""
	case VerdictDeny:
		return VerdictDeny, "denied by auto-mode classifier: " + command
	default:
		return VerdictAsk, ""
	}
}

// bashCommand extracts the shell command string from a bash tool's JSON args,
// mirroring makePreview. A non-bash or unparseable args string yields "", which
// splitSegments treats as an empty (unresolved) command.
func bashCommand(args string) string {
	var v struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal([]byte(args), &v); err == nil {
		return v.Command
	}
	return ""
}

// CloneForChild returns a child permission gate seeded from a snapshot of this
// gate's approval cache. The child's approval prompts will be prefixed [label].
// Child approvals do not propagate back to the parent gate.
func (g *PermissionGate) CloneForChild(label string) *PermissionGate {
	return &PermissionGate{
		mode:          g.mode,
		cfg:           g.cfg,
		cache:         g.cache.Clone(),
		approve:       prefixedApprove(label, g.approve),
		inner:         g.inner,
		classifier:    g.classifier.cloneForChild(),
		workspaceRoot: g.workspaceRoot,
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
