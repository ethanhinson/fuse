package session

import (
	"bufio"
	"bytes"
	"encoding/json"
	"time"
)

// logMeta is the archive.MetaFunc for session JSONL logs: it parses the
// LogEntry lines and returns the domain fields the sidecar records so the model
// (or a human) can tell WHAT is in an archived log without decompressing it.
//
// It is best-effort — a malformed line is skipped, never fatal — mirroring the
// non-fatal contract of the sweep that calls it.
func logMeta(content []byte) map[string]any {
	fields := map[string]any{}
	sc := bufio.NewScanner(bytes.NewReader(content))
	// Session logs can carry large tool-echo lines; raise the token cap well past
	// bufio's 64 KiB default so a big line does not silently truncate the parse.
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	var (
		count      int
		firstTS    time.Time
		lastTS     time.Time
		nodeIDs    = map[string]struct{}{}
		kindCounts = map[string]int{}
		rootLabel  string
		maxDepth   int
	)
	for sc.Scan() {
		line := sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var e LogEntry
		if err := json.Unmarshal(line, &e); err != nil {
			continue
		}
		count++
		if count == 1 {
			firstTS = e.TS
			rootLabel = e.Label
		}
		lastTS = e.TS
		if e.NodeID != "" {
			nodeIDs[e.NodeID] = struct{}{}
		}
		if e.Kind != "" {
			kindCounts[e.Kind]++
		}
		if e.Depth > maxDepth {
			maxDepth = e.Depth
		}
	}

	fields["entry_count"] = count
	if !firstTS.IsZero() {
		fields["first_ts"] = firstTS.UTC().Format(time.RFC3339)
	}
	if !lastTS.IsZero() {
		fields["last_ts"] = lastTS.UTC().Format(time.RFC3339)
	}
	fields["node_ids"] = sortedKeys(nodeIDs)
	fields["root_label"] = rootLabel
	fields["max_depth"] = maxDepth
	fields["kinds"] = kindCounts
	return fields
}

// sortedKeys returns the map's keys sorted, for deterministic sidecar output.
func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// Small sets; insertion sort keeps it dependency-free and deterministic.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}
