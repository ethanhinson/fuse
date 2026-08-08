package agent

import "github.com/ethanhinson/fuse/internal/model"

// SegmentRegion is the raw pre-summarization span handed to a SegmentSink
// alongside the summary that replaced it and savings metadata (change 0027,
// widened per the 2026-08-08 spec amendment / #0030 D1). TurnStart/TurnEnd is
// the inclusive turn range compacted; ToolNames is the distinct tool names in
// the region (an index grep key for #0030); TokensBefore/TokensAfter is the
// estimated size before and after compaction.
type SegmentRegion struct {
	TurnStart, TurnEnd int
	Messages           []model.Message
	Summary            string
	ToolNames          []string
	TokensBefore       int
	TokensAfter        int
}

// SegmentSink persists the raw pre-summarization region and returns a recovery
// pointer (a path the summary can reference) or "" if nothing was persisted.
// #0027 ships only the no-op default; #0030 implements a real disk-backed sink.
// Archive errors are best-effort — the loop logs them, never fails the turn.
type SegmentSink interface {
	Archive(r SegmentRegion) (pointer string, err error)
}

// noopSegmentSink is the default sink: it persists nothing and returns an empty
// pointer, so the injected summary omits the "grep your past at <path>" line.
type noopSegmentSink struct{}

func (noopSegmentSink) Archive(SegmentRegion) (string, error) { return "", nil }
