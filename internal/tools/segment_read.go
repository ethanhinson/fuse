package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/ethanhinson/fuse/internal/artifact"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/segment"
)

// segmentReadMaxBytes bounds the raw-region text segment_read returns up front,
// before the registry's central spill truncation. Sized to a few big tool
// results so a broad turn_range does not dump a whole session.
const segmentReadMaxBytes = 32 << 10

// segmentReadTool recovers raw pre-compaction transcript regions the summarizer
// archived. It reads ONLY the running loop's segments, resolved per call through
// the artifact binding the registry stamps onto the tool's context, so a loop
// sees its own (tenant, loop) archive on whichever runtime serves it.
type segmentReadTool struct{}

// NewSegmentRead builds the segment_read tool. A call whose context carries no
// valid artifact binding reports that segment archiving is not active.
func NewSegmentRead() Tool { return segmentReadTool{} }

func (segmentReadTool) Name() string { return "segment_read" }

func (segmentReadTool) Description() string {
	return "Recover the raw transcript of an OLD tool-result region that context compaction " +
		"summarized and pruned. When a compacted summary references a turn range you need the " +
		"original detail from, call this with that turn_range instead of re-running the tool. " +
		"turn_range is a single turn (\"12\") or an inclusive range (\"12-18\"); tool_filter " +
		"narrows the recovered messages to one tool name (e.g. read_file)."
}

func (segmentReadTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"turn_range": map[string]any{
				"type":        "string",
				"description": "A single turn (\"12\") or an inclusive range (\"12-18\") to recover.",
			},
			"tool_filter": map[string]any{
				"type":        "string",
				"description": "Optional tool name; keep only recovered messages produced by this tool.",
			},
		},
		"required": []string{"turn_range"},
	}
}

type segmentReadArgs struct {
	TurnRange  string `json:"turn_range"`
	ToolFilter string `json:"tool_filter"`
}

func (segmentReadTool) Execute(ctx context.Context, args string) Result {
	var a segmentReadArgs
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return Result{IsError: true, Output: fmt.Sprintf("bad arguments: %v", err)}
	}
	lo, hi, ok := parseTurnRange(a.TurnRange)
	if !ok {
		return Result{IsError: true, Output: fmt.Sprintf("segment_read: bad turn_range %q — use \"12\" or \"12-18\"", a.TurnRange)}
	}

	binding, ok := artifact.BindingFrom(ctx)
	if !ok || !binding.Valid() {
		return Result{Output: "no segments — segment archiving is not active for this session"}
	}
	src := segment.NewArtifactSource(binding)

	idx, err := src.ReadIndex(ctx)
	if err != nil {
		// Fixed text: the store error can name a backend's host path, and the
		// tool result is durable.
		return Result{IsError: true, Output: "segment_read: reading the segment index failed"}
	}

	// Overlapping segments, oldest turn first for a stable, readable order.
	var matched []segment.IndexEntry
	for _, e := range idx.Segments {
		if e.TurnStart <= hi && lo <= e.TurnEnd {
			matched = append(matched, e)
		}
	}
	if len(matched) == 0 {
		return Result{Output: fmt.Sprintf("no segments overlap turns %s", a.TurnRange)}
	}
	sort.SliceStable(matched, func(i, j int) bool { return matched[i].TurnStart < matched[j].TurnStart })

	var b strings.Builder
	written := 0
	truncated := false
	remainingSegs := 0

	for i, e := range matched {
		if ctx.Err() != nil {
			break
		}
		seg, lerr := src.Load(ctx, e)
		if lerr != nil {
			continue // skip an unreadable segment rather than fail the whole call
		}
		msgs := filterByTool(seg.Messages, a.ToolFilter)
		if len(msgs) == 0 {
			continue
		}

		block := fmt.Sprintf("=== segment turns %d-%d ===\n%s\n", e.TurnStart, e.TurnEnd, segment.RenderRawRegion(msgs))
		if written+len(block) > segmentReadMaxBytes && written > 0 {
			truncated = true
			remainingSegs = len(matched) - i
			break
		}
		b.WriteString(block)
		written += len(block)
	}

	if b.Len() == 0 {
		return Result{Output: fmt.Sprintf("no segments overlap turns %s", a.TurnRange)}
	}
	if truncated {
		fmt.Fprintf(&b, "\n[%d more segments/messages match — narrow turn_range or set tool_filter]\n", remainingSegs)
	}
	return Result{Output: b.String()}
}

// filterByTool returns only the messages whose Name equals filter, or all of
// them when filter is empty.
func filterByTool(msgs []model.Message, filter string) []model.Message {
	if filter == "" {
		return msgs
	}
	out := make([]model.Message, 0, len(msgs))
	for _, m := range msgs {
		if m.Name == filter {
			out = append(out, m)
		}
	}
	return out
}

// parseTurnRange parses "N" (→ N,N) or "A-B" (→ A,B, ordered). Returns ok=false
// on malformed input.
func parseTurnRange(s string) (lo, hi int, ok bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, 0, false
	}
	if i := strings.IndexByte(s, '-'); i >= 0 {
		a, err1 := strconv.Atoi(strings.TrimSpace(s[:i]))
		b, err2 := strconv.Atoi(strings.TrimSpace(s[i+1:]))
		if err1 != nil || err2 != nil {
			return 0, 0, false
		}
		if a > b {
			a, b = b, a
		}
		return a, b, true
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, 0, false
	}
	return n, n, true
}

var _ Tool = segmentReadTool{}
