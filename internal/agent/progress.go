package agent

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
)

// DefaultNoProgressCalls is how many tool calls a run may make without a file
// change before the loop nudges the model. On real runs that finished, the
// longest stretch without an edit was 40 to 77 calls, so 100 leaves room for
// ordinary exploration and still fires within minutes on a run that has wandered.
const DefaultNoProgressCalls = 100

// ErrRunTokenLimit ends a run that has used its configured token allowance. The
// allowance covers the whole agent tree, so a parent and its children share it.
var ErrRunTokenLimit = errors.New("agent: run token limit reached")

// runProgress is shared by every agent in one run. A per-agent count misses a run
// that fans out: 1,828 calls spread over 49 children averages 37 calls each, and
// no single loop ever looks stuck.
type runProgress struct {
	callsSinceChange atomic.Int64
	tokens           atomic.Int64
}

type runProgressKey struct{}

// withRunProgress returns ctx carrying the run's tracker, creating one for a root
// agent. A child's context derives from its parent's, so it finds the same one.
func withRunProgress(ctx context.Context) (context.Context, *runProgress) {
	if p, ok := ctx.Value(runProgressKey{}).(*runProgress); ok {
		return ctx, p
	}
	p := &runProgress{}
	return context.WithValue(ctx, runProgressKey{}, p), p
}

// recordCalls adds a turn's tool calls and returns the calls since the last file
// change anywhere in the run.
func (p *runProgress) recordCalls(results []toolResult) int64 {
	changed := false
	for _, r := range results {
		if !r.res.IsError && (r.call.Name == "edit_file" || r.call.Name == "write_file") {
			changed = true
			break
		}
	}
	if changed {
		p.callsSinceChange.Store(0)
		return 0
	}
	return p.callsSinceChange.Add(int64(len(results)))
}

// noProgressNudge is the message injected when a run has gone too long without a
// file change. It never ends the run: investigation is a legitimate task.
func noProgressNudge(calls int64) string {
	return fmt.Sprintf("[no-progress] This run has made %d tool calls without changing a file through edit_file or write_file. "+
		"If the task needs a code change, make it now using what you already know. "+
		"If it only needed investigation, finish with your answer. Do not spawn more agents to research further.", calls)
}
