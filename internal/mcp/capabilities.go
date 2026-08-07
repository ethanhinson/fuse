package mcp

import (
	"encoding/json"
	"sort"
	"strings"
)

// clientProtocolVersion is the MCP protocol version fuse's client advertises in
// its initialize request. The server echoes it when recognized (fuse tolerates
// any version the server returns — capability negotiation fails open).
const clientProtocolVersion = "2025-03-26"

// ServerCapabilities is a server's advertised capability set, stored verbatim as
// the raw JSON of the initialize response's "capabilities" object. Lookups fail
// open: an unknown, absent, or malformed capability is reported as unsupported.
type ServerCapabilities struct {
	raw map[string]json.RawMessage
}

// Supports reports whether the server advertises a capability.
//
//	Supports("logging")             -> the top-level "logging" key is present (non-null)
//	Supports("resources.subscribe") -> "resources" is present AND its "subscribe" field == true
//
// Unknown or missing keys, and any malformed value, return false. A capability
// advertised as an explicit boolean uses that boolean; an object counts as
// supported.
func (c ServerCapabilities) Supports(key string) bool {
	top, rest, nested := strings.Cut(key, ".")
	raw, ok := c.raw[top]
	if !ok || len(raw) == 0 || string(raw) == "null" {
		return false
	}
	if !nested {
		// An explicit boolean advertisement uses its value; any other non-null
		// value (typically an object) counts as supported.
		var b bool
		if json.Unmarshal(raw, &b) == nil {
			return b
		}
		return true
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return false
	}
	v, ok := obj[rest]
	if !ok {
		return false
	}
	var b bool
	if err := json.Unmarshal(v, &b); err != nil {
		return false
	}
	return b
}

// Keys returns a sorted display list of advertised capabilities for the --live
// status surface: each present top-level key, with "resources" expanded to
// resources.subscribe / resources.listChanged when those nested flags are true.
func (c ServerCapabilities) Keys() []string {
	out := make([]string, 0, len(c.raw))
	for k, raw := range c.raw {
		if len(raw) == 0 || string(raw) == "null" {
			continue
		}
		out = append(out, k)
		if k == "resources" {
			for _, sub := range []string{"subscribe", "listChanged"} {
				if c.Supports("resources." + sub) {
					out = append(out, "resources."+sub)
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

// parseInitializeResult extracts the capabilities and protocol version from an
// initialize response. A missing or malformed capabilities object yields an
// empty (supports-nothing) set; this never returns an error — negotiation fails
// open.
func parseInitializeResult(raw json.RawMessage) (ServerCapabilities, string) {
	var res struct {
		ProtocolVersion string                     `json:"protocolVersion"`
		Capabilities    map[string]json.RawMessage `json:"capabilities"`
	}
	_ = json.Unmarshal(raw, &res)
	caps := ServerCapabilities{raw: res.Capabilities}
	if caps.raw == nil {
		caps.raw = map[string]json.RawMessage{}
	}
	return caps, res.ProtocolVersion
}
