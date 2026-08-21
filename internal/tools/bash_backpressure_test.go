package tools

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/tools/sandbox"
)

// A wait past the threshold ⇒ the note is present exactly once, appended AFTER
// the command output, and the command output itself is unmodified.
func TestBashBackpressureNoteAppendedAboveThreshold(t *testing.T) {
	sub := &fakeSubstrate{runner: &fakeRunner{
		out: sandbox.Output{Combined: []byte("build ok\n"), ExitCode: 0, Waited: 4 * time.Second},
	}}
	b := newFakeBash(sub)
	b.noteThreshold = 2 * time.Second

	res := b.Execute(context.Background(), `{"command":"make"}`)
	if res.IsError {
		t.Fatalf("unexpected error result: %q", res.Output)
	}
	if !strings.HasPrefix(res.Output, "build ok\n") {
		t.Errorf("command output was modified: %q", res.Output)
	}
	if n := strings.Count(res.Output, "[sandbox: waited"); n != 1 {
		t.Errorf("note count = %d, want exactly 1 (output: %q)", n, res.Output)
	}
	// The note is AFTER the command output.
	if strings.Index(res.Output, "[sandbox:") < strings.Index(res.Output, "build ok") {
		t.Errorf("note is not appended after the output: %q", res.Output)
	}
	// The duration is rounded to a coarse unit — no sub-second noise.
	if !strings.Contains(res.Output, "waited 4s") {
		t.Errorf("note duration not coarse-rounded: %q", res.Output)
	}
}

// Below the threshold ⇒ no note; the uncontended case is byte-identical to
// today.
func TestBashNoNoteBelowThreshold(t *testing.T) {
	sub := &fakeSubstrate{runner: &fakeRunner{
		out: sandbox.Output{Combined: []byte("quick\n"), ExitCode: 0, Waited: 100 * time.Millisecond},
	}}
	b := newFakeBash(sub)
	b.noteThreshold = 2 * time.Second

	res := b.Execute(context.Background(), `{"command":"echo quick"}`)
	if res.Output != "quick\n" {
		t.Errorf("output = %q, want byte-identical %q (no note below threshold)", res.Output, "quick\n")
	}
}

// The note also attaches on the non-zero-exit path (an ordinary result), but the
// error message and exit status are preserved.
func TestBashBackpressureNoteOnNonZeroExit(t *testing.T) {
	sub := &fakeSubstrate{runner: &fakeRunner{
		out: sandbox.Output{Combined: []byte("nope\n"), ExitCode: 7, Waited: 5 * time.Second},
	}}
	b := newFakeBash(sub)
	b.noteThreshold = 2 * time.Second

	res := b.Execute(context.Background(), `{"command":"false"}`)
	if !res.IsError {
		t.Fatal("want an error result for a non-zero exit")
	}
	if !strings.Contains(res.Output, "exit status 7") {
		t.Errorf("exit status lost: %q", res.Output)
	}
	if !strings.Contains(res.Output, "[sandbox: waited") {
		t.Errorf("note missing on the non-zero-exit path: %q", res.Output)
	}
}

// A capacity refusal ⇒ IsError with the capacity message and no partial-output
// confusion. It names a recoverable condition, so a model retries rather than
// treating bash as broken.
func TestBashCapacityRefusalIsRecoverableError(t *testing.T) {
	sub := &fakeSubstrate{runner: &fakeRunner{
		out: sandbox.Output{ExitCode: -1},
		err: sandbox.ErrSandboxAtCapacity,
	}}
	b := newFakeBash(sub)

	res := b.Execute(context.Background(), `{"command":"echo hi"}`)
	if !res.IsError {
		t.Fatal("a capacity refusal must be an error result")
	}
	if !strings.Contains(res.Output, "at capacity") {
		t.Errorf("message does not name the capacity condition: %q", res.Output)
	}
	if !strings.Contains(res.Output, "retry") {
		t.Errorf("message does not suggest the recovery: %q", res.Output)
	}
	// It must NOT read as a timeout or a substrate error.
	if strings.Contains(res.Output, "timed out") || strings.Contains(res.Output, "error:") {
		t.Errorf("capacity refusal was miscategorised: %q", res.Output)
	}
	// The checkout was still handed back (no leak).
	if !sub.balanced() {
		t.Error("a capacity refusal leaked the checkout")
	}
}

// The note never appears in the sandbox Output.Combined handed to any event
// path: the fakeRunner's Combined is untouched by rendering.
func TestBashNoteNeverMutatesSandboxOutput(t *testing.T) {
	runner := &fakeRunner{
		out: sandbox.Output{Combined: []byte("data\n"), ExitCode: 0, Waited: 9 * time.Second},
	}
	sub := &fakeSubstrate{runner: runner}
	b := newFakeBash(sub)
	b.noteThreshold = 2 * time.Second

	_ = b.Execute(context.Background(), `{"command":"cat"}`)
	// The substrate Output the event path would read is unchanged.
	if string(runner.out.Combined) != "data\n" {
		t.Errorf("sandbox Output.Combined was mutated by note rendering: %q", runner.out.Combined)
	}
}
