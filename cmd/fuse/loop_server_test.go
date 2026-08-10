package main

import (
	"io"
	"strings"
	"testing"
)

// TestLoopServerDispatchRegistered proves `fuse loop-server` is wired into the
// dispatch switch and returns exit 0 when stdin hits EOF immediately (the server's
// Serve returns nil at EOF). stdin is an io.Pipe whose writer is closed at once, so
// the JSON-RPC decode sees EOF on the first read and Serve returns cleanly.
func TestLoopServerDispatchRegistered(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	// Immediately-EOF stdin: closing the writer makes the reader return io.EOF.
	pr, pw := io.Pipe()
	_ = pw.Close()

	// Swap os.Stdin for the duration so runLoopServer reads our EOF reader.
	oldStdin := stdinForLoopServer
	stdinForLoopServer = pr
	defer func() { stdinForLoopServer = oldStdin }()

	var out, errb strings.Builder
	code := run([]string{"loop-server"}, &out, &errb)
	if code != 0 {
		t.Fatalf("loop-server exit = %d, want 0; stderr:\n%s", code, errb.String())
	}
}

// TestHelpListsLoopServer proves the help output advertises the new subcommand.
func TestHelpListsLoopServer(t *testing.T) {
	var out, errb strings.Builder
	if code := run([]string{"help"}, &out, &errb); code != 0 {
		t.Fatalf("help exit = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "loop-server") {
		t.Fatalf("help output does not mention loop-server:\n%s", out.String())
	}
}
