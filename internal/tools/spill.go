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
	// spawnSpillThreshold is the tighter inline budget for a spawn_agent
	// result: a child's final report lands in the parent's context and is
	// re-sent on every later model call, so a report that fits the ordinary
	// budget still floods the parent's prompt (observed live: eight reports
	// of up to 19.5KB became 48% of the root's context and tripled its
	// prompt tokens). The complete report is saved as a spill artifact the
	// parent recovers through its art:// ref, exactly like any other spill.
	spawnSpillThreshold = 6 << 10
	// spawnSpillKeep is the head/tail keep for a spilled spawn report.
	spawnSpillKeep = 2 << 10
	// spillPutTimeout bounds the store write for one spilled result.
	spillPutTimeout = 10 * time.Second
)

// spillBudget is the inline budget for one tool result: the byte ceiling at
// which spilling kicks in and how much of the head and tail survive inline.
type spillBudget struct {
	threshold, keep int
}

// spillBudgetFor returns the inline budget for toolName's results.
// spawn_agent gets the tighter spawn budget (see spawnSpillThreshold); every
// other tool keeps the ordinary 20KB budget.
func spillBudgetFor(toolName string) spillBudget {
	if toolName == "spawn_agent" {
		return spillBudget{spawnSpillThreshold, spawnSpillKeep}
	}
	return spillBudget{spillThreshold, spillKeep}
}

// spillOutput bounds a tool result to the inline budget. Oversized output is
// cut middle-out (head + tail preserved). When b is a valid binding the
// complete output is saved as a spill artifact and the marker names its ref so
// the model can recover the elided middle. Without a binding, or when the store refuses the write, the result
// is truncated only.
func spillOutput(ctx context.Context, b artifact.Binding, toolName, output string) string {
	budget := spillBudgetFor(toolName)
	if len(output) <= budget.threshold {
		return output
	}
	head := truncateBytes(output, budget.keep)
	tailStart := len(output) - budget.keep
	for tailStart < len(output) && output[tailStart]&0xC0 == 0x80 {
		tailStart++ // start the tail on a rune boundary
	}
	tail := output[tailStart:]

	marker := fmt.Sprintf(
		"\n[fuse: output truncated — showing first %dKB and last %dKB of %dKB.",
		budget.keep>>10, budget.keep>>10, len(output)>>10)
	if ref, ok := writeSpill(ctx, b, toolName, output); ok {
		marker += fmt.Sprintf(
			" Full output saved as artifact %s — read_file it with start_line/end_line or grep it with that ref; do NOT read the whole artifact back.",
			ref)
	} else {
		if toolName == "spawn_agent" {
			// Re-running a child to recover a report would cost more than it saves.
			marker += " The elided middle of this report was not saved."
		} else {
			marker += " Re-run the tool with a narrower scope for the elided middle."
		}
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
