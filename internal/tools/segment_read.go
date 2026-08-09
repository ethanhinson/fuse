package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/segment"
)

// segmentsDir holds the running session's segments directory, set once at
// session start (keyed by the root AgentNode.ID) via SetSegmentsDir and read by
// the segment_read tool DefaultTools() registers. Mirrors SetSpillDir's
// package-level model — one process serves one session. Empty ⇒ segment
// archiving is off / no session, and segment_read returns a clean "no segments"
// result rather than an error.
var (
	segmentsMu  sync.RWMutex
	segmentsDir string
)

// SetSegmentsDir installs the running session's segments directory for the
// segment_read tool. Empty disables recovery (clean "no segments" result).
func SetSegmentsDir(dir string) {
	segmentsMu.Lock()
	segmentsDir = dir
	segmentsMu.Unlock()
}

// currentSegmentsDir returns the installed session segments dir (empty when
// unset).
func currentSegmentsDir() string {
	segmentsMu.RLock()
	defer segmentsMu.RUnlock()
	return segmentsDir
}

// segmentReadMaxBytes bounds the raw-region text segment_read returns up front,
// before the registry's central spill truncation. Sized to a few big tool
// results so a broad turn_range does not dump a whole session.
const segmentReadMaxBytes = 32 << 10

// segmentReadTool recovers raw pre-compaction transcript regions the summarizer
// archived (change 0030). It reads ONLY the running session's segments/ dir,
// resolved lazily via dirFn so it always sees the current session (the dir is
// keyed by the root AgentNode.ID at session start).
type segmentReadTool struct {
	dirFn func() string
}

// NewSegmentRead builds the segment_read tool. dirFn returns the running
// session's segments directory (empty ⇒ segment archiving is off / no session).
func NewSegmentRead(dirFn func() string) Tool {
	return segmentReadTool{dirFn: dirFn}
}

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

func (t segmentReadTool) Execute(ctx context.Context, args string) Result {
	var a segmentReadArgs
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return Result{IsError: true, Output: fmt.Sprintf("bad arguments: %v", err)}
	}
	lo, hi, ok := parseTurnRange(a.TurnRange)
	if !ok {
		return Result{IsError: true, Output: fmt.Sprintf("segment_read: bad turn_range %q — use \"12\" or \"12-18\"", a.TurnRange)}
	}

	segDir := ""
	if t.dirFn != nil {
		segDir = t.dirFn()
	}
	if segDir == "" {
		return Result{Output: "no segments — segment archiving is not active for this session"}
	}

	idx, err := segment.ReadIndex(segDir)
	if err != nil {
		return Result{IsError: true, Output: fmt.Sprintf("segment_read: reading index: %v", err)}
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
		seg, lerr := segment.LoadSegment(filepath.Join(segDir, e.Path))
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
