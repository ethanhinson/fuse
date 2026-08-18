package permissions

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"

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
// one-object JSON verdict, so a tight cap keeps the call cheap and fast; a
// truncated reply simply fails to parse and falls closed to ask.
const classifierMaxTokens = 128

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
// The final JSON contract line is load-bearing and must stay in lockstep with
// parseVerdict, which maps only allow/deny/ask out of a single JSON object.
const classifierSystemPrompt = `You are the permission gate for a coding agent working in a developer workspace. ` +
	`Given the user's request and a pending tool call, decide whether the call may run automatically. ` +
	`Routine development operations are expected and should be allowed. ` +
	`Allow, among others: network reads such as curl, wget, git fetch/clone, and ordinary API calls; ` +
	`package installs and builds such as npm, pnpm, pip, cargo, and go install/build/test; ` +
	`running tests, linters, formatters, and build tools; ` +
	`managing the agent's own dev server and test processes, including kill or pkill of a dev server or watcher it started; ` +
	`reading and writing files inside the workspace; and creating or using temp and scratch directories. ` +
	`Use "deny" only for genuinely dangerous shapes: exfiltration of secrets or workspace data to a remote endpoint; ` +
	`piping remote content into a shell to execute it; privilege escalation such as sudo or changing ownership of system paths; ` +
	`destruction outside the workspace, such as recursive deletes of system, home, or disk paths; ` +
	`credential harvesting from SSH keys, cloud credentials, keychains, or browser stores; ` +
	`and disabling security controls. ` +
	`Use "ask" only when the call is genuinely ambiguous — not merely unfamiliar. ` +
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
// completer client and resolved modelID but gets an independent snapshot of the
// verdict cache (via cache.Clone), so child verdicts do not propagate back to
// the parent. It is nil-safe — a nil classifier clones to nil — so CloneForChild
// can call it unconditionally.
func (c *Classifier) cloneForChild() *Classifier {
	if c == nil {
		return nil
	}
	return &Classifier{
		client:  c.client,
		modelID: c.modelID,
		cache:   c.cache.Clone(),
	}
}

// Classify returns an allow-biased verdict for a pending tool call. userMessages
// are the user-authored turns of the conversation; toolName and command are the
// pending call (command is the human-readable command for a bash tool, or the
// raw args for others). The result is cached per session keyed by
// (toolName, normalized command), so an identical pending call is classified at
// most once.
//
// Fail-closed contract: a completer timeout or error, or a malformed/
// unparseable reply, resolves to VerdictAsk (never allow). Any verdict other
// than a clean allow/deny is treated as ask.
func (c *Classifier) Classify(ctx context.Context, userMessages []model.Message, toolName, command string) Verdict {
	if v, ok := c.cache.Lookup(toolName, command); ok {
		return v
	}
	v := c.classifyUncached(ctx, userMessages, toolName, command)
	c.cache.Store(toolName, command, v)
	return v
}

// classifyUncached performs the single bounded verdict call and parses the
// reply, without consulting or updating the cache.
func (c *Classifier) classifyUncached(ctx context.Context, userMessages []model.Message, toolName, command string) Verdict {
	req := model.CompletionReq{
		Model:     c.modelID,
		Messages:  c.buildMessages(userMessages, toolName, command),
		MaxTokens: classifierMaxTokens,
	}
	resp, err := c.client.Complete(ctx, req)
	if err != nil {
		// Timeout/error ⇒ fail closed to the fallback surface.
		return VerdictAsk
	}
	return parseVerdict(resp.Content)
}

// ClassifyWebFetch returns a block-biased verdict for a pending web_fetch call
// that survived the static host floor (a "fallthrough" host). It reuses the
// hygienic prompt discipline (system + the user's own turns + one pending prompt),
// but the pending prompt is web_fetch-aware: it names the target host and
// instructs the model to weigh domain reputation, biasing an unrecognized/
// low-reputation host toward ask/deny and a well-known documentation/reference
// host toward allow. knownGood carries the static floor's AllowNudge as a bias
// hint that is explicitly NOT an absolute bypass — a compromised subdomain of a
// good host must still be deniable. Verdicts are cached per session keyed by
// (toolName, rawArgs), so an identical pending fetch is classified at most once.
//
// Fail-closed contract matches Classify: a completer timeout/error or a
// malformed reply resolves to VerdictAsk.
func (c *Classifier) ClassifyWebFetch(ctx context.Context, userMessages []model.Message, host string, knownGood bool, rawArgs string) Verdict {
	if v, ok := c.cache.Lookup("web_fetch", rawArgs); ok {
		return v
	}
	req := model.CompletionReq{
		Model:     c.modelID,
		Messages:  c.buildWebFetchMessages(userMessages, host, knownGood),
		MaxTokens: classifierMaxTokens,
	}
	resp, err := c.client.Complete(ctx, req)
	var v Verdict
	if err != nil {
		v = VerdictAsk
	} else {
		v = parseVerdict(resp.Content)
	}
	c.cache.Store("web_fetch", rawArgs, v)
	return v
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

// webFetchPendingPrompt renders the web_fetch pending call as the final user turn,
// naming the target host and instructing domain-reputation weighting. The
// known-good hint is stated as a bias, not a bypass: a compromised subdomain of a
// good host must still be deniable.
func webFetchPendingPrompt(host string, knownGood bool) string {
	return fmt.Sprintf(
		"Pending tool call to classify:\ntool: web_fetch\ntarget host: %s\n\n"+
			"Weigh domain reputation: an unrecognized or low-reputation host biases toward \"ask\" or \"deny\"; "+
			"a well-known documentation/reference host biases toward \"allow\". "+
			"A known-good hint for this host is %t, but this hint is NOT an absolute bypass — "+
			"a compromised subdomain of an otherwise good host must still be deniable.\n\n"+
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
	msgs = append(msgs, model.Message{Role: "user", Content: pendingCallPrompt(toolName, command)})
	return msgs
}

// pendingCallPrompt renders the pending tool call as the final user turn the
// classifier judges.
func pendingCallPrompt(toolName, command string) string {
	return fmt.Sprintf("Pending tool call to classify:\ntool: %s\ncommand: %s\n\nRespond with the JSON verdict object.", toolName, command)
}

// verdictReply is the structured JSON shape the classifier asks the model to
// produce. Only "verdict" is load-bearing; reason is advisory and never shown
// to the actor.
type verdictReply struct {
	Verdict string `json:"verdict"`
	Reason  string `json:"reason"`
}

// parseVerdict defensively parses the model's reply into a Verdict. It accepts a
// single JSON object with a "verdict" field of allow/deny/ask (case-insensitive,
// whitespace-tolerant), tolerating surrounding prose by extracting the first
// balanced {...} object. Anything it cannot map to a clean allow or deny
// resolves to ask (block-biased): a missing object, a missing/unknown verdict,
// or a parse failure all fail closed.
func parseVerdict(content string) Verdict {
	obj, ok := firstJSONObject(content)
	if !ok {
		return VerdictAsk
	}
	var reply verdictReply
	if err := json.Unmarshal([]byte(obj), &reply); err != nil {
		return VerdictAsk
	}
	switch strings.ToLower(strings.TrimSpace(reply.Verdict)) {
	case "allow":
		return VerdictAllow
	case "deny":
		return VerdictDeny
	case "ask":
		return VerdictAsk
	default:
		// Missing or unrecognized verdict ⇒ fail closed.
		return VerdictAsk
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
	verdicts map[string]Verdict
}

// newVerdictCache builds an empty verdict cache.
func newVerdictCache() *verdictCache {
	return &verdictCache{verdicts: map[string]Verdict{}}
}

// Lookup returns the cached verdict for (toolName, command), keyed on the
// normalized command, and whether one was present.
func (c *verdictCache) Lookup(toolName, command string) (Verdict, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	v, ok := c.verdicts[cacheKey(toolName, command)]
	return v, ok
}

// Store records a verdict for (toolName, command) under the normalized key.
func (c *verdictCache) Store(toolName, command string, v Verdict) {
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
