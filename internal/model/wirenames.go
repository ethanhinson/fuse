package model

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
)

// maxWireNameLen is the strictest tool-name limit among providers (OpenAI's
// function names; Anthropic allows 128).
const maxWireNameLen = 64

var safeWireName = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// wireNames maps fuse's internal tool names to names every provider accepts
// (^[a-zA-Z0-9_-]{1,64}$) and back. Internal names such as "mcp:server/tool"
// stay as-is everywhere in fuse; only the name sent to the model changes.
//
// A name's wire form depends only on the name and on which other names are in
// the request, never on their order, so it is byte-stable across turns:
//   - a safe name passes through;
//   - an unsafe name gets the plain encoding (":" and "/" become "__", any
//     other bad character "_");
//   - if the plain encoding is over 64 characters, equals a safe name, or is
//     shared by another distinct unsafe name in the request, the original
//     name's 8-hex sha256 suffix is appended (truncating to fit).
type wireNames struct {
	toWire   map[string]string
	toActual map[string]string
}

// newWireNames builds the mapping for one request from its advertised tools
// and every tool name in the replayed history.
func newWireNames(tools []ToolSchema, history []Message) *wireNames {
	var all []string
	for _, t := range tools {
		all = append(all, t.Name)
	}
	for _, m := range history {
		if m.Name != "" {
			all = append(all, m.Name)
		}
		for _, tc := range m.ToolCalls {
			all = append(all, tc.Name)
		}
	}
	return buildWireNames(all)
}

func buildWireNames(all []string) *wireNames {
	w := &wireNames{toWire: map[string]string{}, toActual: map[string]string{}}
	safe := map[string]bool{}
	unsafeByPlain := map[string]map[string]bool{}
	for _, n := range all {
		if safeWireName.MatchString(n) {
			safe[n] = true
			continue
		}
		p := encodeToolName(n)
		if unsafeByPlain[p] == nil {
			unsafeByPlain[p] = map[string]bool{}
		}
		unsafeByPlain[p][n] = true
	}
	for _, n := range all {
		if _, done := w.toWire[n]; done {
			continue
		}
		enc := n
		if !safe[n] {
			p := encodeToolName(n)
			if len(p) > maxWireNameLen || safe[p] || len(unsafeByPlain[p]) > 1 {
				enc = hashedWireName(p, n)
			} else {
				enc = p
			}
		}
		w.toWire[n] = enc
		w.toActual[enc] = n
	}
	return w
}

// encodeToolName rewrites the MCP separators ("mcp:server/tool") to "__" and
// any other disallowed character to "_".
func encodeToolName(name string) string {
	name = strings.NewReplacer(":", "__", "/", "__").Replace(name)
	var b strings.Builder
	for _, r := range name {
		if r == '_' || r == '-' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "_"
	}
	return b.String()
}

// hashedWireName truncates enc and appends a short hash of the original name.
func hashedWireName(enc, original string) string {
	sum := sha256.Sum256([]byte(original))
	suffix := "_" + hex.EncodeToString(sum[:4])
	if len(enc) > maxWireNameLen-len(suffix) {
		enc = enc[:maxWireNameLen-len(suffix)]
	}
	return enc + suffix
}

// wire returns the provider-safe name for an internal name. A name the
// mapping was not built from is encoded alone.
func (w *wireNames) wire(name string) string {
	if n, ok := w.toWire[name]; ok {
		return n
	}
	if safeWireName.MatchString(name) {
		return name
	}
	p := encodeToolName(name)
	if len(p) > maxWireNameLen {
		return hashedWireName(p, name)
	}
	return p
}

// actual returns the internal name for a wire name; an unknown wire name is
// returned unchanged so dispatch reports it as an unknown tool.
func (w *wireNames) actual(name string) string {
	if n, ok := w.toActual[name]; ok {
		return n
	}
	return name
}

// wireSchemas returns tools renamed to their wire names.
func (w *wireNames) wireSchemas(tools []ToolSchema) []ToolSchema {
	out := make([]ToolSchema, len(tools))
	for i, t := range tools {
		t.Name = w.wire(t.Name)
		out[i] = t
	}
	return out
}

// decode renames calls from wire to internal names in place.
func (w *wireNames) decode(calls []ToolCall) {
	for i := range calls {
		calls[i].Name = w.actual(calls[i].Name)
	}
}

// ResolveToolName maps a tool name as the model wrote it back to the internal
// name among advertised. An internal name is returned as is; a wire name of an
// advertised tool resolves to that tool; anything else is returned unchanged.
// Use it wherever a model-supplied argument names a tool (e.g. spawn_agent).
func ResolveToolName(advertised []string, name string) string {
	for _, a := range advertised {
		if a == name {
			return name
		}
	}
	return buildWireNames(advertised).actual(name)
}
