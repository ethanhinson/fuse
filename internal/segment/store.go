// Package segment implements the concrete filesystem SegmentSink for the
// pre-compaction transcript archive (change 0030). It is deliberately OUTSIDE
// internal/agent (which only DEFINES the SegmentSink interface) so cmd/fuse can
// construct it and inject it without an import cycle.
package segment

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/ethanhinson/fuse/internal/model"
)

// tsLayout is the RFC3339 (UTC, second precision) timestamp format written into
// the segment front-matter and index.
const tsLayout = "2006-01-02T15:04:05Z"

// Segment is one archived pre-summarization region: the front-matter metadata,
// the ODSNF summary that replaced it, and the raw region messages.
type Segment struct {
	TurnStart    int
	TurnEnd      int
	Tools        []string
	TokensBefore int
	TokensAfter  int
	TS           time.Time
	Summary      string
	Messages     []model.Message
}

// IndexEntry is one segment's row in a session's index.json. Path is relative to
// the session's segments/ directory.
type IndexEntry struct {
	TurnStart    int       `json:"turn_start"`
	TurnEnd      int       `json:"turn_end"`
	Tools        []string  `json:"tools"`
	TokensBefore int       `json:"tokens_before"`
	TokensAfter  int       `json:"tokens_after"`
	Path         string    `json:"path"`
	TS           time.Time `json:"ts"`
}

// Index is a session's segment index (one index.json per session).
type Index struct {
	SessionID string       `json:"session_id"`
	Segments  []IndexEntry `json:"segments"`
}

// FileName is the LOGICAL segment file name for a turn range and disambiguating
// seq: "<turnStart>-<turnEnd>-<seq>.md". This is the name recorded in the index
// and used to probe for collisions; the ON-DISK file is written gzip-compressed
// with the ".gz" suffix (see FileNameGz) after the change 0030 scope expansion.
func FileName(turnStart, turnEnd, seq int) string {
	return fmt.Sprintf("%d-%d-%d.md", turnStart, turnEnd, seq)
}

// GzSuffix is appended to the logical segment name for the on-disk compressed
// file. Segments are born compressed (creation-time compression, change 0030
// scope expansion): the sink writes RenderSegment output through gzip and the
// reader gunzips transparently, so the whole recovery path is unchanged.
const GzSuffix = ".gz"

// FileNameGz is the ON-DISK segment file name: FileName + GzSuffix.
func FileNameGz(turnStart, turnEnd, seq int) string {
	return FileName(turnStart, turnEnd, seq) + GzSuffix
}

// SegmentsDir returns the segments directory for a session:
// <baseDir>/<sessionID>/segments.
func SegmentsDir(baseDir, sessionID string) string {
	return filepath.Join(baseDir, sessionID, "segments")
}

// IndexFileName is the per-session segment index file name.
const IndexFileName = "index.json"

// rawRegionFence is the language tag on the fenced code block that carries the
// raw region's on-disk JSON encoding inside the "## Raw region" section.
const rawRegionFence = "```json"

// RenderSegment renders a segment to its on-disk markdown form: YAML
// front-matter, the ## Summary section, and the ## Raw region section. The raw
// region is stored as a self-delimiting JSON array of messages inside a fenced
// code block so reconstruction (LoadSegment) is exact and lossless — arbitrary
// tool output (including lines that look like message headers) round-trips
// byte-for-byte. Sanitizing/pretty-printing is a TUI render concern (see
// RenderRawRegion), not a storage one.
func RenderSegment(s Segment) string {
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "turn_start: %d\n", s.TurnStart)
	fmt.Fprintf(&b, "turn_end: %d\n", s.TurnEnd)
	fmt.Fprintf(&b, "tools: [%s]\n", strings.Join(s.Tools, ", "))
	fmt.Fprintf(&b, "tokens_before: %d\n", s.TokensBefore)
	fmt.Fprintf(&b, "tokens_after: %d\n", s.TokensAfter)
	fmt.Fprintf(&b, "ts: %s\n", s.TS.UTC().Format(tsLayout))
	b.WriteString("---\n\n")

	b.WriteString("## Summary\n\n")
	b.WriteString(strings.TrimSpace(s.Summary))
	b.WriteString("\n\n")

	b.WriteString("## Raw region\n\n")
	b.WriteString(renderRawRegionStore(s.Messages))
	if !strings.HasSuffix(b.String(), "\n") {
		b.WriteString("\n")
	}
	return b.String()
}

// renderRawRegionStore encodes the raw region for ON-DISK storage: the message
// slice as a JSON array inside a ```json fenced code block. JSON is
// self-delimiting, so LoadSegment reconstructs every field (Role, Name, Content,
// ToolCallID, ToolCalls) exactly regardless of what the content contains — no
// line-scanning heuristic that arbitrary tool output could spoof. This is the
// storage format; RenderRawRegion is the separate human-readable display form.
func renderRawRegionStore(msgs []model.Message) string {
	if msgs == nil {
		msgs = []model.Message{}
	}
	enc, err := json.MarshalIndent(msgs, "", "  ")
	if err != nil {
		// model.Message is plain data; marshaling cannot realistically fail.
		enc = []byte("[]")
	}
	var b strings.Builder
	b.WriteString(rawRegionFence)
	b.WriteString("\n")
	b.Write(enc)
	b.WriteString("\n```\n")
	return b.String()
}

// RenderRawRegion renders a message region as parseable-but-readable blocks:
// each message opens with a header line "<role> [<name>]:" (the "[<name>]" part
// omitted for messages without a tool name), followed by the raw content on the
// next lines, then a blank separator. This is the HUMAN-READABLE display form
// used by segment_read's tool output and the TUI show-original drill-in — it is
// NOT the on-disk storage format (see renderRawRegionStore / LoadSegment) and is
// never parsed back.
func RenderRawRegion(msgs []model.Message) string {
	var b strings.Builder
	for _, m := range msgs {
		b.WriteString(m.Role)
		if m.Name != "" {
			b.WriteString(" [")
			b.WriteString(m.Name)
			b.WriteString("]")
		}
		b.WriteString(":\n")
		b.WriteString(m.Content)
		b.WriteString("\n\n")
	}
	return b.String()
}
