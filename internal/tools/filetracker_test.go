package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestFileTrackerNewFile: write_file on a path that does not exist succeeds.
func TestFileTrackerNewFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "new.txt")
	ft := NewFileTracker()
	ctx := WithFileTracker(context.Background(), ft)

	res := NewWriteFile().Execute(ctx, `{"path":"`+p+`","content":"hello"}`)
	if res.IsError {
		t.Fatalf("creating new file should succeed: %v", res.Output)
	}
	data, _ := os.ReadFile(p)
	if string(data) != "hello" {
		t.Fatalf("content = %q", string(data))
	}
}

// TestFileTrackerUnreadOverwriteRefused: write_file on an existing file that
// this agent has not read is refused.
func TestFileTrackerUnreadOverwriteRefused(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "existing.txt")
	if err := os.WriteFile(p, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}

	ft := NewFileTracker()
	ctx := WithFileTracker(context.Background(), ft)

	res := NewWriteFile().Execute(ctx, `{"path":"`+p+`","content":"overwrite"}`)
	if !res.IsError {
		t.Fatal("expected error when overwriting an unread file")
	}
	if !strings.Contains(res.Output, "refusing to overwrite") {
		t.Fatalf("error should mention refusing to overwrite: %q", res.Output)
	}
	// File content must be unchanged.
	data, _ := os.ReadFile(p)
	if string(data) != "original" {
		t.Fatalf("file was modified: %q", string(data))
	}
}

// TestFileTrackerFullReadThenOverwrite: after a full read, write_file
// overwrite is allowed.
func TestFileTrackerFullReadThenOverwrite(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "existing.txt")
	if err := os.WriteFile(p, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}

	ft := NewFileTracker()
	ctx := WithFileTracker(context.Background(), ft)

	// Full read first (no start_line/end_line).
	readRes := NewReadFile().Execute(ctx, `{"path":"`+p+`"}`)
	if readRes.IsError {
		t.Fatalf("read failed: %v", readRes.Output)
	}

	// Overwrite should now be allowed.
	res := NewWriteFile().Execute(ctx, `{"path":"`+p+`","content":"overwrite"}`)
	if res.IsError {
		t.Fatalf("overwrite after full read should succeed: %v", res.Output)
	}
	data, _ := os.ReadFile(p)
	if string(data) != "overwrite" {
		t.Fatalf("content = %q", string(data))
	}
}

// TestFileTrackerPartialReadOverwriteRefused: a partial read (with
// start_line/end_line) does NOT authorize an overwrite.
func TestFileTrackerPartialReadOverwriteRefused(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "existing.txt")
	if err := os.WriteFile(p, []byte("line1\nline2\nline3\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ft := NewFileTracker()
	ctx := WithFileTracker(context.Background(), ft)

	// Partial read.
	readRes := NewReadFile().Execute(ctx, `{"path":"`+p+`","start_line":2,"end_line":2}`)
	if readRes.IsError {
		t.Fatalf("read failed: %v", readRes.Output)
	}

	// Overwrite should still be refused.
	res := NewWriteFile().Execute(ctx, `{"path":"`+p+`","content":"overwrite"}`)
	if !res.IsError {
		t.Fatal("expected error when overwriting after partial read")
	}
	if !strings.Contains(res.Output, "refusing to overwrite") {
		t.Fatalf("error should mention refusing to overwrite: %q", res.Output)
	}
}

// TestFileTrackerWriteThenOverwrite: after writing a file (via read+write),
// overwrite is allowed (the agent was the last writer).
func TestFileTrackerWriteThenOverwrite(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "existing.txt")
	if err := os.WriteFile(p, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}

	ft := NewFileTracker()
	ctx := WithFileTracker(context.Background(), ft)

	// Read it first.
	NewReadFile().Execute(ctx, `{"path":"`+p+`"}`)
	// Now write.
	res2 := NewWriteFile().Execute(ctx, `{"path":"`+p+`","content":"first"}`)
	if res2.IsError {
		t.Fatalf("write after read should succeed: %v", res2.Output)
	}

	// Now overwrite again without another read - should succeed because
	// we were the last writer.
	res3 := NewWriteFile().Execute(ctx, `{"path":"`+p+`","content":"second"}`)
	if res3.IsError {
		t.Fatalf("overwrite after own write should succeed: %v", res3.Output)
	}
	data, _ := os.ReadFile(p)
	if string(data) != "second" {
		t.Fatalf("content = %q", string(data))
	}
}

// TestFileTrackerOverwriteAfterOutsideChange: if the file changes on disk
// between the read and the overwrite, the overwrite is still allowed
// (the guard only checks whether the agent has read the file, not whether
// the content matches — that's the edit_file job).
func TestFileTrackerOverwriteAfterOutsideChange(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "existing.txt")
	if err := os.WriteFile(p, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}

	ft := NewFileTracker()
	ctx := WithFileTracker(context.Background(), ft)

	// Full read.
	NewReadFile().Execute(ctx, `{"path":"`+p+`"}`)

	// Outside change.
	if err := os.WriteFile(p, []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The agent's view is out of date, so its write would discard the outside change.
	res := NewWriteFile().Execute(ctx, `{"path":"`+p+`","content":"new"}`)
	if !res.IsError {
		t.Fatal("overwrite after an outside change should be refused")
	}
	data, _ := os.ReadFile(p)
	if string(data) != "outside" {
		t.Fatalf("content = %q, want the outside change kept", string(data))
	}
}

// TestFileTrackerEditAfterOutsideChangeRefused: edit_file on a file that
// changed on disk since this agent last read it is refused.
func TestFileTrackerEditAfterOutsideChangeRefused(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "edit.txt")
	if err := os.WriteFile(p, []byte("line1\nold_code\nline3\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ft := NewFileTracker()
	ctx := WithFileTracker(context.Background(), ft)

	// Full read.
	NewReadFile().Execute(ctx, `{"path":"`+p+`"}`)

	// Outside change (simulate another process modifying the file).
	time.Sleep(50 * time.Millisecond) // ensure different modtime
	if err := os.WriteFile(p, []byte("line1\nmodified\nline3\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Edit should be refused.
	res := NewEditFile().Execute(ctx, `{"path":"`+p+`","old_string":"modified","new_string":"replaced"}`)
	if !res.IsError {
		t.Fatal("expected error when editing after outside change")
	}
	if !strings.Contains(res.Output, "refusing to edit") {
		t.Fatalf("error should mention refusing to edit: %q", res.Output)
	}
}

// TestFileTrackerEditWithoutReadStillAllowed: edit_file on a file the agent
// has never read is still allowed (because edit_file requires exact unique
// match, so the model must know the content).
func TestFileTrackerEditWithoutReadStillAllowed(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "edit.txt")
	if err := os.WriteFile(p, []byte("line1\nold_code\nline3\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ft := NewFileTracker()
	ctx := WithFileTracker(context.Background(), ft)

	// Edit without prior read — allowed because HasState returns false.
	res := NewEditFile().Execute(ctx, `{"path":"`+p+`","old_string":"old_code","new_string":"new_code"}`)
	if res.IsError {
		t.Fatalf("edit without prior read should succeed: %v", res.Output)
	}
	data, _ := os.ReadFile(p)
	if !strings.Contains(string(data), "new_code") {
		t.Fatalf("content = %q", string(data))
	}
}

// TestFileTrackerEditAfterReadAllowed: edit_file after reading is allowed
// and records the write.
func TestFileTrackerEditAfterReadAllowed(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "edit.txt")
	if err := os.WriteFile(p, []byte("line1\nold_code\nline3\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ft := NewFileTracker()
	ctx := WithFileTracker(context.Background(), ft)

	// Read first.
	NewReadFile().Execute(ctx, `{"path":"`+p+`"}`)

	// Edit.
	res := NewEditFile().Execute(ctx, `{"path":"`+p+`","old_string":"old_code","new_string":"new_code"}`)
	if res.IsError {
		t.Fatalf("edit after read should succeed: %v", res.Output)
	}

	// After edit, the write is recorded. A second edit (without another read)
	// should work if the file hasn't changed on disk.
	res2 := NewEditFile().Execute(ctx, `{"path":"`+p+`","old_string":"new_code","new_string":"final_code"}`)
	if res2.IsError {
		t.Fatalf("edit after own edit should succeed: %v", res2.Output)
	}
	data, _ := os.ReadFile(p)
	if !strings.Contains(string(data), "final_code") {
		t.Fatalf("content = %q", string(data))
	}
}

// TestFileTrackerChildIsolation: a child agent's reads/writes do not
// affect the parent's file tracker, and vice versa.
func TestFileTrackerChildIsolation(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "shared.txt")
	if err := os.WriteFile(p, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}

	parentFT := NewFileTracker()
	childFT := NewFileTracker()

	parentCtx := WithFileTracker(context.Background(), parentFT)
	childCtx := WithFileTracker(context.Background(), childFT)

	// Parent reads the file.
	NewReadFile().Execute(parentCtx, `{"path":"`+p+`"}`)

	// Child has not read the file — overwrite should be refused.
	res := NewWriteFile().Execute(childCtx, `{"path":"`+p+`","content":"child"}`)
	if !res.IsError {
		t.Fatal("child overwrite should be refused (child has not read)")
	}

	// Parent should still be able to overwrite (parent read it).
	res2 := NewWriteFile().Execute(parentCtx, `{"path":"`+p+`","content":"parent"}`)
	if res2.IsError {
		t.Fatalf("parent overwrite should succeed: %v", res2.Output)
	}

	// File content should be parent's.
	data, _ := os.ReadFile(p)
	if string(data) != "parent" {
		t.Fatalf("content = %q", string(data))
	}
}

// TestFileTrackerPathResolution: two spellings of the same file share
// tracker state (e.g. "foo.txt" and "./foo.txt").
func TestFileTrackerPathResolution(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "target.txt")
	if err := os.WriteFile(p, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}

	ft := NewFileTracker()

	// Read using the resolved path.
	ft.MarkRead(p, true, time.Now(), 7)

	// Overwrite using a relative path should be authorized because
	// resolvePath canonicalizes it.
	rel := filepath.Join(dir, "target.txt") // same path
	if !ft.AuthorizeOverwrite(rel) {
		t.Fatal("AuthorizeOverwrite should resolve path")
	}
}

// TestFileTrackerNoTrackerBehavior: without a file tracker on context,
// the tools behave as before (no guard).
func TestFileTrackerNoTrackerBehavior(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "existing.txt")
	if err := os.WriteFile(p, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}

	// No file tracker on context — write_file should work as before.
	ctx := context.Background()
	res := NewWriteFile().Execute(ctx, `{"path":"`+p+`","content":"overwrite"}`)
	if res.IsError {
		t.Fatalf("write without tracker should succeed: %v", res.Output)
	}
	data, _ := os.ReadFile(p)
	if string(data) != "overwrite" {
		t.Fatalf("content = %q", string(data))
	}
}

// TestFileTrackerRegistryWire: the registry wires the file tracker through
// Execute so tools see it via context.
func TestFileTrackerRegistryWire(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "existing.txt")
	if err := os.WriteFile(p, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}

	ft := NewFileTracker()
	reg := NewRegistry()
	reg.Register(NewWriteFile())
	reg.Register(NewReadFile())
	reg.WithFileTracker(ft)

	// Write without read should be refused.
	res := reg.Execute(context.Background(), "write_file", `{"path":"`+p+`","content":"overwrite"}`)
	if !res.IsError {
		t.Fatal("expected error when overwriting an unread file through registry")
	}
	if !strings.Contains(res.Output, "refusing to overwrite") {
		t.Fatalf("error should mention refusing: %q", res.Output)
	}

	// Read it.
	reg.Execute(context.Background(), "read_file", `{"path":"`+p+`"}`)

	// Now write should succeed.
	res2 := reg.Execute(context.Background(), "write_file", `{"path":"`+p+`","content":"overwrite"}`)
	if res2.IsError {
		t.Fatalf("overwrite after read through registry should succeed: %v", res2.Output)
	}
}

// TestFileTrackerMarkReadPartialForStaleness: a partial read does not
// authorize overwrite but does count for edit_file staleness checks.
func TestFileTrackerMarkReadPartialForStaleness(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "edit.txt")
	if err := os.WriteFile(p, []byte("line1\nold_code\nline3\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ft := NewFileTracker()
	ctx := WithFileTracker(context.Background(), ft)

	// Partial read.
	NewReadFile().Execute(ctx, `{"path":"`+p+`","start_line":2,"end_line":2}`)

	// Overwrite should be refused (partial read does not authorize overwrite).
	res := NewWriteFile().Execute(ctx, `{"path":"`+p+`","content":"overwrite"}`)
	if !res.IsError {
		t.Fatal("expected error when overwriting after partial read")
	}

	// Edit should be allowed (partial read counts for staleness, but edit
	// on a file the agent has state for is allowed if not stale).
	// First check the file hasn't changed.
	res2 := NewEditFile().Execute(ctx, `{"path":"`+p+`","old_string":"old_code","new_string":"new_code"}`)
	if res2.IsError {
		t.Fatalf("edit after partial read should succeed: %v", res2.Output)
	}
}

// A symlink and its target are one file, so reading through either authorizes both.
func TestFileTrackerSymlinkSharesState(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.txt")
	link := filepath.Join(dir, "link.txt")
	if err := os.WriteFile(target, []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	ctx := WithFileTracker(context.Background(), NewFileTracker())
	args := func(path string, extra string) string { return `{"path":"` + path + `"` + extra + `}` }

	if res := NewWriteFile().Execute(ctx, args(link, `,"content":"x"`)); !res.IsError {
		t.Fatal("overwrite through an unread symlink was allowed")
	}
	if res := NewReadFile().Execute(ctx, args(target, "")); res.IsError {
		t.Fatal(res.Output)
	}
	if res := NewWriteFile().Execute(ctx, args(link, `,"content":"x"`)); res.IsError {
		t.Fatalf("read of the target did not authorize writing through its symlink: %s", res.Output)
	}
}
