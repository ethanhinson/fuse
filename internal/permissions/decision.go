package permissions

import "context"

// Layer names for permission decisions: the pipeline stage that produced a
// verdict. They are a durable vocabulary — permission.decision events carry
// them on the wire — so values are never renamed, only added.
const (
	LayerMediation   = "mediation"    // complete-mediation target denial (change #52 D5)
	LayerDisabled    = "disabled"     // permissions.disabled list
	LayerModeOff     = "mode_off"     // mode: off auto-approve
	LayerCache       = "cache"        // session approval-cache hit
	LayerParse       = "parse"        // unparseable bash ⇒ fail-closed ask (the #0070 target)
	LayerRules       = "rules"        // deterministic rules (dangerous names, config deny/ask/allow)
	LayerSafelist    = "safelist"     // built-in read-only safe list
	LayerHeuristic   = "heuristic"    // egress boundary / workspace path scoping
	LayerClassifier  = "classifier"   // LLM classifier verdict
	LayerFetchFloor  = "fetch_floor"  // web_fetch static host floor (SSRF/blocklist/config)
	LayerEditScope   = "edit_scope"   // write_file/edit_file workspace path scoping
	LayerValve       = "valve"        // escalation-valve pause
	LayerHuman       = "human"        // human approval outcome (or its AlwaysApprove stand-in)
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
	// DecidedBy disambiguates LayerHuman outcomes for the audit trail: "human"
	// when a real person answered the prompt, "policy" when a binding-level
	// stand-in (AlwaysApprove, the non-interactive deny fallback) answered with
	// no human present. Empty on non-human layers.
	DecidedBy string
	// Classifier carries the call metadata when the classifier was consulted
	// for this resolution (including an ask that then fell to the human), so a
	// degraded verdict — truncation, parse failure — is distinguishable from a
	// considered one. Nil when no classifier ran.
	Classifier *ClassifierCall
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
