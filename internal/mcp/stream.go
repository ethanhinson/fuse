package mcp

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Stream buffering bounds (D3). A streaming tool's $/stream chunks assemble into
// a per-call ring that keeps the head and the tail with a truncation marker in
// between when the total exceeds the cap — consistent with internal/tools/spill.go
// (tail matters: a stream's final summary/error lives at the end).
const (
	// streamRingCap is the total inline budget for one call's assembled stream.
	streamRingCap = 64 << 10
	// streamRingKeep is how much of the head and of the tail survive on overflow.
	streamRingKeep = 16 << 10
	// streamPartialTail is how many bytes the rolling-partial accessor exposes
	// for the TUI (the most recent content, sanitized).
	streamPartialTail = 4 << 10
)

// streamNotifyMethod is the JSON-RPC method for MCP stream-delta notifications.
const streamNotifyMethod = "$/stream"

// streamBuffer accumulates ordered delta chunks for a single call. A single
// pump goroutine appends, so no reordering logic is needed. On overflow it keeps
// a bounded head and a rolling tail; assemble() renders head + marker + tail.
type streamBuffer struct {
	head      []byte // first streamRingKeep bytes (frozen once full)
	tail      []byte // rolling last streamRingKeep bytes
	total     int    // total bytes appended (pre-truncation)
	truncated bool
}

// append adds a delta chunk. Once head is full, further bytes roll through tail.
func (b *streamBuffer) append(delta string) {
	b.total += len(delta)
	data := []byte(delta)

	// Fill the head first.
	if len(b.head) < streamRingKeep {
		room := streamRingKeep - len(b.head)
		if room >= len(data) {
			b.head = append(b.head, data...)
			return
		}
		b.head = append(b.head, data[:room]...)
		data = data[room:]
	}

	// Remaining bytes roll into the tail, capped at streamRingKeep (drop oldest).
	b.truncated = true
	b.tail = append(b.tail, data...)
	if len(b.tail) > streamRingKeep {
		b.tail = b.tail[len(b.tail)-streamRingKeep:]
	}
}

// assemble renders the complete buffered stream. When truncation occurred, the
// elided middle is replaced by a marker (matching the spill.go head+tail style).
func (b *streamBuffer) assemble() string {
	if !b.truncated {
		return string(b.head)
	}
	marker := fmt.Sprintf(
		"\n[fuse: stream truncated — showing first %dKB and last %dKB of %dKB]\n",
		streamRingKeep>>10, streamRingKeep>>10, b.total>>10)
	return string(b.head) + marker + string(b.tail)
}

// partial returns the most recent content (rolling tail) for the TUI. It is the
// raw bytes; callers sanitize before a fixed-width render.
func (b *streamBuffer) partial() string {
	// Prefer the rolling tail; fall back to the head before any overflow.
	src := b.tail
	if len(src) == 0 {
		src = b.head
	}
	if len(src) > streamPartialTail {
		src = src[len(src)-streamPartialTail:]
	}
	return string(src)
}

// streamParams is the wire shape of a $/stream notification's params.
type streamParams struct {
	ProgressToken string `json:"progressToken"`
	Delta         string `json:"delta"`
}

// handleStream is the router handler for "$/stream". It appends the delta to the
// matched call's ring buffer. An unmatched, late, or malformed notification is
// silently dropped (fail-open); a missing delta is a no-op.
func (m *Manager) handleStream(server string, params json.RawMessage) {
	var p streamParams
	if err := json.Unmarshal(params, &p); err != nil || p.ProgressToken == "" || p.Delta == "" {
		return
	}
	ct := m.trackingFor(p.ProgressToken)
	if ct == nil {
		return // unmatched or late
	}
	ct.mu.Lock()
	if ct.stream == nil {
		ct.stream = &streamBuffer{}
	}
	ct.stream.append(p.Delta)
	ct.mu.Unlock()
}

// StreamPartial returns the rolling-partial content for an in-flight streaming
// call, sanitized for a fixed-width TUI render (the same sanitizer discipline the
// TUI uses on tool output). Empty when the token is unknown or no chunks arrived.
func (m *Manager) StreamPartial(token string) string {
	ct := m.trackingFor(token)
	if ct == nil {
		return ""
	}
	ct.mu.Lock()
	defer ct.mu.Unlock()
	if ct.stream == nil {
		return ""
	}
	return sanitizeStreamChunk(ct.stream.partial())
}

// sanitizeStreamChunk strips control bytes from an untrusted $/stream chunk
// before any fixed-width TUI render: ESC sequences, C0/C1 controls, and CR are
// removed; tabs expand to spaces; newlines survive. This mirrors the TUI's
// sanitizeDisplay discipline (kept in-package to avoid an import cycle: the TUI
// depends on mcp, not the reverse).
func sanitizeStreamChunk(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s { // range replaces invalid UTF-8 with U+FFFD
		switch {
		case r == '\n':
			b.WriteRune(r)
		case r == '\t':
			b.WriteString("    ")
		case r == '\r':
			// drop CR — it rewinds the cursor and shears fixed-width panes
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f):
			// C0/C1 controls (incl. ESC 0x1b) and DEL become nothing
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
