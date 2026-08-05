package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// Spill-file truncation: oversized tool results keep a head+tail preview
// inline while the FULL output is written to a spill file the model can
// grep or read with line ranges — truncation is lossless from the model's
// perspective (OpenCode/grok-build pattern). The context stays a cache over
// disk instead of the source of truth.
const (
	// spillThreshold is the inline budget per tool result.
	spillThreshold = 20 << 10
	// spillKeep is how much of the head and of the tail survive inline.
	// Tail matters as much as head: shell errors and summaries live at the end.
	spillKeep = 8 << 10
	// spillMaxAge is the GC horizon for spill files.
	spillMaxAge = 7 * 24 * time.Hour
)

var (
	spillMu  sync.RWMutex
	spillDir string
	spillSeq atomic.Int64
)

// SetSpillDir installs the directory for full-output spill files and sweeps
// entries older than the GC horizon. Empty dir disables spilling (results
// are still truncated, just without a recovery file).
func SetSpillDir(dir string) {
	spillMu.Lock()
	spillDir = dir
	spillMu.Unlock()
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	go sweepSpillDir(dir)
}

func sweepSpillDir(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-spillMaxAge)
	for _, e := range entries {
		info, err := e.Info()
		if err == nil && info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// SpillOutput bounds a tool result to the inline budget. Oversized output is
// cut middle-out (head + tail preserved) and, when a spill dir is configured,
// the complete output is saved and the marker tells the model how to recover
// the elided middle — including by delegating to a subagent.
func SpillOutput(toolName, output string) string {
	if len(output) <= spillThreshold {
		return output
	}
	head := truncateBytes(output, spillKeep)
	tailStart := len(output) - spillKeep
	for tailStart < len(output) && output[tailStart]&0xC0 == 0x80 {
		tailStart++ // start the tail on a rune boundary
	}
	tail := output[tailStart:]

	marker := fmt.Sprintf(
		"\n[fuse: output truncated — showing first %dKB and last %dKB of %dKB.",
		spillKeep>>10, spillKeep>>10, len(output)>>10)
	if path := writeSpill(toolName, output); path != "" {
		marker += fmt.Sprintf(
			" Full output saved to: %s — grep it, read_file specific line ranges, or spawn_agent a child to analyze it; do NOT read the whole file back.",
			path)
	} else {
		marker += " Re-run the tool with a narrower scope for the elided middle."
	}
	marker += "]\n"

	return head + marker + tail
}

// writeSpill persists the full output; returns "" when disabled or on error.
func writeSpill(toolName string, output string) string {
	spillMu.RLock()
	dir := spillDir
	spillMu.RUnlock()
	if dir == "" {
		return ""
	}
	name := fmt.Sprintf("%d_%03d_%s.txt", time.Now().Unix(), spillSeq.Add(1), sanitizeToolName(toolName))
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(output), 0o600); err != nil {
		return ""
	}
	return path
}

// sanitizeToolName keeps spill filenames shell-friendly.
func sanitizeToolName(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s) && i < 32; i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' {
			out = append(out, c)
		} else {
			out = append(out, '_')
		}
	}
	if len(out) == 0 {
		return "tool"
	}
	return string(out)
}
