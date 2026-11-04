package tools

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ethanhinson/fuse/internal/artifact"
)

// Spill truncation: oversized tool results keep a head+tail preview inline
// while the FULL output is written to the loop's artifact store as a spill
// artifact the model can grep or read with line ranges by its art:// ref —
// truncation is lossless from the model's perspective. The context stays a
// cache over the store instead of the source of truth.
const (
	// spillThreshold is the inline budget per tool result.
	spillThreshold = 20 << 10
	// spillKeep is how much of the head and of the tail survive inline.
	// Tail matters as much as head: shell errors and summaries live at the end.
	spillKeep = 8 << 10
	// spillPutTimeout bounds the store write for one spilled result.
	spillPutTimeout = 10 * time.Second
)

// spillOutput bounds a tool result to the inline budget. Oversized output is
// cut middle-out (head + tail preserved). When b is a valid binding the
// complete output is saved as a spill artifact and the marker names its ref so
// the model can recover the elided middle — including by delegating to a
// subagent. Without a binding, or when the store refuses the write, the result
// is truncated only.
func spillOutput(ctx context.Context, b artifact.Binding, toolName, output string) string {
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
	if ref, ok := writeSpill(ctx, b, toolName, output); ok {
		marker += fmt.Sprintf(
			" Full output saved as artifact %s — read_file it with start_line/end_line or grep it with that ref, or spawn_agent a child to analyze it; do NOT read the whole artifact back.",
			ref)
	} else {
		marker += " Re-run the tool with a narrower scope for the elided middle."
	}
	marker += "]\n"

	return head + marker + tail
}

// writeSpill stores the full output through b. The write detaches from ctx's
// cancellation (bounded by its own timeout) so a timed-out or cancelled tool
// call still records its output. It reports false when b is unbound or the
// store refuses the write.
func writeSpill(ctx context.Context, b artifact.Binding, toolName, output string) (artifact.Ref, bool) {
	if !b.Valid() {
		return artifact.Ref{}, false
	}
	putCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), spillPutTimeout)
	defer cancel()
	meta, err := b.Put(putCtx, artifact.KindSpill, strings.NewReader(output), artifact.Meta{Tool: sanitizeToolName(toolName)})
	if err != nil {
		return artifact.Ref{}, false
	}
	return meta.Ref, true
}

// sanitizeToolName bounds and cleans a tool name for spill metadata.
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
