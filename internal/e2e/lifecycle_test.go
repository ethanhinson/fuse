package e2e

import (
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestMaxRunDuration_Config_DuringModelCall(t *testing.T) {
	t.Parallel()
	srv := newServer(t)
	srv.route = func(*request) step { return srv.hang() }
	h := newHarness(t, srv)
	h.extra = "max_run_duration: 1s\n"
	r := h.run()
	wantCode(t, r, 1)
	if r.signaled || r.timedOut {
		t.Fatalf("signaled=%v timedOut=%v, want the run to end itself", r.signaled, r.timedOut)
	}
	if r.dur > 6*time.Second {
		t.Fatalf("took %s to honor a 1s limit", r.dur)
	}
	wantContains(t, r.stderr, "max_run_duration", "stderr")
}

// The flag beats the config, in both directions.
func TestMaxRunDuration_Flag(t *testing.T) {
	t.Parallel()
	t.Run("tightens", func(t *testing.T) {
		t.Parallel()
		srv := newServer(t)
		srv.route = func(*request) step { return srv.hang() }
		h := newHarness(t, srv)
		h.extra = "max_run_duration: 1h\n"
		r := h.run("--max-run-duration", "1s")
		wantCode(t, r, 1)
		wantContains(t, r.stderr, "max_run_duration", "stderr")
	})
	t.Run("zero_disables", func(t *testing.T) {
		t.Parallel()
		srv := newServer(t, text("done"))
		h := newHarness(t, srv)
		h.extra = "max_run_duration: 1ns\n"
		wantCode(t, h.run("--max-run-duration", "0"), 0)
	})
	t.Run("negative_refused", func(t *testing.T) {
		t.Parallel()
		srv := newServer(t)
		h := newHarness(t, srv)
		wantCode(t, h.run("--max-run-duration", "-1s"), 2)
		if srv.count() != 0 {
			t.Fatalf("model called %d times", srv.count())
		}
	})
}

func TestMaxRunDuration_FinishedRunUnaffected(t *testing.T) {
	t.Parallel()
	srv := newServer(t, text("done"))
	h := newHarness(t, srv)
	h.extra = "max_run_duration: 1h\n"
	r := h.run()
	wantCode(t, r, 0)
	wantNotContains(t, r.stderr, "max_run_duration", "stderr")
}

func TestMaxRunDuration_KillsHungBash(t *testing.T) {
	t.Parallel()
	srv, h := scripted(t)
	h.hostSandbox()
	h.extra = "max_run_duration: 1s\n"
	marker := sleepMarker()
	srv.script(tool("bash", map[string]string{"command": "sleep " + marker + "; echo finished"}), text("should not get here"))
	r := h.run("--approve-all")
	wantCode(t, r, 1)
	wantContains(t, r.stderr, "max_run_duration", "stderr")
	if hits := waitNoProcess(t, "sleep "+marker, 3*time.Second); len(hits) > 0 {
		t.Fatalf("orphan after the limit: %v", hits)
	}
}

func TestMaxRunDuration_RepoConfigIgnored(t *testing.T) {
	t.Parallel()
	srv := newServer(t, text("done"))
	h := newHarness(t, srv)
	h.write(h.path(".fuse.local.yml"), "max_run_duration: 1ns\n", 0o644)
	wantCode(t, h.run(), 0)
}

func TestMaxRunDuration_BadValue_ConfigError(t *testing.T) {
	t.Parallel()
	srv := newServer(t)
	h := newHarness(t, srv)
	h.extra = "max_run_duration: soon\n"
	r := h.run()
	wantCode(t, r, 1)
	wantContains(t, r.stderr, "max_run_duration", "stderr")
	if srv.count() != 0 {
		t.Fatalf("model called %d times", srv.count())
	}
}

func TestSignal_StderrSaysWhy(t *testing.T) {
	t.Parallel()
	srv := newServer(t)
	srv.route = func(*request) step { return srv.hang() }
	h := newHarness(t, srv)
	p := h.start(defaultTimeout, "--model", "m", "e2e task")
	srv.waitForRequests(t, 1, 10*time.Second)
	p.signal(syscall.SIGTERM)
	wantContains(t, p.wait().stderr, "SIGTERM", "stderr")
}

// TestSignal_DuringEdit_FileNotTruncated can only fire on a truncation, so against
// the atomic write it skips. This one catches the replacement write in the act, by
// its temp file, and checks what is left is one whole version of the file.
func TestSignal_DuringEdit_TempWriteWindow_FileIntact(t *testing.T) {
	if testing.Short() {
		t.Skip("slow: writes a large file")
	}
	t.Parallel()
	const size = 128 << 20
	srv, h := scripted(t)
	path := h.path("big.txt")
	before := strings.Repeat("x", size/2) + "NEEDLE" + strings.Repeat("y", size/2)
	after := strings.Replace(before, "NEEDLE", "PINGLE", 1)
	h.write(path, before, 0o644)
	srv.script(tool("edit_file", map[string]string{"path": path, "old_string": "NEEDLE", "new_string": "PINGLE"}), text("done"))

	p := h.start(defaultTimeout, "--model", "m", "--approve-all", "e2e task")
	caught := false
	deadline := time.Now().Add(20 * time.Second)
	for !caught && time.Now().Before(deadline) {
		entries, err := os.ReadDir(h.cwd)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".fuse-tmp-") {
				p.signal(syscall.SIGTERM)
				caught = true
				break
			}
		}
		select {
		case <-p.waited:
			deadline = time.Now()
		default:
		}
	}
	p.wait()
	if !caught {
		t.Skip("the write finished before its temp file was observed")
	}
	got := readFile(t, path)
	if got != before && got != after {
		t.Fatalf("file is %d bytes and neither version after SIGTERM mid-write", len(got))
	}
}
