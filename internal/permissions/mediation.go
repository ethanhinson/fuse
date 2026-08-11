package permissions

import "context"

// TargetMediator is the per-call complete-mediation seam for tool/resource
// identity propagation (change #52, D5). It answers, for a tool call that may
// reach a downstream target, whether the call is permitted by the loop-start
// root of trust — the principal/tenant's target allowlist and scope ceilings,
// sourced from config / #49 grants, NEVER from model output.
//
// It is deliberately narrow and independent of internal/toolidentity and
// internal/mcp: the gate consults it by tool name + args, and the concrete
// mediator (wired at the composition root) owns the tool→target map and the
// ceilings. This keeps the permissions package free of an auth/mcp import while
// still enforcing complete mediation at the one per-tool-call choke point.
//
// A denial here is TERMINAL and independent of the permission mode: "the tool
// exists and its args are schema-valid" is NOT authorization (Saltzer &
// Schroeder). It runs before, and regardless of, the approve/mode logic — even
// ModeOff cannot bypass it — because it enforces authority, not user preference.
type TargetMediator interface {
	// MediateTarget reports whether a call to toolName with args is permitted by
	// the root-of-trust ceilings, and a denial reason when not. A tool that
	// reaches no downstream target (an ordinary in-process tool) is permitted.
	MediateTarget(ctx context.Context, toolName, args string) (allowed bool, reason string)
}

// WithTargetMediator wires a complete-mediation TargetMediator into the gate. A
// gate built without one behaves exactly as before (no mediation in the path).
func WithTargetMediator(m TargetMediator) Option {
	return func(g *PermissionGate) { g.mediator = m }
}
