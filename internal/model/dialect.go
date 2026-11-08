package model

import (
	"slices"
	"strings"
)

// A dialect adapts fuse's OpenAI-compatible request to what one model family
// needs on top of it. The gateway speaks one wire format, but families differ
// in what that format should carry: Anthropic models cache a prompt prefix only
// where the request marks it, while OpenAI-style providers cache automatically.
//
// Each family is one implementation registered in dialects. A model picks its
// dialect with the config key `dialect:`; unset, it is detected from the model
// ID (see DetectDialect). Adding a family means adding a type here and an entry
// in dialects — nothing outside this package changes.
type dialect interface {
	// prepare edits the outgoing request just before it is marshalled. It must
	// only add what its family needs: the request is otherwise already valid.
	prepare(req *wireReq)
}

// Dialect names accepted by the config key `dialect:`.
const (
	DialectOpenAI    = "openai"
	DialectAnthropic = "anthropic"
)

var dialects = map[string]dialect{
	DialectOpenAI:    openAIDialect{},
	DialectAnthropic: anthropicDialect{},
}

// DialectNames lists the dialects a model can be configured with, sorted.
func DialectNames() []string {
	names := make([]string, 0, len(dialects))
	for n := range dialects {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

// IsDialect reports whether name is a known dialect.
func IsDialect(name string) bool {
	_, ok := dialects[name]
	return ok
}

// DetectDialect picks a dialect from a model ID when the config names none. It
// looks at the whole ID, so gateway aliases (cloud/claude-haiku-5.5,
// cloud/opus-5.5) and provider paths (openrouter/anthropic/...) both match.
func DetectDialect(modelID string) string {
	id := strings.ToLower(modelID)
	for _, marker := range []string{"anthropic", "claude", "opus", "sonnet", "haiku"} {
		if strings.Contains(id, marker) {
			return DialectAnthropic
		}
	}
	return DialectOpenAI
}

// resolveDialect returns the configured dialect, or the detected one when the
// config names none (or names one this build does not know).
func resolveDialect(configured, modelID string) dialect {
	if d, ok := dialects[configured]; ok {
		return d
	}
	return dialects[DetectDialect(modelID)]
}

// openAIDialect sends the request as is.
type openAIDialect struct{}

func (openAIDialect) prepare(*wireReq) {}

// anthropicDialect marks prompt-cache breakpoints. Anthropic caches a prefix
// only up to a block marked cache_control, so without marks every call pays full
// input price for a history it has already seen. LiteLLM and OpenRouter pass the
// marks through in the OpenAI content-part form used here (verified live: the
// second call reads the whole marked prefix from cache).
//
// It marks the system prompt, which also caches the tool definitions ahead of
// it, and the last two non-empty user or tool messages. The latest mark extends
// the cache to the whole history; the one before it gives the next call a cache
// read even when a turn adds more blocks than Anthropic's lookback reaches.
// That is three of the four marks Anthropic allows. Assistant messages are never
// marked, and neither is empty text, which Anthropic rejects.
type anthropicDialect struct{}

const anthropicHistoryMarks = 2

func (anthropicDialect) prepare(req *wireReq) {
	for i := range req.Messages {
		if req.Messages[i].Role == "system" && req.Messages[i].Content != "" {
			req.Messages[i].markCacheBreakpoint()
			break
		}
	}
	marked := 0
	for i := len(req.Messages) - 1; i >= 0 && marked < anthropicHistoryMarks; i-- {
		m := &req.Messages[i]
		if (m.Role == "user" || m.Role == "tool") && m.Content != "" {
			m.markCacheBreakpoint()
			marked++
		}
	}
}
