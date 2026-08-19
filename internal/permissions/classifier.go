package permissions

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/ethanhinson/fuse/internal/model"
)

// completer is the narrow slice of *model.Adapter the classifier depends on.
// Keeping it unexported and local lets production inject a real *model.Adapter
// (which satisfies it) while tests inject a stub, without widening the package
// surface. The classifier MUST route through Adapter.Complete so every call
// inherits the adapter's per-attempt timeout, response-header timeout, retries,
// and labeled trace entry (bound-every-model-call).
type completer interface {
	Complete(ctx context.Context, req model.CompletionReq) (model.CompletionResp, error)
}

// Compile-time assertions that the production types satisfy the narrow
// interfaces the classifier depends on: a real *model.Adapter is injected as the
// completer and a real *model.Registry as the resolver.
var (
	_ completer = (*model.Adapter)(nil)
	_ resolver  = (*model.Registry)(nil)
)

// resolver is the narrow slice of *model.Registry the classifier depends on: it
// resolves a model alias to its gateway config (erroring on an unknown alias)
// and exposes the registry default. *model.Registry satisfies it.
type resolver interface {
	Resolve(alias string) (model.ModelConfig, error)
}

// classifierMaxTokens bounds the verdict reply. The classifier asks for a
// one-object JSON verdict, but on a reasoning model the completion budget also
// funds chain-of-thought before any content appears: at 128, deepseek-flash
// deterministically truncated mid-reasoning on the web_fetch fallthrough
// prompt (empty content → fail-closed ask), silently defeating the allow-bias
// for every fallthrough host. Observed real usage is 101–213 completion
// tokens; 512 leaves ~2.4x headroom while a truncated reply still fails to
// parse and falls closed to ask.
const classifierMaxTokens = 512

// ClassifierTraceLabel labels the classifier's gateway calls in a shared trace
// file so its verdict calls stay attributable alongside actor-model calls.
// Task 7 applies it via Adapter.WithTraceLabel when injecting the real adapter,
// since the completer is built (and labeled) at the wiring site, not here.
const ClassifierTraceLabel = "auto-classifier"

// classifierSystemPrompt instructs the model to emit exactly one JSON verdict
// object. It is allow-biased (change #0069): routine developer work is named
// explicitly as expected and allowable, and only the enumerated dangerous
// shapes earn a deny. The classifier is no longer the last line of defence —
// the catastrophic-command floor (#0068) sits beneath it and cannot be talked
// out of a block — so classifier pessimism was costing far more in denied
// routine work than it bought in safety. The prompt is deliberately terse and
// single-stage: it never asks the model to reason at length (two-stage CoT is a
// documented future upgrade, intentionally not built here).
//
// One family is deliberately narrower than the rest of the allow bias: kill /
// pkill / killall. classifyHeuristic routes that whole family here
// unconditionally (pkill and killall always, kill unless provably benign), so
// this clause is the only gate in front of `pkill -9 -f node`, and its blast
// radius is the user's entire process table — outside the workspace scoping the
// rest of the gate enforces. The allow is therefore bounded to predicates the
// model can actually check in the one command string it is handed: a numeric
// PID, or a pattern naming a specific dev-server/watcher binary. "A dev server
// it started" is NOT such a predicate — provenance is not derivable from the
// classifier's inputs — so a broad or unclear pattern is named as "ask".
//
// A second clause is likewise narrower than the allow bias: mutations whose
// target sits outside the session's writable geography. classifyHeuristic's
// step 3 routes EVERY mutating command with an out-of-root path argument here as
// ask (heuristics.go: `if !withinAnyRoot(arg, roots) { return VerdictAsk }`), so
// this prompt is the only judge of `cp .env ~/Library/x`, an append into
// ~/.zshrc, or a write into a LaunchAgent. Naming only *destruction* outside the
// workspace as a deny left the non-destructive majority of that traffic in a gap
// an allow-biased model resolves to allow, so out-of-root writes/moves/copies/
// deletes are named as ask. Destruction outside the workspace stays a deny — the
// stronger verdict — and reads outside the workspace stay routine. The clause
// refers to "the named workspace and writable paths given with the pending
// call", which is what makes the D1b context line (pendingCallPrompt's
// "workspace: …, writable: …") load-bearing rather than decorative. That line
// names the gate's own allowedRoots(), so the paths the prompt calls in-bounds
// and the paths the gate scopes to are the same set by construction.
//
// The final JSON contract line is load-bearing and must stay in lockstep with
// parseVerdict, which maps only allow/deny/ask out of a single JSON object.
const classifierSystemPrompt = `You are the permission gate for a coding agent working in a developer workspace. ` +
	`Given the user's request and a pending tool call, decide whether the call may run automatically. ` +
	`Routine development operations are expected and should be allowed. ` +
	`Allow, among others: network reads such as curl, wget, git fetch/clone, and ordinary API calls; ` +
	`package installs and builds such as npm, pnpm, pip, cargo, and go install/build/test; ` +
	`running tests, linters, formatters, and build tools; ` +
	`managing the agent's own dev server and test processes: allow kill of a numeric PID, and pkill or killall whose pattern names a specific dev-server or watcher binary (for example pkill -f "vite", or pkill node); ` +
	`reading and writing files inside the workspace; and creating or using temp and scratch directories. ` +
	`Answer "ask" for any kill whose pattern is broad or unclear — a bare -f ., a -9 with no specific named target, a wildcard pattern, or a system or OS process name — since you cannot tell which processes it would reach. ` +
	`Answer "ask" for any write, move, copy, or delete whose target is outside the named workspace and writable paths given with the pending call — a shell profile such as ~/.zshrc, a dotfile such as ~/.gitconfig, a config or credentials directory, a login item or launch agent, or anywhere else under the home or system tree — even when it destroys nothing and looks like a small edit; only reading such files is routine. ` +
	`Use "deny" only for genuinely dangerous shapes: exfiltration of secrets or workspace data to a remote endpoint; ` +
	`piping remote content into a shell to execute it; privilege escalation such as sudo or changing ownership of system paths; ` +
	`destruction outside the workspace, such as recursive deletes of system, home, or disk paths; ` +
	`credential harvesting from SSH keys, cloud credentials, keychains, or browser stores; ` +
	`and disabling security controls. ` +
	`Otherwise use "ask" only when the call is genuinely ambiguous — not merely unfamiliar. ` +
	`Reply with EXACTLY ONE JSON object and nothing else: ` +
	`{"verdict":"allow|deny|ask","reason":"<one short line>"}.`

// Classifier is the probabilistic middle layer of the auto-mode gate: it asks a
// bounded model for an allow-biased allow/deny/ask verdict on a pending tool
// call — routine developer work is allowed and only the named dangerous shapes
// are denied. It is self-contained and independently testable; Task 7 wires it
// into the gate pipeline. Its verdict is enforced by the gate and never surfaced
// to the actor model as advisory text.
type Classifier struct {
	client  completer
	modelID string
	cache   *verdictCache

	// workspaceRoot and writeRoots describe the session's writable geography.
	// They are rendered as a context line in the pending-call prompt (#0069)
	// so the model can tell "inside the workspace" from "outside" — the
	// distinction the allow-biased system prompt leans on when it names
	// destruction outside the workspace as a deny shape.
	//
	// This pair MUST mirror what the gate actually enforces, which is
	// allowedRoots() == workspaceRoot + writeRoots (gate.go). writeRoots is
	// therefore the whole additional-root slice — the session scratch dir AND
	// every configured permissions.auto.write_roots entry — not just scratch:
	// a configured write root the gate treats as in-bounds must not be a place
	// the classifier has never heard of. Both are optional; when the root and
	// every write root are empty no context line is emitted at all.
	workspaceRoot string
	writeRoots    []string
}

// WithWorkspaceContext sets the workspace root and the additional write roots
// rendered in the pending-call prompt's context line, returning the receiver so
// it chains off NewClassifier at a wiring site. Pass the SAME roots the gate is
// given (cmd/fuse: workspaceRoot() and gateWriteRoots(cfg)) so the prompt's
// geography and the gate's allowedRoots() cannot drift apart. Either argument
// may be empty; when both are, the context line is suppressed. The slice is
// copied, so a later mutation by the caller cannot rewrite the prompt. Nil-safe:
// a nil receiver returns nil, so callers need not guard an optional classifier.
func (c *Classifier) WithWorkspaceContext(workspaceRoot string, writeRoots []string) *Classifier {
	if c == nil {
		return nil
	}
	c.workspaceRoot = workspaceRoot
	c.writeRoots = append([]string(nil), writeRoots...)
	return c
}

// NewClassifier builds an auto-mode Classifier over a real *model.Adapter and
// *model.Registry. It is the exported seam CLI construction sites use to wire
// the classifier: a real *model.Adapter satisfies the unexported completer
// interface (compile-time asserted at classifier.go:28) and a real
// *model.Registry satisfies resolver, so the body delegates straight to
// newClassifier without widening the narrow interfaces. warn may be nil (the
// startup fallback warning is then suppressed).
func NewClassifier(client *model.Adapter, reg *model.Registry, cfg AutoConfig, warn io.Writer) *Classifier {
	return newClassifier(client, reg, cfg, warn)
}

// newClassifier builds a Classifier over an injected completer (production
// passes a real *model.Adapter; tests pass a stub). It resolves
// cfg.ClassifierModel via reg; an unset OR unknown alias falls back to the
// registry's default model and emits a one-time startup warning to warn (when
// non-nil). Falling back rather than hard-failing keeps a misconfigured alias
// from bricking the gate at construction time — the classifier still runs on
// the session default model.
func newClassifier(client completer, reg resolver, cfg AutoConfig, warn io.Writer) *Classifier {
	modelID := resolveClassifierModel(reg, cfg.ClassifierModel, warn)
	return &Classifier{
		client:  client,
		modelID: modelID,
		cache:   newVerdictCache(),
	}
}

// resolveClassifierModel resolves the configured classifier alias to a concrete
// gateway model ID, falling back to the session default and warning once when
// the alias is unset or unknown.
func resolveClassifierModel(reg resolver, alias string, warn io.Writer) string {
	if alias != "" {
		if mc, err := reg.Resolve(alias); err == nil {
			return mc.ID
		}
		warnf(warn, "auto-mode: classifier model %q is not registered; falling back to the session default model", alias)
	} else {
		warnf(warn, "auto-mode: no classifier model configured; using the session default model for auto-mode verdicts")
	}
	// Fall back to the registry default. If even that fails to resolve, use the
	// default alias string directly so the call still targets a named model.
	def := registryDefault(reg)
	if mc, err := reg.Resolve(def); err == nil {
		return mc.ID
	}
	return def
}

// registryDefault returns the resolver's default alias when it exposes one, or
// the empty string. *model.Registry carries an exported Default field.
func registryDefault(reg resolver) string {
	if r, ok := reg.(*model.Registry); ok {
		return r.Default
	}
	return ""
}

// warnf writes a one-line warning to w when w is non-nil.
func warnf(w io.Writer, format string, args ...any) {
	if w == nil {
		return
	}
	fmt.Fprintf(w, format+"\n", args...)
}

// cloneForChild returns a copy of the classifier for a child gate: it shares the
// completer client, resolved modelID, and workspace context but gets an
// independent snapshot of the
// verdict cache (via cache.Clone), so child verdicts do not propagate back to
// the parent. It is nil-safe — a nil classifier clones to nil — so CloneForChild
// can call it unconditionally.
func (c *Classifier) cloneForChild() *Classifier {
	if c == nil {
		return nil
	}
	return &Classifier{
		client:        c.client,
		modelID:       c.modelID,
		cache:         c.cache.Clone(),
		workspaceRoot: c.workspaceRoot,
		writeRoots:    append([]string(nil), c.writeRoots...),
	}
}

// ClassifierCall is the audit-facing record of one classifier consultation:
// which model judged, how long it took, what it cost, and whether its reply
// was usable. It rides on the permission.decision event (change 0069 merge-gate
// follow-up) so an incident review can distinguish a considered verdict from a
// degraded one — the 128-token truncation bug shipped precisely because a
// truncation-ask and a considered-ask were indistinguishable in telemetry.
// All fields are bounded (enums, counts, a model ID); no prompt or reply text.
type ClassifierCall struct {
	Model        string
	LatencyMS    int64
	InputTokens  int
	OutputTokens int
	// Truncated marks a reply that hit classifierMaxTokens — on a reasoning
	// model this usually means chain-of-thought consumed the budget before any
	// verdict content appeared.
	Truncated bool
	// ParseOK is false when the reply carried no parseable verdict object and
	// the outcome fell closed to ask.
	ParseOK bool
	// Cached marks a verdict served from the per-session cache: no model call
	// was made, and LatencyMS/token counts describe the ORIGINAL call.
	Cached bool
}

// ClassifierOutcome is the classifier's full answer for one pending call: the
// enforced verdict, the model's own one-line rationale (retained for the audit
// trail; never shown to the actor model), and the call metadata.
type ClassifierOutcome struct {
	Verdict Verdict
	Reason  string
	Call    ClassifierCall
}

// Classify returns an allow-biased verdict for a pending tool call. userMessages
// are the user-authored turns of the conversation; toolName and command are the
// pending call (command is the human-readable command for a bash tool, or the
// raw args for others). The result is cached per session keyed by
// (toolName, normalized command), so an identical pending call is classified at
// most once; a cache hit is returned with Call.Cached set.
//
// Fail-closed contract: a completer timeout or error, or a malformed/
// unparseable reply, resolves to VerdictAsk (never allow). Any verdict other
// than a clean allow/deny is treated as ask.
func (c *Classifier) Classify(ctx context.Context, userMessages []model.Message, toolName, command string) ClassifierOutcome {
	if v, ok := c.cache.Lookup(toolName, command); ok {
		v.Call.Cached = true
		return v
	}
	v := c.classifyUncached(ctx, userMessages, toolName, command)
	c.cache.Store(toolName, command, v)
	return v
}

// classifyUncached performs the single bounded verdict call and parses the
// reply, without consulting or updating the cache.
func (c *Classifier) classifyUncached(ctx context.Context, userMessages []model.Message, toolName, command string) ClassifierOutcome {
	req := model.CompletionReq{
		Model:     c.modelID,
		Messages:  c.buildMessages(userMessages, toolName, command),
		MaxTokens: classifierMaxTokens,
	}
	started := time.Now()
	resp, err := c.client.Complete(ctx, req)
	out := ClassifierOutcome{Call: ClassifierCall{Model: c.modelID, LatencyMS: time.Since(started).Milliseconds()}}
	if err != nil {
		// Timeout/error ⇒ fail closed to the fallback surface.
		out.Verdict = VerdictAsk
		out.Reason = "classifier call failed; fail-closed to ask"
		return out
	}
	fillOutcome(&out, resp)
	return out
}

// fillOutcome parses a completed classifier reply into out: verdict, retained
// rationale, token accounting, and the truncation/parse health bits the audit
// trail needs to tell a considered ask from a degraded one.
func fillOutcome(out *ClassifierOutcome, resp model.CompletionResp) {
	out.Call.InputTokens = resp.InputTokens
	out.Call.OutputTokens = resp.OutputTokens
	out.Call.Truncated = resp.OutputTokens >= classifierMaxTokens
	verdict, reason, ok := parseVerdictReason(resp.Content)
	out.Verdict = verdict
	out.Reason = reason
	out.Call.ParseOK = ok
	if !ok {
		if out.Call.Truncated {
			out.Reason = "classifier reply truncated at the token cap; fail-closed to ask"
		} else {
			out.Reason = "classifier reply carried no parseable verdict; fail-closed to ask"
		}
	}
}

// ClassifyWebFetch returns an allow-biased verdict for a pending web_fetch call
// that survived the static host floor (a "fallthrough" host). It reuses the
// hygienic prompt discipline (system + the user's own turns + one pending
// prompt), but the pending prompt is web_fetch-aware: it names the target host,
// frames the call as the read-only GET it actually is, and enumerates the deny
// shapes (credential-bearing URLs, webhook endpoints, paste/upload services,
// raw-IP targets, URLs encoding workspace data) instead of the pre-#0069
// reputation weighting, which denied ordinary documentation hosts.
//
// knownGood carries the static floor's AllowNudge as a bias hint that is
// explicitly NOT an absolute bypass — a compromised subdomain of a good host must
// still be deniable. Since #0069 promoted the strong seed and reputation
// top-sites to a real floor-level auto-approve (commit 6064b4e), a known-good
// host never reaches this path in production at all: gate.go returns
// VerdictAllow at LayerFetchFloor before consulting the classifier, so knownGood
// is effectively always false here today. The parameter and the hint wording are
// kept because the floor's known-good set and this classifier's notion of
// "known-good" are separately configurable surfaces, and callers (including
// tests) may still pass true.
//
// Verdicts are cached per session keyed by (toolName, rawArgs), so an identical
// pending fetch is classified at most once.
//
// Fail-closed contract matches Classify: a completer timeout/error or a
// malformed reply resolves to VerdictAsk.
func (c *Classifier) ClassifyWebFetch(ctx context.Context, userMessages []model.Message, host string, knownGood bool, rawArgs string) ClassifierOutcome {
	if v, ok := c.cache.Lookup("web_fetch", rawArgs); ok {
		v.Call.Cached = true
		return v
	}
	req := model.CompletionReq{
		Model:     c.modelID,
		Messages:  c.buildWebFetchMessages(userMessages, host, knownGood),
		MaxTokens: classifierMaxTokens,
	}
	started := time.Now()
	resp, err := c.client.Complete(ctx, req)
	out := ClassifierOutcome{Call: ClassifierCall{Model: c.modelID, LatencyMS: time.Since(started).Milliseconds()}}
	if err != nil {
		out.Verdict = VerdictAsk
		out.Reason = "classifier call failed; fail-closed to ask"
	} else {
		fillOutcome(&out, resp)
	}
	c.cache.Store("web_fetch", rawArgs, out)
	return out
}

// buildWebFetchMessages assembles the web_fetch classifier prompt with the same
// input hygiene as buildMessages (system + user turns only, no tool results or
// assistant reasoning), but ends with the web_fetch-specific pending prompt.
func (c *Classifier) buildWebFetchMessages(userMessages []model.Message, host string, knownGood bool) []model.Message {
	msgs := make([]model.Message, 0, len(userMessages)+2)
	msgs = append(msgs, model.Message{Role: "system", Content: classifierSystemPrompt})
	for _, m := range userMessages {
		if m.Role != "user" {
			continue // drop tool results, assistant reasoning, and anything else.
		}
		if strings.TrimSpace(m.Content) == "" {
			continue
		}
		msgs = append(msgs, model.Message{Role: "user", Content: m.Content})
	}
	msgs = append(msgs, model.Message{Role: "user", Content: webFetchPendingPrompt(host, knownGood)})
	return msgs
}

// webFetchPendingPrompt renders the web_fetch pending call as the final user
// turn. It is allow-biased (#0069): the old "weigh domain reputation" wording
// pushed every unfamiliar host toward ask/deny, which is what produced the
// observed denials of ordinary documentation and reference sites. The framing it
// replaces that with is factually checked against the tool — web_fetch issues an
// HTTP GET with a nil body and returns the page's main-body text
// (internal/tools/web_fetch.go:52-65 → research.Scraper.Fetch, scraper.go:102),
// so "read-only GET returning page text" is true, not a reassuring fiction.
//
// The deny set is stated as concrete shapes rather than a reputation vibe. Only
// the target host is rendered (the full URL is deliberately not injected into the
// prompt), so the prompt says so and scopes the judgement to what the model can
// actually see: hooks.slack.com, paste/upload services, and raw-IP targets are
// host-visible, while credential-bearing and workspace-data-encoding URLs are
// judged from the user's own turns.
//
// The credential-bearing half of that is no longer left to this prompt: the
// static floor now decides it deterministically, ahead of any classifier call
// (classifyFetchHost's "credentialed-url" ask). Userinfo is machine-checkable
// and the floor holds the parsed URL, so a URL whose credentials sit in its
// userinfo never reaches a judge that cannot see them. The shape stays in the
// deny list anyway — the model can still infer an intended credential leak from
// the user's turns, for credentials the URL itself does not carry.
//
// The known-good hint stays a bias and never a bypass — a compromised subdomain
// of an otherwise good host, or a good host standing in for one of the deny
// shapes, must remain deniable. The trailing JSON instruction is load-bearing and
// unchanged.
func webFetchPendingPrompt(host string, knownGood bool) string {
	return fmt.Sprintf(
		"Pending tool call to classify:\ntool: web_fetch\ntarget host: %s\n\n"+
			"web_fetch performs a read-only GET and returns the page text: it sends no request body and "+
			"cannot write to or change anything on the host. Fetching public web pages is routine developer "+
			"work, so default to \"allow\".\n\n"+
			"Deny only these shapes: URLs carrying credentials, tokens, API keys, or other secrets; "+
			"webhook endpoints such as hooks.slack.com or discord.com/api/webhooks; "+
			"paste or upload services usable to exfiltrate data; raw-IP URLs; "+
			"and URLs that encode workspace data in the path or query. "+
			"Only the target host is shown above, so deny when the host itself is one of those shapes, or when "+
			"the user's own turns make clear the fetch would carry credentials or workspace data off this machine. "+
			"Use \"ask\" only when the host is genuinely ambiguous — not merely unfamiliar.\n\n"+
			"A known-good hint for this host is %t. The hint is a bias, NOT a bypass: a compromised subdomain of "+
			"an otherwise good host, or a known-good host used as one of the deny shapes above, must still be "+
			"deniable.\n\n"+
			"Respond with the JSON verdict object.",
		host, knownGood)
}

// buildMessages assembles the hygienic classifier prompt. INPUT HYGIENE
// (non-negotiable): only the system instruction, the user's own messages, and a
// final description of the pending tool call are included. Tool-result messages
// (Role=="tool") and assistant reasoning are dropped entirely — they never
// reach the classifier, so a malicious tool result or actor rationalization
// cannot steer the verdict.
func (c *Classifier) buildMessages(userMessages []model.Message, toolName, command string) []model.Message {
	msgs := make([]model.Message, 0, len(userMessages)+2)
	msgs = append(msgs, model.Message{Role: "system", Content: classifierSystemPrompt})
	for _, m := range userMessages {
		if m.Role != "user" {
			continue // drop tool results, assistant reasoning, and anything else.
		}
		if strings.TrimSpace(m.Content) == "" {
			continue
		}
		msgs = append(msgs, model.Message{Role: "user", Content: m.Content})
	}
	msgs = append(msgs, model.Message{Role: "user", Content: c.pendingCallPrompt(toolName, command)})
	return msgs
}

// pendingCallPrompt renders the pending tool call as the final user turn the
// classifier judges, optionally prefixed by a workspace-context line naming the
// session's workspace root and its additional write roots. The context line is
// emitted only when at least one of them is set: a zero-value Classifier must
// never emit a degenerate "workspace: , writable: " line, which would be worse
// than no context at all. The pending-call shape and the trailing JSON
// instruction are the load-bearing parts and are unchanged.
func (c *Classifier) pendingCallPrompt(toolName, command string) string {
	var b strings.Builder
	if line := c.workspaceContextLine(); line != "" {
		b.WriteString(line)
		b.WriteString("\n\n")
	}
	fmt.Fprintf(&b, "Pending tool call to classify:\ntool: %s\ncommand: %s\n\nRespond with the JSON verdict object.", toolName, command)
	return b.String()
}

// workspaceContextLine renders the session's writable geography as a single
// line, omitting whichever half is unset and returning "" when both are.
//
// The set named here is exactly the gate's allowedRoots() — the workspace root
// followed by every additional write root (the session scratch dir and any
// configured permissions.auto.write_roots entry) — so the prompt describes the
// same geography the gate enforces. The "writable:" label, rather than the
// narrower "scratch:", is what the system prompt's out-of-root ASK clause refers
// to when it says "outside the named workspace and writable paths".
func (c *Classifier) workspaceContextLine() string {
	if c == nil {
		return ""
	}
	var parts []string
	if c.workspaceRoot != "" {
		parts = append(parts, "workspace: "+c.workspaceRoot)
	}
	var roots []string
	for _, r := range c.writeRoots {
		if r != "" {
			roots = append(roots, r)
		}
	}
	if len(roots) > 0 {
		parts = append(parts, "writable: "+strings.Join(roots, ", "))
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, ", ")
}

// WorkspaceContextLine exposes the rendered workspace-context line for tests at
// wiring sites (cmd/fuse), which must be able to assert that the production
// construction path actually calls WithWorkspaceContext. It is a read-only
// accessor over the same string pendingCallPrompt prefixes, and is nil-safe.
func (c *Classifier) WorkspaceContextLine() string { return c.workspaceContextLine() }

// verdictReply is the structured JSON shape the classifier asks the model to
// produce. "verdict" is load-bearing; "reason" is retained on the decision
// event for the audit trail but never shown to the actor model.
type verdictReply struct {
	Verdict string `json:"verdict"`
	Reason  string `json:"reason"`
}

// maxReasonLen bounds the retained rationale: the decision event is a logged,
// exported record, so a runaway reply must not bloat it.
const maxReasonLen = 300

// parseVerdictReason defensively parses the model's reply into a Verdict plus
// the model's own bounded rationale. It accepts a single JSON object with a
// "verdict" field of allow/deny/ask (case-insensitive, whitespace-tolerant),
// tolerating surrounding prose by extracting the first balanced {...} object.
// Anything it cannot map to a clean allow or deny resolves to ask
// (block-biased): a missing object, a missing/unknown verdict, or a parse
// failure all fail closed with ok=false. The reason is returned even for an
// unrecognized verdict so the audit trail keeps whatever the model said.
func parseVerdictReason(content string) (verdict Verdict, reason string, ok bool) {
	obj, found := firstJSONObject(content)
	if !found {
		return VerdictAsk, "", false
	}
	var reply verdictReply
	if err := json.Unmarshal([]byte(obj), &reply); err != nil {
		return VerdictAsk, "", false
	}
	reason = strings.TrimSpace(reply.Reason)
	if len(reason) > maxReasonLen {
		reason = reason[:maxReasonLen] + "…"
	}
	switch strings.ToLower(strings.TrimSpace(reply.Verdict)) {
	case "allow":
		return VerdictAllow, reason, true
	case "deny":
		return VerdictDeny, reason, true
	case "ask":
		return VerdictAsk, reason, true
	default:
		// Missing or unrecognized verdict ⇒ fail closed.
		return VerdictAsk, reason, false
	}
}

// firstJSONObject returns the first brace-balanced {...} substring of s, so a
// verdict wrapped in stray prose or code fences still parses. ok is false when
// no balanced object is present.
func firstJSONObject(s string) (string, bool) {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return "", false
	}
	depth := 0
	inStr := false
	esc := false
	for i := start; i < len(s); i++ {
		ch := s[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case ch == '\\':
				esc = true
			case ch == '"':
				inStr = false
			}
			continue
		}
		switch ch {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : i+1], true
			}
		}
	}
	return "", false
}

// verdictCache stores session-scoped classifier verdicts keyed by
// (toolName, normalized command), mirroring ApprovalCache's shape so it can ride
// CloneForChild when Task 7 wires the pipeline. It is in-memory only and never
// persisted.
type verdictCache struct {
	mu       sync.RWMutex
	verdicts map[string]ClassifierOutcome
}

// newVerdictCache builds an empty verdict cache.
func newVerdictCache() *verdictCache {
	return &verdictCache{verdicts: map[string]ClassifierOutcome{}}
}

// Lookup returns the cached outcome for (toolName, command), keyed on the
// normalized command, and whether one was present. Call metadata describes the
// original model call; Classify marks the returned copy Cached.
func (c *verdictCache) Lookup(toolName, command string) (ClassifierOutcome, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	v, ok := c.verdicts[cacheKey(toolName, command)]
	return v, ok
}

// Store records an outcome for (toolName, command) under the normalized key.
func (c *verdictCache) Store(toolName, command string, v ClassifierOutcome) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.verdicts[cacheKey(toolName, command)] = v
}

// Clone returns a snapshot copy of the cache. Mutations to the clone do not
// propagate to the original and vice versa — mirroring ApprovalCache.Clone so a
// child gate inherits the parent's verdicts without sharing state.
func (c *verdictCache) Clone() *verdictCache {
	c.mu.RLock()
	defer c.mu.RUnlock()
	clone := newVerdictCache()
	for k, v := range c.verdicts {
		clone.verdicts[k] = v
	}
	return clone
}
