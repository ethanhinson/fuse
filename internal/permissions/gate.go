package permissions

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/tools"
)

// LoopApprovalToolName is the sentinel ToolName carried by a doom-loop
// force-through ApprovalRequest (as opposed to a real tool call). The TUI keys
// on it to render the prompt as a loop check and to drop the "allow for
// session" option, whose bool is meaningless for a loop trip. See change 0038.
const LoopApprovalToolName = "possible loop"

// ValveApprovalToolName is the sentinel ToolName carried by the escalation
// valve's one-time recovery ApprovalRequest (change 0067): when the valve trips
// in an interactive gate, ONE prompt asks the human whether to continue in auto
// mode; approval resets the valve, rejection leaves per-call asks. The TUI keys
// on it like LoopApprovalToolName (no "allow for session" option).
const ValveApprovalToolName = "auto-mode escalation"

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
	// modeMu guards mode. The root gate's mode is switched live by SetMode from
	// the TUI goroutine while resolve calls read it concurrently, so every read
	// goes through currentMode() and every write through SetMode under this one
	// mutex — guarding only one side would still be a race
	// (mutex-test-double-concurrent-provider).
	modeMu sync.Mutex
	mode   PermissionMode
	// sessionMode, when non-nil, is the live source of truth for the gate's mode:
	// currentMode() returns sessionMode.Get() instead of the mode snapshot, so a
	// mid-turn switch bites the already-built gate (and its running children).
	// Holderless gates (one-shot, mcp-server, tests) leave this nil and resolve off
	// the mode snapshot exactly as before. It is shared by reference into children
	// via CloneForChild so a child follows the session mode live too.
	sessionMode *SessionMode
	// lastObservedMode is the mode currentMode() last returned, tracked under modeMu
	// so the live read path can detect an auto→non-auto transition (driven by the
	// holder, which SetMode never sees) and reset the escalation valve — preserving
	// SetMode's valve semantics across the live read seam.
	lastObservedMode PermissionMode
	cfg              config.PermissionsConfig
	cache            *ApprovalCache
	approve          ApprovalFunc
	inner            *tools.Registry

	// classifier is the auto-mode probabilistic layer. It is nil outside auto
	// mode and nil in auto mode when no classifier was injected — a nil
	// classifier makes the residual gray area fail closed to a human ask.
	classifier *Classifier
	// workspaceRoot is the pre-canonicalized (filepath.EvalSymlinks) workspace
	// directory used by the auto-mode heuristic for path scoping. The gate
	// canonicalizes once at construction and passes it to classifyHeuristic.
	workspaceRoot string

	// interactive marks whether a human is reachable through g.approve. When the
	// escalation valve trips, an interactive gate falls back to prompting; a
	// non-interactive gate aborts the run with a structured summary error. It is
	// false by default (the non-interactive posture) and set via WithInteractive.
	interactive bool

	// valve is the per-session escalation counter shared across CloneForChild: a
	// child's classifier blocks count toward the same session valve. It is nil
	// outside auto mode wiring only if never constructed; New always allocates it.
	valve *escalationValve

	// mediator, when wired (WithTargetMediator), is the complete-mediation seam
	// (change #52 D5): every tool call is checked against the principal/tenant
	// target allowlist + scope ceilings BEFORE any mode/approval logic, and a
	// denial there is terminal — even ModeOff cannot bypass it. Nil ⇒ no mediation
	// in the path (byte-identical to pre-#52 behavior).
	mediator TargetMediator
}

// escalationValve tracks classifier-layer block verdicts across a session and
// trips (pausing auto mode) at 3 consecutive OR 20 total blocks. It is shared by
// reference across CloneForChild so parent and child blocks accrue to the same
// session budget, unlike the snapshot-cloned approval/verdict caches.
type escalationValve struct {
	mu          sync.Mutex
	consecutive int
	total       int
	// promptedOnce marks that the one-time interactive recovery prompt (change
	// 0067) has been issued for the current trip. It prevents re-prompting after
	// a rejection (subsequent gray-area calls go straight to per-call asks) and
	// double-prompting from parallel children (the valve is shared by reference
	// across CloneForChild). reset() clears it so a future trip can prompt again.
	promptedOnce bool
}

// valveConsecutiveLimit and valveTotalLimit are the escalation thresholds: 3
// consecutive OR 20 total classifier blocks pause auto mode (Claude Code's
// thresholds, per the auto-mode design D8).
const (
	valveConsecutiveLimit = 3
	valveTotalLimit       = 20
)

// recordBlock counts a classifier-layer block (a deny from the classifier). The
// trip is observed separately via tripped() at the next classifier call, so the
// block that reaches a threshold is itself still surfaced as a normal deny.
func (v *escalationValve) recordBlock() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.consecutive++
	v.total++
}

// recordNonBlock records a non-block classifier verdict (allow or ask), resetting
// the consecutive counter; the cumulative total is never reset within a session.
func (v *escalationValve) recordNonBlock() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.consecutive = 0
}

// tripped reports whether the valve has reached either threshold.
func (v *escalationValve) tripped() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.trippedLocked()
}

func (v *escalationValve) trippedLocked() bool {
	return v.consecutive >= valveConsecutiveLimit || v.total >= valveTotalLimit
}

// counts returns a snapshot of the consecutive and total block counters for the
// summary error.
func (v *escalationValve) counts() (consecutive, total int) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.consecutive, v.total
}

// reset zeroes both counters under the valve mutex. It is called by the gate's
// SetMode only on the leaving-auto transition: a fresh entry into auto later
// starts the escalation budget clean, while entering/staying in auto leaves the
// counters as-is.
func (v *escalationValve) reset() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.consecutive = 0
	v.total = 0
	v.promptedOnce = false
}

// claimPrompt atomically claims the one-time recovery prompt for the current
// trip: the first caller gets true (and issues the prompt), every later caller
// gets false until reset() clears the claim.
func (v *escalationValve) claimPrompt() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.promptedOnce {
		return false
	}
	v.promptedOnce = true
	return true
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

// WithInteractive marks the gate as interactive (a human is reachable through
// approve). It governs the escalation valve's tripped behaviour: interactive
// gates fall back to prompting, non-interactive gates abort with a summary error.
// The default (option omitted) is the non-interactive posture.
func WithInteractive(interactive bool) Option {
	return func(g *PermissionGate) { g.interactive = interactive }
}

// WithMode overrides the gate's starting mode after it is seeded from cfg.Mode.
// It is the per-turn-construction seam for the session-mode surface: a fresh
// gate is built at the session's current mode (SessionMode.Get()) rather than
// the raw cfg.Permissions.Mode, so a mid-session switch is picked up by the next
// built gate. Omitting the option leaves the cfg-derived mode untouched, so
// existing three-argument callers (one-shot, mcp-server) behave unchanged.
func WithMode(mode PermissionMode) Option {
	return func(g *PermissionGate) { g.mode = mode }
}

// WithSessionMode wires the gate to the session's live PermissionMode holder. When
// set, currentMode() returns holder.Get() live rather than the construction-time
// snapshot, so a mid-turn Shift+Tab / /mode switch bites the already-built gate
// and every running child (CloneForChild propagates the holder). The mode snapshot
// remains the fallback for holderless gates (one-shot, mcp-server, tests), and
// SetMode still drives holderless gates. Passing nil is a no-op, leaving the
// snapshot posture intact.
func WithSessionMode(sm *SessionMode) Option {
	return func(g *PermissionGate) { g.sessionMode = sm }
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
		valve:   &escalationValve{},
	}
	for _, opt := range opts {
		opt(g)
	}
	// Seed the transition tracker to the gate's effective initial mode so the first
	// currentMode() read is never mistaken for a mode transition. Read the holder
	// directly here (not via currentMode) to avoid a spurious self-observed reset.
	if g.sessionMode != nil {
		g.lastObservedMode = g.sessionMode.Get()
	} else {
		g.lastObservedMode = g.mode
	}
	return g
}

// currentMode returns the gate's active mode. When a SessionMode holder is wired
// (the interactive shell), the holder is the live source of truth so a mid-turn
// switch bites this already-built gate; otherwise the mode snapshot is used. Every
// mode read in resolve/resolveAuto/CloneForChild goes through here so a concurrent
// SetMode or holder flip never races the read.
//
// Because the holder can change without SetMode being called, currentMode() is
// also the observation point for the escalation-valve reset: it compares the
// effective mode to lastObservedMode (both under modeMu) and, on an auto→non-auto
// transition, resets the valve — mirroring SetMode's leaving-auto semantics.
// valve.reset() takes its own valve.mu; modeMu and valve.mu are never acquired in
// the opposite order anywhere, so there is no lock-order inversion.
func (g *PermissionGate) currentMode() PermissionMode {
	g.modeMu.Lock()
	mode := g.mode
	if g.sessionMode != nil {
		mode = g.sessionMode.Get()
	}
	leavingAuto := g.lastObservedMode == ModeAuto && mode != ModeAuto
	g.lastObservedMode = mode
	g.modeMu.Unlock()

	if leavingAuto && g.valve != nil {
		g.valve.reset()
	}
	return mode
}

// Mode returns the gate's active mode through the guarded accessor. It is the
// exported read seam construction sites use to verify a freshly built gate was
// constructed at the intended (session) mode.
func (g *PermissionGate) Mode() PermissionMode { return g.currentMode() }

// HasClassifier reports whether the gate was wired with an auto-mode classifier.
// It is the exported seam construction sites use to verify the classifier is
// present (constructible-and-wired) versus the nil fail-closed-ask posture.
func (g *PermissionGate) HasClassifier() bool { return g.classifier != nil }

// SetMode switches the live gate's mode under modeMu, so an in-flight resolve
// never races the write and the very next resolve observes the new mode. It is
// the root-gate half of the session-mode surface for HOLDERLESS gates (the
// SessionMode holder is the per-turn-construction half); holder-backed gates take
// their mode from the holder and SetMode's snapshot write is inert on their read
// path, but SetMode still keeps the transition tracker honest below.
//
// D10 semantics:
//   - The root gate switches immediately (no new gate needed).
//   - Session-cache grants survive the switch: SetMode does not touch g.cache.
//   - The escalation valve resets ONLY when leaving auto (auto → non-auto), so a
//     later fresh entry into auto starts the budget clean; entering/staying in
//     auto leaves the counters as-is.
//   - Already-spawned children are unaffected: CloneForChild snapshots the
//     parent's mode into the child's own field at spawn time.
//
// The leaving-auto transition is computed against lastObservedMode — the SAME
// ledger currentMode() maintains — and lastObservedMode is advanced here, so a
// currentMode() call immediately after SetMode does NOT re-detect (and
// re-reset on) the same transition. This keeps the reset firing exactly once
// per leaving-auto edge whether that edge is driven by SetMode or by the holder.
func (g *PermissionGate) SetMode(mode PermissionMode) {
	g.modeMu.Lock()
	leavingAuto := g.lastObservedMode == ModeAuto && mode != ModeAuto
	g.mode = mode
	g.lastObservedMode = mode
	g.modeMu.Unlock()

	if leavingAuto && g.valve != nil {
		g.valve.reset()
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
		return tools.Result{
			IsError:   true,
			Output:    fmt.Sprintf("tool %q is disabled", name),
			Denied:    true,
			DenyLayer: LayerDisabled,
		}
	}
	if !policy.AutoApprove {
		msg := "tool call denied by user"
		if policy.DenyReason != "" {
			msg = policy.DenyReason
		}
		layer := policy.DenyLayer
		if layer == "" {
			layer = LayerHuman
		}
		// Append actionable guidance (change 0067): models retry denied calls
		// verbatim, so the denial itself says why that will keep failing.
		return tools.Result{
			IsError:   true,
			Output:    msg + "; " + denyHint(layer),
			Denied:    true,
			DenyLayer: layer,
		}
	}
	return g.inner.Execute(ctx, name, args)
}

// emitDecision reports one gate resolution through the ctx-carried
// DecisionSink (change 0067). No-op when no sink is installed (gates outside
// an agent loop: mcp-server, probes, tests without wiring).
func (g *PermissionGate) emitDecision(ctx context.Context, mode PermissionMode, name, args, verdict, layer, reason string) {
	sink := decisionSinkFrom(ctx)
	if sink == nil {
		return
	}
	sink(Decision{
		Tool:    name,
		Verdict: verdict,
		Layer:   layer,
		Reason:  reason,
		Mode:    mode.String(),
		Command: commandPreview(name, args),
	})
}

// resolve applies the 3-source policy merge and returns the final ToolPolicy.
// Every terminal outcome — and the pre-approval ask — is reported through the
// ctx-carried DecisionSink so gate behaviour is measurable (change 0067).
func (g *PermissionGate) resolve(ctx context.Context, name, args string) (ToolPolicy, error) {
	// Read the live mode once so a concurrent SetMode cannot flip it mid-resolve.
	// (Read before mediation only so decision events carry the mode; mediation
	// itself remains mode-independent and terminal.)
	mode := g.currentMode()

	// Complete mediation (change #52 D5) runs FIRST and is terminal: a tool that
	// reaches a downstream target not on the principal's allowlist — or requesting
	// scope beyond the ceiling — is denied regardless of mode or approval, because
	// "the tool exists and its args are schema-valid" is not authorization. This
	// enforces authority, not user preference, so even ModeOff cannot bypass it.
	if g.mediator != nil {
		if allowed, reason := g.mediator.MediateTarget(ctx, name, args); !allowed {
			if reason == "" {
				reason = "tool target denied by policy (not on the caller's allowlist)"
			}
			g.emitDecision(ctx, mode, name, args, "deny", LayerMediation, reason)
			return ToolPolicy{Enabled: true, AutoApprove: false, DenyReason: reason, DenyLayer: LayerMediation}, nil
		}
	}

	// Disabled list overrides everything.
	for _, d := range g.cfg.Disabled {
		if d == name {
			g.emitDecision(ctx, mode, name, args, "deny", LayerDisabled, "tool disabled")
			return ToolPolicy{Enabled: false}, nil
		}
	}

	policy := ToolPolicy{Enabled: true}

	if mode == ModeOff {
		g.emitDecision(ctx, mode, name, args, "allow", LayerModeOff, "")
		policy.AutoApprove = true
		return policy, nil
	}

	// Session cache — highest precedence, covers both smart and prompt-all.
	if g.cache.Check(name, args) {
		g.emitDecision(ctx, mode, name, args, "allow", LayerCache, "")
		policy.AutoApprove = true
		return policy, nil
	}

	// askLayer names the pipeline stage that routed this call to the human, so
	// the pre-approval ask event says WHY a prompt appeared. Defaults to human
	// (prompt-all, smart fallthrough) and is refined by the auto pipeline.
	askLayer := LayerHuman

	if mode == ModeAuto {
		verdict, layer, reason := g.resolveAuto(ctx, name, args)
		switch verdict {
		case VerdictAllow:
			g.emitDecision(ctx, mode, name, args, "allow", layer, "")
			policy.AutoApprove = true
			return policy, nil
		case VerdictDeny:
			// A deny is not an error: return a non-auto-approve policy carrying
			// the layer-named reason so the model can retry with a different call.
			g.emitDecision(ctx, mode, name, args, "deny", layer, reason)
			return ToolPolicy{Enabled: true, AutoApprove: false, DenyReason: reason, DenyLayer: layer}, nil
		default:
			// VerdictAsk ⇒ fall through to the shared human-approval block below.
			askLayer = layer
		}
	}

	if mode == ModeSmart {
		// always_prompt demotes even safe-list entries — check first.
		if !matchesAny(g.cfg.AlwaysPrompt, name, args) {
			// auto_approve config patterns promote beyond the safe list.
			if matchesAny(g.cfg.AutoApprove, name, args) || onSafeList(name) {
				g.emitDecision(ctx, mode, name, args, "allow", LayerSmartConfig, "")
				policy.AutoApprove = true
				return policy, nil
			}
		}
	}

	// Human approval required. The ask is emitted BEFORE g.approve runs so asks
	// are countable regardless of the approval binding — an AlwaysApprove
	// (headless) gate shows as back-to-back ask→allow events.
	g.emitDecision(ctx, mode, name, args, "ask", askLayer, "")
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
		g.emitDecision(ctx, mode, name, args, "deny", LayerHuman, "tool call denied by user")
		return ToolPolicy{Enabled: true, AutoApprove: false, DenyLayer: LayerHuman}, nil
	}
	if allowSession {
		g.cache.Allow(name, args)
	}
	g.emitDecision(ctx, mode, name, args, "allow", LayerHuman, "")
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
//
// The returned layer names the deciding pipeline stage (Layer* constants) for
// the permission.decision event and the typed denial (change 0067).
func (g *PermissionGate) resolveAuto(ctx context.Context, name, args string) (verdict Verdict, layer, reason string) {
	if name != "bash" {
		if onSafeList(name) {
			return VerdictAllow, LayerSafelist, ""
		}
		// Edit tools carry a single "path" arg rather than a shell command: scope it
		// against the workspace exactly as the bash heuristic scopes mutating path
		// args. An in-workspace target auto-approves; anything the scope cannot prove
		// in-workspace (an escape, a symlink whose target escapes, a missing/garbled
		// path) fails toward the human — never toward the classifier.
		if isEditTool(name) {
			path, ok := editPath(args)
			if !ok {
				return VerdictAsk, LayerEditScope, ""
			}
			if withinWorkspace(path, g.workspaceRoot) {
				return VerdictAllow, LayerEditScope, ""
			}
			return VerdictAsk, LayerEditScope, ""
		}
		// web_fetch carries a URL rather than a shell command: apply the static host
		// floor first (SSRF / config deny/ask / blocklist), and only a fallthrough
		// host reaches the reputation-aware classifier. A missing/garbled url makes
		// classifyFetchHost return Ask("malformed-url"), so it fails toward the human.
		if name == "web_fetch" {
			r := classifyFetchHost(fetchURL(args), g.cfg.Auto.FetchDeny, g.cfg.Auto.FetchAsk)
			switch r.Verdict {
			case VerdictDeny:
				return VerdictDeny, LayerFetchFloor, "denied by auto-mode web_fetch host floor (" + r.DecidedBy + "): " + r.Host
			case VerdictAsk:
				return VerdictAsk, LayerFetchFloor, ""
			default:
				// Fallthrough host ⇒ reputation-aware classifier (valve-enforced).
				return g.classifyWebFetch(ctx, args, r)
			}
		}
		return g.classifyOrAsk(ctx, name, args)
	}

	command := bashCommand(args)

	// 1. Static floor: parse into segments. Unparseable fails toward the human.
	// LayerParse makes these asks countable — they are the shapes change 0070
	// (shell-parse widening) targets.
	segments, err := splitSegments(command)
	if err != nil {
		return VerdictAsk, LayerParse, ""
	}

	// 2. Deterministic rules. Deny is terminal; allow is a positive auto-approve.
	switch evalRules(segments, g.cfg.Auto, g.cfg.AutoApprove, g.cfg.AlwaysPrompt) {
	case VerdictDeny:
		return VerdictDeny, LayerRules, "denied by auto-mode rules layer: " + command
	case VerdictAllow:
		return VerdictAllow, LayerRules, ""
	}

	// 3. Read-only safe list short-circuit.
	if allSegmentsReadOnlySafe(segments) {
		return VerdictAllow, LayerSafelist, ""
	}

	// 4. Heuristics: egress boundary / path scoping. Ask ⇒ continue to classifier.
	switch classifyHeuristic(segments, g.workspaceRoot) {
	case VerdictDeny:
		return VerdictDeny, LayerHeuristic, "denied by auto-mode heuristic layer: " + command
	case VerdictAllow:
		return VerdictAllow, LayerHeuristic, ""
	}

	// 5. Classifier (final) or fail-closed ask.
	return g.classifyOrAsk(ctx, name, command)
}

// classifyOrAsk consults the injected classifier for a final verdict, or fails
// closed to VerdictAsk when no classifier is wired. A classifier deny carries a
// layer-named reason.
//
// This is the classifier-layer invocation site, so the escalation valve is
// enforced here (and only here — static rules/safe-list/heuristic denies never
// count toward it). When the valve has already tripped, the classifier is not
// consulted at all: an interactive gate returns VerdictAsk (fall back to the
// human), a non-interactive gate returns VerdictDeny with a summary error naming
// the trip and the counts. Otherwise the classifier's verdict is recorded — a
// deny is a block (advancing/tripping the valve), an allow or ask resets the
// consecutive counter.
func (g *PermissionGate) classifyOrAsk(ctx context.Context, name, command string) (verdict Verdict, layer, reason string) {
	if g.classifier == nil {
		return VerdictAsk, LayerClassifier, ""
	}

	// Valve already tripped ⇒ recover (one interactive prompt, change 0067) or
	// pause auto mode without consulting the classifier.
	if g.valve.tripped() && !g.valveRecover(ctx) {
		v, r := g.valvePaused()
		return v, LayerValve, r
	}

	// The user's conversation turns are carried on ctx by the agent loop via
	// permissions.WithUserMessages (the ctx-carry seam, avoiding widening
	// agent.ToolExecutor). userMessagesFrom is nil-safe: when the loop never
	// wired them, this is nil and the classifier functions from the pending-call
	// turn alone. The classifier itself re-filters to user turns as defense in
	// depth (buildMessages drops non-user roles).
	switch g.classifier.Classify(ctx, userMessagesFrom(ctx), name, command) {
	case VerdictAllow:
		g.valve.recordNonBlock()
		return VerdictAllow, LayerClassifier, ""
	case VerdictDeny:
		// A classifier deny is a "block": advance the valve. This deny is still
		// enforced as a real verdict; the trip only pauses auto mode from the NEXT
		// classifier call on (checked via g.valve.tripped() at entry above), so the
		// block that reaches a threshold is itself surfaced normally.
		g.valve.recordBlock()
		return VerdictDeny, LayerClassifier, "denied by auto-mode classifier: " + command
	default:
		g.valve.recordNonBlock()
		return VerdictAsk, LayerClassifier, ""
	}
}

// classifyWebFetch consults the reputation-aware web_fetch classifier for a final
// verdict on a fallthrough host, or fails closed to VerdictAsk when no classifier
// is wired. It is a sibling of classifyOrAsk and enforces the SAME escalation
// valve: the valve is consulted before the classifier (a tripped valve pauses auto
// mode without a call — VerdictAsk interactive, a summary-error VerdictDeny
// otherwise), a deny is recorded as a block (advancing/tripping the valve) and an
// allow/ask resets the consecutive counter. r carries the static floor's host and
// AllowNudge, threaded to the classifier as a reputation bias hint.
func (g *PermissionGate) classifyWebFetch(ctx context.Context, args string, r fetchFloorResult) (verdict Verdict, layer, reason string) {
	if g.classifier == nil {
		return VerdictAsk, LayerClassifier, ""
	}

	// Valve already tripped ⇒ recover (one interactive prompt, change 0067) or
	// pause auto mode without consulting the classifier.
	if g.valve.tripped() && !g.valveRecover(ctx) {
		v, rr := g.valvePaused()
		return v, LayerValve, rr
	}

	// The user's conversation turns are carried on ctx (parity with
	// classifyOrAsk); userMessagesFrom is nil-safe, so a context that never
	// passed through WithUserMessages preserves the pending-call-only behavior.
	switch g.classifier.ClassifyWebFetch(ctx, userMessagesFrom(ctx), r.Host, r.AllowNudge, args) {
	case VerdictAllow:
		g.valve.recordNonBlock()
		return VerdictAllow, LayerClassifier, ""
	case VerdictDeny:
		g.valve.recordBlock()
		return VerdictDeny, LayerClassifier, "denied by auto-mode web_fetch classifier: " + r.Host
	default:
		g.valve.recordNonBlock()
		return VerdictAsk, LayerClassifier, ""
	}
}

// valveRecover attempts the one-time interactive recovery from a tripped valve
// (change 0067): the first caller after a trip prompts the human ONCE — "auto
// mode has denied N commands, continue?" — and an approval resets the valve
// (both counters and the prompt claim), letting the pending call proceed to
// the classifier. It returns true only on that approved recovery. A rejection,
// a non-interactive gate, an approval error, or a prompt already claimed (by a
// rejection earlier or a parallel child mid-prompt) all return false: the
// caller falls back to valvePaused.
func (g *PermissionGate) valveRecover(ctx context.Context) bool {
	if !g.interactive {
		return false
	}
	if !g.valve.claimPrompt() {
		return false
	}
	consecutive, total := g.valve.counts()
	req := ApprovalRequest{
		ToolName: ValveApprovalToolName,
		Preview: fmt.Sprintf("auto mode has denied %d commands (%d in a row) — continue in auto mode?",
			total, consecutive),
	}
	approved, _, err := g.approve(ctx, req)
	if err != nil || !approved {
		// The claim stays set: no re-prompt this trip; gray-area calls fall back
		// to per-call human asks until the valve resets (mode transition or a
		// future approved recovery after reset).
		return false
	}
	g.valve.reset()
	return true
}

// valvePaused returns the verdict for a tripped (and unrecovered) escalation
// valve: VerdictAsk in an interactive gate (fall back to per-call human asks),
// or VerdictDeny carrying a structured summary error in a non-interactive gate.
func (g *PermissionGate) valvePaused() (Verdict, string) {
	if g.interactive {
		return VerdictAsk, ""
	}
	consecutive, total := g.valve.counts()
	return VerdictDeny, fmt.Sprintf(
		"auto mode paused: escalation valve tripped after %d consecutive / %d total classifier blocks this session (thresholds: %d consecutive, %d total)",
		consecutive, total, valveConsecutiveLimit, valveTotalLimit)
}

// isEditTool reports whether name is one of the workspace-scoped edit tools whose
// single "path" arg is path-scoped in auto mode (rather than routed to the
// classifier as a shell command).
func isEditTool(name string) bool {
	return name == "write_file" || name == "edit_file"
}

// editPath extracts the "path" arg from an edit tool's JSON args. ok is false when
// the args are unparseable or the path is missing/empty, so the caller fails toward
// the human rather than scoping a garbled path.
func editPath(args string) (path string, ok bool) {
	var v struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(args), &v); err != nil {
		return "", false
	}
	if v.Path == "" {
		return "", false
	}
	return v.Path, true
}

// fetchURL extracts the "url" arg from a web_fetch tool's JSON args (the built-in
// web_fetch schema names it "url"). A non-JSON or missing-url args string yields
// "", which classifyFetchHost treats as a malformed URL ⇒ Ask (fail toward the
// human) rather than a fallthrough.
func fetchURL(args string) string {
	var v struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal([]byte(args), &v); err == nil {
		return v.URL
	}
	return ""
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
	// Snapshot the parent's effective mode once via the guarded accessor. For a
	// holderless gate this is the child's fixed mode (a later parent SetMode does
	// not disturb an already-spawned child). For a holder-backed gate the holder is
	// propagated by reference below, so the child follows the session mode live —
	// this snapshot only seeds the child's field/tracker consistently.
	mode := g.currentMode()
	child := &PermissionGate{
		mode: mode,
		// Propagate the session holder by reference: a child of a holder-backed gate
		// reads the same live session mode, so a mid-turn switch reaches running
		// children too (supersedes D10's "children keep their spawn mode").
		sessionMode: g.sessionMode,
		// lastObservedMode is per-gate (seeded to the child's effective mode), unlike
		// the by-reference valve: parent and child each maintain their own transition
		// ledger. Both may independently observe the same holder auto→non-auto edge
		// and each call valve.reset(), but reset is idempotent (0,0 → 0,0) and both
		// reach the same conclusion, so the shared budget stays correct.
		lastObservedMode: mode,
		cfg:              g.cfg,
		cache:            g.cache.Clone(),
		approve:          prefixedApprove(label, g.approve),
		inner:            g.inner,
		classifier:       g.classifier.cloneForChild(),
		workspaceRoot:    g.workspaceRoot,
		interactive:      g.interactive,
		// The escalation valve is a per-session budget: unlike the snapshot-cloned
		// approval/verdict caches, it is shared by reference so a child's classifier
		// blocks count toward the same session valve as the parent.
		valve: g.valve,
		// The complete-mediation seam is shared by reference: a child agent's
		// downstream tool calls inherit the same principal/tenant target ceiling.
		mediator: g.mediator,
	}
	return child
}

// PrefixApproval wraps an ApprovalFunc so a child/subagent's prompts are
// prefixed with [label] and routed through the same parent approval channel,
// exactly like CloneForChild does internally. Entry points that build child
// gates directly (rather than via CloneForChild) use this so subagent approvals
// surface on the parent's channel instead of bypassing it.
func PrefixApproval(label string, fn ApprovalFunc) ApprovalFunc {
	return prefixedApprove(label, fn)
}

// prefixedApprove wraps an ApprovalFunc so that prompts are prefixed with
// [label] to identify which child agent is asking.
func prefixedApprove(label string, fn ApprovalFunc) ApprovalFunc {
	return func(ctx context.Context, req ApprovalRequest) (bool, bool, error) {
		req.Preview = "[" + label + "] " + req.Preview
		return fn(ctx, req)
	}
}

// commandPreview builds the bounded command field for a permission.decision
// event: the bash command truncated to 200 chars, or a truncated raw-args
// preview for other tools. Never full args — tool.call already carries them,
// and the decision stream must stay small (event-stream hygiene, change 0067).
func commandPreview(name, args string) string {
	s := args
	if name == "bash" {
		s = bashCommand(args)
	}
	s = strings.TrimSpace(s)
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
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
