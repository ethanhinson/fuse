package e2e

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

// script returns a server and harness, with steps set later once paths are known.
func scripted(t *testing.T) (*server, *harness) {
	t.Helper()
	srv := newServer(t)
	return srv, newHarness(t, srv)
}

func (s *server) script(steps ...step) { s.steps = steps }

func TestExit_UsageAndStartupErrors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		args   []string
		config string // raw config, "" keeps the default
		want   int
		output string
	}{
		{name: "no_task", args: nil, want: 2, output: "usage"},
		{name: "removed_subcommand", args: []string{"orchestrate"}, want: 2, output: "was removed"},
		{name: "version", args: []string{"version"}, want: 0, output: "fuse "},
		{name: "unknown_model", args: []string{"--model", "nope", "task"}, want: 1, output: "nope"},
		{name: "bad_yaml", args: []string{"task"}, config: "gateway: [unterminated", want: 1, output: "config"},
		{name: "bad_permission_mode", args: []string{"task"}, config: "permissions:\n  mode: wide-open\n", want: 1, output: "mode"},
		{name: "unwritable_trace", args: []string{"--model", "m", "--trace", "/nonexistent-dir/x/trace", "task"}, want: 1, output: "trace"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			srv := newServer(t)
			h := newHarness(t, srv)
			h.writeConfig()
			if c.config != "" {
				h.write(h.home+"/.fuse/config.yml", c.config, 0o600)
			}
			r := h.startNoConfig(defaultTimeout, c.args...).wait()
			wantCode(t, r, c.want)
			wantContains(t, r.all(), c.output, "output")
			if srv.count() != 0 {
				t.Fatalf("model called %d times on a startup failure", srv.count())
			}
		})
	}
}

func TestMaxRunTokens_Nonzero(t *testing.T) {
	t.Parallel()
	bigReply := func(w http.ResponseWriter, r *http.Request) {
		sseHeader(w)
		sse(w, toolCallChunk(0, call{"call_1", "list_directory", `{"path":"."}`}))
		sse(w, finishChunk("tool_calls"))
		sse(w, usageChunk(4000, 1000))
		done(w)
	}
	srv := newRouter(t, func(*request) step { return bigReply })
	h := newHarness(t, srv)
	h.extra = "max_run_tokens: 3000\n"
	r := h.run()
	wantCode(t, r, 1)
	if srv.count() != 1 {
		t.Fatalf("requests = %d, want the run stopped by the first usage report", srv.count())
	}
	wantContains(t, r.stderr, "3000", "stderr")
}

func TestMaxTurns_Nonzero(t *testing.T) {
	t.Parallel()
	srv := newRouter(t, func(r *request) step {
		return tool("list_directory", fmt.Sprintf(`{"path":".","n":%d}`, r.N))
	})
	h := newHarness(t, srv)
	h.extra = "max_turns: 3\n"
	r := h.run()
	wantCode(t, r, 1)
	if srv.count() != 3 {
		t.Fatalf("requests = %d, want 3", srv.count())
	}
	wantContains(t, r.stderr, "max turns", "stderr")
}

func TestApproveAllAbsent_StdinClosed_RefusesMutatingTools(t *testing.T) {
	t.Parallel()
	cases := map[string]func(h *harness) step{
		"write_file": func(h *harness) step {
			return tool("write_file", map[string]string{"path": h.path("new.txt"), "content": "x"})
		},
		"edit_file": func(h *harness) step {
			h.write(h.path("new.txt"), "aaa", 0o644)
			return tool("edit_file", map[string]string{"path": h.path("new.txt"), "old_string": "aaa", "new_string": "x"})
		},
		"bash": func(h *harness) step {
			h.hostSandbox()
			return tool("bash", map[string]string{"command": "echo x > new.txt"})
		},
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv, h := scripted(t)
			srv.script(mk(h), text("gave up"))
			r := h.run()
			wantCode(t, r, 0)
			wantContains(t, h.toolOutput(), "requires approval", "tool result")
			if b, err := os.ReadFile(h.path("new.txt")); err == nil && strings.Contains(string(b), "x") {
				t.Fatalf("%s ran without approval", name)
			}
		})
	}
}

var signals = map[string]syscall.Signal{"SIGTERM": syscall.SIGTERM, "SIGINT": syscall.SIGINT}

func TestSignal_DuringModelCall_ExitsPromptlyNonzero(t *testing.T) {
	t.Parallel()
	for name, sig := range signals {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := newServer(t)
			srv.route = func(*request) step { return srv.hang() }
			h := newHarness(t, srv)
			h.write(h.path("keep.txt"), "keep me", 0o644)
			p := h.start(defaultTimeout, "--model", "m", "e2e task")
			srv.waitForRequests(t, 1, 10*time.Second)
			at := time.Now()
			p.signal(sig)
			r := p.wait()
			if took := time.Since(at); took > 3*time.Second {
				t.Fatalf("took %s to exit after %s", took, name)
			}
			if r.code == 0 {
				t.Fatalf("exit 0 after %s", name)
			}
			if got := readFile(t, h.path("keep.txt")); got != "keep me" {
				t.Fatalf("file changed: %q", got)
			}
		})
	}
}

// No signal handler exists: the Go runtime kills the process, so nothing tears
// down, nothing is reported and the conventional 128+N exit status never appears.
func TestSignal_DuringModelCall_HandledGracefully(t *testing.T) {
	defect(t, "no signal handling in one-shot: StartLoop runs on context.Background() and SIGTERM/SIGINT kill the process outright (cmd/fuse/main.go:217)")
	t.Parallel()
	want := map[string]int{"SIGTERM": 143, "SIGINT": 130}
	for name, sig := range signals {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := newServer(t)
			srv.route = func(*request) step { return srv.hang() }
			h := newHarness(t, srv)
			p := h.start(defaultTimeout, "--model", "m", "e2e task")
			srv.waitForRequests(t, 1, 10*time.Second)
			p.signal(sig)
			r := p.wait()
			if r.signaled || r.code != want[name] {
				t.Fatalf("signaled=%v code=%d, want a handled exit with code %d", r.signaled, r.code, want[name])
			}
		})
	}
}

// startHungBash runs a command that forks a long sleep, and returns once that
// sleep is visible in the process table. A trailing "; echo" stops the shell
// exec-ing into the sleep, which is what real multi-step commands look like.
func startHungBash(t *testing.T) (*harness, *proc, string) {
	t.Helper()
	marker := sleepMarker()
	srv := newRouter(t, func(r *request) step {
		if r.N == 0 {
			return tool("bash", map[string]string{"command": "sleep " + marker + "; echo finished"})
		}
		return text("done")
	})
	h := newHarness(t, srv)
	h.hostSandbox()
	h.write(h.path("keep.txt"), "keep me", 0o644)
	p := h.start(defaultTimeout, "--model", "m", "--approve-all", "e2e task")
	deadline := time.Now().Add(10 * time.Second)
	for len(processesMatching(t, "sleep "+marker)) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("hung command never started\n%s", tail(p.res.stderr))
		}
		time.Sleep(20 * time.Millisecond)
	}
	return h, p, marker
}

func TestSignal_DuringHungBash_ExitsPromptly(t *testing.T) {
	t.Parallel()
	for name, sig := range signals {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h, p, _ := startHungBash(t)
			at := time.Now()
			p.signal(sig)
			r := p.wait()
			if took := time.Since(at); took > 3*time.Second {
				t.Fatalf("took %s to exit after %s", took, name)
			}
			if r.code == 0 {
				t.Fatalf("exit 0 after %s", name)
			}
			if got := readFile(t, h.path("keep.txt")); got != "keep me" {
				t.Fatalf("file changed: %q", got)
			}
		})
	}
}

func TestSignal_DuringHungBash_NoOrphan(t *testing.T) {
	defect(t, "SIGTERM/SIGINT to fuse leave the running bash command orphaned: no handler, no process group (cmd/fuse/main.go:217, internal/tools/sandbox/host.go:97)")
	t.Parallel()
	for name, sig := range signals {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, p, marker := startHungBash(t)
			p.signal(sig)
			p.wait()
			if hits := waitNoProcess(t, "sleep "+marker, 3*time.Second); len(hits) > 0 {
				t.Fatalf("orphan after %s: %v", name, hits)
			}
		})
	}
}

// write_file and edit_file truncate in place, so dying during the write loses the
// original. The test watches for the truncation and delivers SIGTERM inside that
// window, which makes a rare crash deterministic.
func TestSignal_DuringEdit_FileNotTruncated(t *testing.T) {
	defect(t, "edit_file/write_file write in place via os.WriteFile (O_TRUNC), so a signal or crash mid-write destroys the file (internal/tools/edit.go:59, internal/tools/write.go:50)")
	if testing.Short() {
		t.Skip("slow: writes a large file")
	}
	t.Parallel()
	const size = 128 << 20
	srv, h := scripted(t)
	path := h.path("big.txt")
	content := strings.Repeat("x", size/2) + "NEEDLE" + strings.Repeat("y", size/2)
	h.write(path, content, 0o644)
	srv.script(tool("edit_file", map[string]string{"path": path, "old_string": "NEEDLE", "new_string": "PINGLE"}), text("done"))

	p := h.start(defaultTimeout, "--model", "m", "--approve-all", "e2e task")
	caught := false
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if info, err := os.Stat(path); err == nil && info.Size() < int64(len(content)) {
			p.signal(syscall.SIGTERM)
			caught = true
			break
		}
		select {
		case <-p.waited:
			deadline = time.Now()
		default:
		}
	}
	p.wait()
	if !caught {
		t.Skip("the write finished before the truncation window was observed")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != int64(len(content)) {
		t.Fatalf("file is %d bytes after SIGTERM, was %d: original destroyed", info.Size(), len(content))
	}
}
