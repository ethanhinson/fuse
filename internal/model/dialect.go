package model

import (
	"slices"
	"strings"
)

// A dialect is one model family: what it does on the wire on top of fuse's
// OpenAI-compatible request. The gateway speaks one wire format, but families
// differ in what that format should carry: Anthropic models cache a prompt prefix
// only where the request marks it, while OpenAI-style providers cache
// automatically. See docs/model-dialects.md.
//
// A dialect is a name, ID markers for detection, a struct of capability
// defaults, and two optional hooks. A model picks its dialect with the config key
// `dialect:`; unset, it is detected from the model ID (see DetectDialect). Its
// `dialect_options:` then overlay the family's capability defaults.
//
// Adding a family: create dialect_<name>.go declaring a `dialect` literal (name,
// markers, defaults) and list it in dialects before openAIDialect, whose position
// is the detection fallback. Use prepare/finish only for wire edits a capability
// flag cannot express; a behaviour several families could want, or a user should
// toggle, is a new capability instead. Then add dialect_<name>_test.go and update
// the config test that names the dialects and the docs table.
type dialect struct {
	name     string
	markers  []string // lower-case substrings of a model ID that select it; nil = never detected
	defaults capabilities
	// prepare, when set, edits the request after shared building and cache
	// marking, just before it is marshalled.
	prepare func(req *wireReq, c capabilities)
	// finish, when set, edits a parsed reply after shared parsing and tool-call
	// recovery, on every read path (streamed, buffered, unstreamed retry).
	finish func(resp *CompletionResp, tools []ToolSchema, c capabilities)
}

// capabilities is what a model family does on the wire, after a model's
// dialect_options are applied to the family's defaults.
type capabilities struct {
	cacheMarkers         bool   // mark prompt-cache breakpoints (markPromptCache)
	reasoningField       string // "" = replay reasoning in the field it arrived in; else always this field
	recoverToolCalls     bool   // lift tool calls written as XML text (recover.go)
	retryEmptyUnstreamed bool   // after an empty streamed reply, ask once more without streaming
	attachments          bool   // image input when config `attachments:` is unset and no `vision` tag
}

// resolvedDialect is a dialect with one model's options applied.
type resolvedDialect struct {
	*dialect
	caps capabilities
}

// Dialect names accepted by the config key `dialect:`.
const (
	DialectOpenAI    = "openai"
	DialectAnthropic = "anthropic"
)

// dialects lists every family. Order is detection priority; openAIDialect is
// last and is the fallback for an ID no other family claims.
var dialects = []*dialect{&anthropicDialect, &openAIDialect}

// DialectOptions are a model's per-family tuning (config `dialect_options:`).
// A nil pointer or empty string keeps the family default.
type DialectOptions struct {
	CacheMarkers     *bool
	ReasoningField   string
	RecoverToolCalls *bool
}

// DialectNames lists the dialects a model can be configured with, sorted.
func DialectNames() []string {
	names := make([]string, 0, len(dialects))
	for _, d := range dialects {
		names = append(names, d.name)
	}
	slices.Sort(names)
	return names
}

func lookupDialect(name string) *dialect {
	for _, d := range dialects {
		if d.name == name {
			return d
		}
	}
	return nil
}

// IsDialect reports whether name is a known dialect.
func IsDialect(name string) bool {
	return lookupDialect(name) != nil
}

// DetectDialect picks a dialect from a model ID when the config names none. It
// looks at the whole ID, so gateway aliases (cloud/claude-haiku-5.5,
// cloud/opus-5.5) and provider paths (openrouter/anthropic/...) both match.
func DetectDialect(modelID string) string {
	id := strings.ToLower(modelID)
	for _, d := range dialects {
		for _, marker := range d.markers {
			if strings.Contains(id, marker) {
				return d.name
			}
		}
	}
	return DialectOpenAI
}

// with overlays a model's options on the family defaults. A ReasoningField that
// is not a known field is ignored (config validation rejects it earlier).
func (c capabilities) with(o DialectOptions) capabilities {
	if o.CacheMarkers != nil {
		c.cacheMarkers = *o.CacheMarkers
	}
	if o.RecoverToolCalls != nil {
		c.recoverToolCalls = *o.RecoverToolCalls
	}
	if slices.Contains(ReasoningFields(), o.ReasoningField) {
		c.reasoningField = o.ReasoningField
	}
	return c
}

// resolveDialect returns the configured dialect, or the detected one when the
// config names none (or names one this build does not know), with the model's
// options applied to its capability defaults.
func resolveDialect(configured, modelID string, opts DialectOptions) resolvedDialect {
	d := lookupDialect(configured)
	if d == nil {
		d = lookupDialect(DetectDialect(modelID))
	}
	return resolvedDialect{dialect: d, caps: d.defaults.with(opts)}
}

// replayField is the wire field an assistant message's reasoning is sent back
// in: the dialect's fixed field when it has one, else the field it arrived in.
func (d resolvedDialect) replayField(arrived string) string {
	if d.caps.reasoningField != "" {
		return d.caps.reasoningField
	}
	return arrived
}

// prepareRequest edits the outgoing request just before it is marshalled. It
// must only add what its family needs: the request is otherwise already valid.
func (d resolvedDialect) prepareRequest(req *wireReq) {
	if d.caps.cacheMarkers {
		markPromptCache(req)
	}
	if d.prepare != nil {
		d.prepare(req, d.caps)
	}
}
