package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// runOneTool scripts one tool call then a final answer, and returns what the
// model was told.
func runOneTool(t *testing.T, srv *server, h *harness, name string, args any, flags ...string) string {
	t.Helper()
	srv.script(tool(name, args), text("done"))
	wantCode(t, h.run(flags...), 0)
	return h.toolOutput()
}

const memLimit = 200 << 20

func TestRead_BinaryFile_Refused(t *testing.T) {
	t.Parallel()
	srv, h := scripted(t)
	h.write(h.path("blob.bin"), "abc\x00def\x01\x02", 0o644)
	out := runOneTool(t, srv, h, "read_file", map[string]string{"path": h.path("blob.bin")})
	wantContains(t, out, "binary", "tool result")
}

func TestRead_MissingFile_Error(t *testing.T) {
	t.Parallel()
	srv, h := scripted(t)
	out := runOneTool(t, srv, h, "read_file", map[string]string{"path": h.path("nope.txt")})
	wantContains(t, out, "no such file", "tool result")
}

func TestRead_Directory_Error(t *testing.T) {
	t.Parallel()
	srv, h := scripted(t)
	out := runOneTool(t, srv, h, "read_file", map[string]string{"path": h.cwd})
	wantContains(t, out, "is a directory", "tool result")
}

func TestRead_LongFile_WindowedWithFooter(t *testing.T) {
	t.Parallel()
	srv, h := scripted(t)
	var b strings.Builder
	for i := 1; i <= 5000; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	h.write(h.path("long.txt"), b.String(), 0o644)
	out := runOneTool(t, srv, h, "read_file", map[string]string{"path": h.path("long.txt")})
	wantContains(t, out, "Showing lines 1-1000", "tool result")
	wantNotContains(t, out, "line 1001\n", "tool result")
}

func TestRead_HugeFile_MemoryBounded(t *testing.T) {
	defect(t, "read_file loads the whole file into memory even to return a window; no size guard (internal/archive/archive.go:92)")
	if testing.Short() {
		t.Skip("slow")
	}
	t.Parallel()
	srv, h := scripted(t)
	path := h.path("huge.dat")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(1 << 30); err != nil { // sparse: costs no disk
		t.Fatal(err)
	}
	f.Close()
	srv.script(tool("read_file", map[string]any{"path": path, "start_line": 1, "end_line": 2}), text("done"))
	r := h.run()
	wantCode(t, r, 0)
	if r.maxRSS > memLimit {
		t.Fatalf("peak RSS %dMB reading two lines of a 1GB file, limit %dMB", r.maxRSS>>20, memLimit>>20)
	}
}

// A FIFO blocks os.ReadFile forever. The tool timeout has to bound it.
func TestRead_FIFO_BoundedByToolTimeout(t *testing.T) {
	t.Parallel()
	srv, h := scripted(t)
	h.extra = "agents:\n  tool_timeout_seconds: 1\n"
	path := h.path("pipe")
	if err := syscall.Mkfifo(path, 0o644); err != nil {
		t.Fatal(err)
	}
	out := runOneTool(t, srv, h, "read_file", map[string]string{"path": path})
	wantContains(t, out, "per-call timeout", "tool result")
}

func TestWrite_OutsideWorkspace_RefusedWithoutApproveAll(t *testing.T) {
	t.Parallel()
	srv, h := scripted(t)
	target := filepath.Join(h.outside(), "new.txt")
	out := runOneTool(t, srv, h, "write_file", map[string]string{"path": target, "content": "x"})
	wantContains(t, out, "requires approval", "tool result")
	mustNotExist(t, target)
}

// --approve-all is documented as mode: off, "a deliberate footgun" (main.go:105).
func TestWrite_OutsideWorkspace_AllowedWithApproveAll_Documented(t *testing.T) {
	t.Parallel()
	srv, h := scripted(t)
	target := filepath.Join(h.outside(), "new.txt")
	runOneTool(t, srv, h, "write_file", map[string]string{"path": target, "content": "x"}, "--approve-all")
	if got := readFile(t, target); got != "x" {
		t.Fatalf("got %q", got)
	}
}

// Each escape shape should need approval, just as a write outside the workspace
// does. The safe-list comment deliberately treats read tools as inert, but says
// nothing about scope; see the report.
func TestRead_OutsideWorkspace_RequiresApproval(t *testing.T) {
	defect(t, "read_file/grep/list_directory are safe-listed with no path scope: any path, symlink, or ~/.fuse/config.yml is read with no approval on a headless run (internal/permissions/policy.go:77, gate.go:613)")
	t.Parallel()
	const secret = "OUTSIDE-SECRET-CONTENT"
	cases := map[string]func(h *harness) (string, any){
		"absolute": func(h *harness) (string, any) {
			p := filepath.Join(h.outside(), "s.txt")
			h.write(p, secret, 0o600)
			return "read_file", map[string]string{"path": p}
		},
		"dotdot": func(h *harness) (string, any) {
			h.write(filepath.Join(h.cwd, "..", "s.txt"), secret, 0o600)
			return "read_file", map[string]string{"path": "../s.txt"}
		},
		"symlink": func(h *harness) (string, any) {
			p := filepath.Join(h.outside(), "s.txt")
			h.write(p, secret, 0o600)
			if err := os.Symlink(p, h.path("notes.txt")); err != nil {
				h.t.Fatal(err)
			}
			return "read_file", map[string]string{"path": "notes.txt"}
		},
		"grep_outside": func(h *harness) (string, any) {
			o := h.outside()
			h.write(filepath.Join(o, "s.txt"), secret, 0o600)
			return "grep", map[string]string{"pattern": secret, "path": o}
		},
		"list_outside": func(h *harness) (string, any) {
			o := h.outside()
			h.write(filepath.Join(o, secret), "", 0o600)
			return "list_directory", map[string]string{"path": o}
		},
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv, h := scripted(t)
			tn, args := mk(h)
			out := runOneTool(t, srv, h, tn, args)
			wantNotContains(t, out, secret, "tool result")
		})
	}
}

// The escape that matters most: the gateway key, on a run with no approver.
func TestRead_GatewayKeyFromConfig_RequiresApproval(t *testing.T) {
	defect(t, "read_file of ~/.fuse/config.yml is auto-approved, so a prompt-injected model reads the gateway key into its context and the trace (internal/permissions/policy.go:77)")
	t.Parallel()
	srv, h := scripted(t)
	trace := filepath.Join(t.TempDir(), "trace")
	srv.script(tool("read_file", map[string]string{"path": filepath.Join(h.home, ".fuse", "config.yml")}), text("done"))
	wantCode(t, h.run("--trace", trace), 0)
	wantNotContains(t, h.toolOutput(), h.key, "tool result")
	wantNotContains(t, readFile(t, trace), h.key, "trace")
}

// Without --approve-all, mutating through any escape shape stays refused.
func TestWriteEdit_EscapesRefusedWithoutApproveAll(t *testing.T) {
	t.Parallel()
	const original = "untouched"
	for _, tn := range []string{"write_file", "edit_file"} {
		for _, shape := range []string{"symlink", "dotdot"} {
			t.Run(tn+"_"+shape, func(t *testing.T) {
				t.Parallel()
				srv, h := scripted(t)
				var target, arg string
				if shape == "symlink" {
					target = filepath.Join(h.outside(), "t.txt")
					h.write(target, original, 0o644)
					if err := os.Symlink(target, h.path("link.txt")); err != nil {
						t.Fatal(err)
					}
					arg = "link.txt"
				} else {
					target = filepath.Join(h.cwd, "..", "t.txt")
					h.write(target, original, 0o644)
					arg = "../t.txt"
				}
				args := map[string]string{"path": arg, "content": "pwned"}
				if tn == "edit_file" {
					args = map[string]string{"path": arg, "old_string": original, "new_string": "pwned"}
				}
				out := runOneTool(t, srv, h, tn, args)
				wantContains(t, out, "requires approval", "tool result")
				if got := readFile(t, target); got != original {
					t.Fatalf("target = %q", got)
				}
			})
		}
	}
}

// Auto mode scopes mutations to the workspace with no human or classifier, so its
// scoping must see through every symlink shape.
func TestAutoMode_WriteEscapes_NotAutoApproved(t *testing.T) {
	t.Parallel()
	cases := map[string]func(h *harness, outside string) (arg, target string){
		"symlink_to_file": func(h *harness, o string) (string, string) {
			target := filepath.Join(o, "t.txt")
			h.write(target, "original", 0o644)
			if err := os.Symlink(target, h.path("link.txt")); err != nil {
				h.t.Fatal(err)
			}
			return "link.txt", target
		},
		"symlink_dir_new_file": func(h *harness, o string) (string, string) {
			if err := os.Symlink(o, h.path("linkdir")); err != nil {
				h.t.Fatal(err)
			}
			return "linkdir/new.txt", filepath.Join(o, "new.txt")
		},
		"dotdot": func(h *harness, o string) (string, string) {
			return "../t.txt", filepath.Join(h.cwd, "..", "t.txt")
		},
		"absolute": func(h *harness, o string) (string, string) {
			return filepath.Join(o, "new.txt"), filepath.Join(o, "new.txt")
		},
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv, h := scripted(t)
			h.extra = "permissions:\n  mode: auto\n"
			arg, target := mk(h, h.outside())
			out := runOneTool(t, srv, h, "write_file", map[string]string{"path": arg, "content": "pwned"})
			wantNotContains(t, out, "wrote ", "tool result")
			if b, err := os.ReadFile(target); err == nil && string(b) == "pwned" {
				t.Fatalf("auto mode wrote outside the workspace via %s", name)
			}
		})
	}
}

func TestAutoMode_DanglingSymlink_NotAutoApproved(t *testing.T) {
	defect(t, "auto-mode scope misjudges a dangling symlink as in-workspace: EvalSymlinks fails, the leaf is re-appended, and write_file follows the link out (internal/permissions/heuristics.go:764-781)")
	t.Parallel()
	srv, h := scripted(t)
	h.extra = "permissions:\n  mode: auto\n"
	target := filepath.Join(h.outside(), "created.txt")
	if err := os.Symlink(target, h.path("link.txt")); err != nil {
		t.Fatal(err)
	}
	runOneTool(t, srv, h, "write_file", map[string]string{"path": "link.txt", "content": "pwned"})
	mustNotExist(t, target)
}

// --approve-all follows symlinks out: it is mode off, the documented footgun.
func TestApproveAll_SymlinkEscapeWrites_Documented(t *testing.T) {
	t.Parallel()
	srv, h := scripted(t)
	target := filepath.Join(h.outside(), "t.txt")
	h.write(target, "original", 0o644)
	if err := os.Symlink(target, h.path("link.txt")); err != nil {
		t.Fatal(err)
	}
	runOneTool(t, srv, h, "edit_file", map[string]string{"path": "link.txt", "old_string": "original", "new_string": "changed"}, "--approve-all")
	if got := readFile(t, target); got != "changed" {
		t.Fatalf("got %q", got)
	}
}

func TestEdit_EmptyOldString_RefusedFileUntouched(t *testing.T) {
	t.Parallel()
	srv, h := scripted(t)
	h.write(h.path("a.txt"), "content", 0o644)
	out := runOneTool(t, srv, h, "edit_file", map[string]string{"path": h.path("a.txt"), "old_string": "", "new_string": "X"}, "--approve-all")
	wantContains(t, out, "not unique", "tool result")
	if got := readFile(t, h.path("a.txt")); got != "content" {
		t.Fatalf("got %q", got)
	}
}

// old_string == new_string writes the same bytes and reports "edited": harmless
// but untrue. See the report.
func TestEdit_OldEqualsNew_BytesUnchanged(t *testing.T) {
	t.Parallel()
	srv, h := scripted(t)
	h.write(h.path("a.txt"), "same content", 0o644)
	runOneTool(t, srv, h, "edit_file", map[string]string{"path": h.path("a.txt"), "old_string": "same", "new_string": "same"}, "--approve-all")
	if got := readFile(t, h.path("a.txt")); got != "same content" {
		t.Fatalf("got %q", got)
	}
}

func TestEdit_NonUTF8File_BytesPreserved(t *testing.T) {
	t.Parallel()
	srv, h := scripted(t)
	path := h.path("latin1.txt")
	original := "caf\xe9 \xff\xfe TOKEN \x80 end\n"
	h.write(path, original, 0o644)
	srv.script(
		tool("read_file", map[string]string{"path": path}),
		tools(call{"call_2", "edit_file", mustArgs(map[string]string{"path": path, "old_string": "TOKEN", "new_string": "REPLACED"})}),
		text("done"),
	)
	wantCode(t, h.run("--approve-all"), 0)
	if got, want := readFile(t, path), strings.Replace(original, "TOKEN", "REPLACED", 1); got != want {
		t.Fatalf("bytes corrupted:\n got %q\nwant %q", got, want)
	}
}

func TestEdit_CRLF_Preserved(t *testing.T) {
	t.Parallel()
	srv, h := scripted(t)
	path := h.path("dos.txt")
	h.write(path, "one\r\ntwo\r\nthree\r\n", 0o644)
	runOneTool(t, srv, h, "edit_file", map[string]string{"path": path, "old_string": "two", "new_string": "TWO"}, "--approve-all")
	if got := readFile(t, path); got != "one\r\nTWO\r\nthree\r\n" {
		t.Fatalf("got %q", got)
	}
}

func TestEditWrite_FileModePreserved(t *testing.T) {
	t.Parallel()
	for _, mode := range []os.FileMode{0o755, 0o700, 0o600, 0o640} {
		for _, tn := range []string{"edit_file", "write_file"} {
			t.Run(fmt.Sprintf("%s_%o", tn, mode), func(t *testing.T) {
				t.Parallel()
				srv, h := scripted(t)
				path := h.path("script.sh")
				h.write(path, "#!/bin/sh\necho old\n", mode)
				args := map[string]string{"path": path, "content": "#!/bin/sh\necho new\n"}
				if tn == "edit_file" {
					args = map[string]string{"path": path, "old_string": "old", "new_string": "new"}
				}
				runOneTool(t, srv, h, tn, args, "--approve-all")
				info, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				if info.Mode().Perm() != mode {
					t.Fatalf("mode = %o, want %o", info.Mode().Perm(), mode)
				}
				wantContains(t, readFile(t, path), "new", "file")
			})
		}
	}
}

// --- bash, on the host substrate, inside temp dirs ------------------------

var artRef = regexp.MustCompile(`art://[^\s,;\]]+`)

func TestBash_HugeOutput_SpillsAndIsRecoverable(t *testing.T) {
	t.Parallel()
	srv := newRouter(t, func(r *request) step {
		switch r.N {
		case 0:
			return tool("bash", map[string]string{"command": "seq 1 300000"})
		case 1:
			out, _ := r.toolResult("call_1")
			if len(out) > 40<<10 {
				return status(500, "inline result not bounded")
			}
			return tools(call{"call_2", "read_file", mustArgs(map[string]any{"path": artRef.FindString(out), "start_line": 150000, "end_line": 150001})})
		}
		return text("done")
	})
	h := newHarness(t, srv)
	h.hostSandbox()
	wantCode(t, h.run("--approve-all"), 0)
	wantContains(t, h.lastRequest().toolResults()[0], "output truncated", "tool result")
	out, ok := h.lastRequest().toolResult("call_2")
	if !ok || out != "150000\n150001" {
		t.Fatalf("spill read back %q", out)
	}
}

func TestBash_HugeOutput_MemoryBounded(t *testing.T) {
	defect(t, "bash buffers all output in memory via CombinedOutput with no cap; a noisy command can OOM the harness (internal/tools/sandbox/host.go:157)")
	if testing.Short() {
		t.Skip("slow")
	}
	t.Parallel()
	srv, h := scripted(t)
	h.hostSandbox()
	srv.script(tool("bash", map[string]string{"command": `head -c 300000000 /dev/zero | tr '\0' x`}), text("done"))
	r := h.run("--approve-all")
	wantCode(t, r, 0)
	if r.maxRSS > memLimit {
		t.Fatalf("peak RSS %dMB for 300MB of output, limit %dMB", r.maxRSS>>20, memLimit>>20)
	}
}

// A bare command: the shell execs into it, so killing the shell kills it.
func TestBash_Forever_TimeoutSeconds_Simple(t *testing.T) {
	if runtime.GOOS == "linux" {
		defect(t, "on Linux /bin/sh forks instead of execing, so the bash timeout kills only sh while its child holds the pipe")
	}
	t.Parallel()
	srv, h := scripted(t)
	h.hostSandbox()
	marker := sleepMarker()
	srv.script(tool("bash", map[string]any{"command": "sleep " + marker, "timeout_seconds": 1}), text("done"))
	r := h.run("--approve-all")
	wantCode(t, r, 0)
	wantContains(t, h.toolOutput(), "timed out after 1s", "tool result")
	if r.dur > 10*time.Second {
		t.Fatalf("took %s", r.dur)
	}
	if hits := waitNoProcess(t, "sleep "+marker, 2*time.Second); len(hits) > 0 {
		t.Fatalf("still running: %v", hits)
	}
}

// The per-call tool timeout bounds any bash command, whatever it does.
func TestBash_Forever_ToolTimeoutBoundsRun(t *testing.T) {
	t.Parallel()
	srv, h := scripted(t)
	h.hostSandbox()
	h.extra = "agents:\n  tool_timeout_seconds: 1\n"
	srv.script(tool("bash", map[string]string{"command": "sleep " + sleepMarker() + "; echo x"}), text("done"))
	r := h.run("--approve-all")
	wantCode(t, r, 0)
	wantContains(t, h.toolOutput(), "per-call timeout", "tool result")
}

// A compound command's shell is killed but its child keeps the output pipe open,
// so CombinedOutput blocks: the requested timeout isn't honored and the child lives.
func TestBash_TimeoutSeconds_CompoundCommand_KillsChildrenPromptly(t *testing.T) {
	defect(t, "bash timeout kills only /bin/sh: no process group, no WaitDelay, so a compound command blocks past timeout_seconds and its child is orphaned (internal/tools/sandbox/host.go:97,157)")
	t.Parallel()
	srv, h := scripted(t)
	h.hostSandbox()
	// The 4s cap only stops an unfixed binary blocking for the full 120s default.
	h.extra = "agents:\n  tool_timeout_seconds: 4\n"
	marker := sleepMarker()
	srv.script(tool("bash", map[string]any{"command": "sleep " + marker + "; echo x", "timeout_seconds": 1}), text("done"))
	wantCode(t, h.run("--approve-all"), 0)
	out := h.toolOutput()
	if hits := waitNoProcess(t, "sleep "+marker, 2*time.Second); len(hits) > 0 {
		t.Errorf("child still running after timeout and exit: %v", hits)
	}
	wantContains(t, out, "timed out after 1s", "tool result")
}

func TestBash_InvalidUTF8_RequestStaysValid(t *testing.T) {
	t.Parallel()
	srv, h := scripted(t)
	h.hostSandbox()
	out := runOneTool(t, srv, h, "bash", map[string]string{"command": `printf 'before \377\376 after\n'`}, "--approve-all")
	wantContains(t, out, "before", "tool result")
	wantContains(t, out, "after", "tool result")
}

func TestBash_ExitStatusReported(t *testing.T) {
	t.Parallel()
	srv, h := scripted(t)
	h.hostSandbox()
	out := runOneTool(t, srv, h, "bash", map[string]string{"command": "echo oops; exit 7"}, "--approve-all")
	wantContains(t, out, "oops", "tool result")
	wantContains(t, out, "exit status 7", "tool result")
}
