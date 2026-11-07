package permissions

import "context"

// Layer names for permission decisions: the pipeline stage that produced a
// verdict. They are a durable vocabulary — permission.decision events carry
// them on the wire — so values are never renamed, only added.
const (
	LayerDisabled    = "disabled"     // permissions.disabled list
	LayerModeOff     = "mode_off"     // mode: off auto-approve
	LayerCache       = "cache"        // session approval-cache hit
	LayerParse       = "parse"        // unparseable bash with a human bound ⇒ fail-closed ask (the #0070 target); a policy stand-in gets a rules deny (change 0096)
	LayerRules       = "rules"        // deterministic rules (dangerous names, config deny/ask/allow)
	LayerSafelist    = "safelist"     // built-in read-only safe list
	LayerHeuristic   = "heuristic"    // egress boundary / workspace path scoping
	LayerClassifier  = "classifier"   // LLM classifier verdict
	LayerFetchFloor  = "fetch_floor"  // web_fetch static host floor (SSRF/blocklist/config)
	LayerReadScope   = "read_scope"   // read_file/grep/list_directory path scoping
	LayerEditScope   = "edit_scope"   // write_file/edit_file workspace path scoping
	LayerValve       = "valve"        // escalation-valve pause
	LayerHuman       = "human"        // the gate's approver answered (a person, or a policy stand-in)
	LayerSmartConfig = "smart_config" // smart-mode safe-list/config auto-approve
)

// Decision is one permission-gate resolution, reported through the ctx-carried
// DecisionSink so the agent loop can emit it as a permission.decision event
// (change 0067). Verdict is "allow" | "ask" | "deny"; Layer is a Layer*
// constant; Command is a bounded preview, never full args.
type Decision struct {
	Tool    string
	Verdict string
	Layer   string
	Reason  string
	Mode    string
	Command string
	// DecidedBy disambiguates LayerHuman outcomes for the audit trail, taken
	// from the gate's approver: "human" when a real person answered the prompt,
	// "policy" when a stand-in (AlwaysApprove, the non-interactive deny
	// fallback) answered with no human present. Empty on non-human layers
	// except a rules-layer deny forced by a policy approver.
	DecidedBy string
	// Classifier carries the call metadata when the classifier was consulted
	// for this resolution (including an ask that then fell to the human), so a
	// degraded verdict — truncation, parse failure — is distinguishable from a
	// considered one. Nil when no classifier ran.
	Classifier *ClassifierCall
	// SegmentVerdicts carries the per-segment deterministic verdict for a
	// COMPOUND bash command (change 0142): each element names one parsed
	// segment and the verdict the deterministic layers reached for it, so a
	// compound decision stays attributable to the specific part — not just the
	// raw string. Nil for non-compound commands and for non-bash tools. The
	// element that drove an ask or deny is exactly the one whose Verdict is
	// not VerdictAllow; an all-allow compound is attributed to the whole.
	SegmentVerdicts []SegmentVerdict
}

// SegmentVerdict is one parsed segment's deterministic verdict, carried on the
// Decision for a compound bash command so the outcome stays attributable to the
// specific part (change 0142). Name is the segment's argv[0] basename; Verdict
// is what the deterministic layers (rules / safelist / heuristic) reached for
// that segment alone; Reason names the layer when the verdict is not allow.
type SegmentVerdict struct {
	Name    string
	Verdict Verdict
	Reason  string
}

// DecisionSink receives every gate decision. Installed per tool call by the
// agent loop (which stamps the tool-call ID into the emitted event); absent
// from ctx, decisions are silently not reported — gates used outside an agent
// loop (mcp-server, probes) need no wiring.
type DecisionSink func(Decision)

// decisionSinkKey is the unexported context key carrying the DecisionSink,
// mirroring userMessagesKey (the established agent→permissions ctx-carry seam
// that avoids widening agent.ToolExecutor).
type decisionSinkKey struct{}

// WithDecisionSink returns a child context carrying sink for the permission
// gate to report per-call decisions to. A nil sink is allowed and equivalent
// to not installing one.
func WithDecisionSink(ctx context.Context, sink DecisionSink) context.Context {
	return context.WithValue(ctx, decisionSinkKey{}, sink)
}

// decisionSinkFrom returns the sink carried on ctx, or nil when none was
// stored. Nil-safe like userMessagesFrom.
func decisionSinkFrom(ctx context.Context) DecisionSink {
	sink, _ := ctx.Value(decisionSinkKey{}).(DecisionSink)
	return sink
}

// verdictString maps a Verdict to its durable wire token.
func verdictString(v Verdict) string {
	switch v {
	case VerdictAllow:
		return "allow"
	case VerdictDeny:
		return "deny"
	default:
		return "ask"
	}
}
