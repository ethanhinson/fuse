package model

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strconv"
	"strings"
)

// maxWireNameLen is the strictest tool-name limit among providers (OpenAI's
// function names; Anthropic allows 128).
const maxWireNameLen = 64

var safeWireName = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// wireNames maps fuse's internal tool names to names every provider accepts
// (^[a-zA-Z0-9_-]{1,64}$) and back. Internal names such as "mcp:server/tool"
// stay as-is everywhere in fuse; only the name sent to the model changes.
// The mapping is built per request from the advertised tool list, in order,
// so it is deterministic and byte-stable across turns.
type wireNames struct {
	toWire   map[string]string
	toActual map[string]string
}

// newWireNames encodes every tool name. Already-safe names pass through.
func newWireNames(tools []ToolSchema) *wireNames {
	w := &wireNames{toWire: map[string]string{}, toActual: map[string]string{}}
	// Safe names are reserved first so an encoded name can never displace one.
	for _, t := range tools {
		if safeWireName.MatchString(t.Name) {
			w.toWire[t.Name] = t.Name
			w.toActual[t.Name] = t.Name
		}
	}
	for _, t := range tools {
		if _, done := w.toWire[t.Name]; done {
			continue
		}
		enc := encodeToolName(t.Name)
		_, taken := w.toActual[enc]
		if taken || len(enc) > maxWireNameLen {
			base := enc
			enc = hashedWireName(base, t.Name)
			for n := 1; ; n++ {
				if _, taken := w.toActual[enc]; !taken {
					break
				}
				enc = hashedWireName(base, t.Name+"#"+strconv.Itoa(n))
			}
		}
		w.toWire[t.Name] = enc
		w.toActual[enc] = t.Name
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

// wire returns the provider-safe name for an internal name. A name not in the
// advertised list (history from a tool no longer offered) is encoded the same
// way, without collision handling.
func (w *wireNames) wire(name string) string {
	if n, ok := w.toWire[name]; ok {
		return n
	}
	if safeWireName.MatchString(name) {
		return name
	}
	enc := encodeToolName(name)
	if len(enc) > maxWireNameLen {
		enc = hashedWireName(enc, name)
	}
	return enc
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
