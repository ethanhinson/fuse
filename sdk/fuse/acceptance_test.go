package fuse

import (
	"context"
	"io"
	"net"
	"net/http"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/event"
)

// TestRealLoopAcceptance stands up the REAL fuse loop runtime over the wire and drives
// the Go REMOTE SDK end-to-end against it — StartLoop -> Observe -> (no-loss/no-dup on
// reconnect) — with a SCRIPTED local gateway double (never Claude/Anthropic; project
// policy). It closes #55's fake-backend gap: everything below the SDK is the production
// engine.
//
// Because cmd/fuse is package main (un-importable), the real runtime is stood up via a
// `go test`-managed subprocess: the test `go build`s ./cmd/fuse once, then execs the
// binary as `fuse loop-serve-net --addr 127.0.0.1:<port>` with LLM_GATEWAY_URL pointed
// at an in-test scripted gateway and HOME set to a temp dir so it uses the built-in dev
// token (fuse-dev-token → tenant _default). This is the plan's documented Task-6 fallback
// (Risks/notes) for the un-importable composition root. (A built binary, not `go run`,
// so CommandContext's kill reaches the real server rather than orphaning it.)
func TestRealLoopAcceptance(t *testing.T) {
	if testing.Short() {
		t.Skip("real-loop acceptance builds+runs cmd/fuse; skipped in -short")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH; real-loop acceptance needs the go toolchain (skipping)")
	}

	// Scripted gateway double: a fixed non-streamed assistant reply so each loop produces
	// a deterministic, model-free turn. NEVER a real provider.
	gwURL := newScriptedGateway(t)

	repoRoot := repoRootFromTest(t)

	// Build the fuse binary ONCE to a temp path, then exec it DIRECTLY (not via `go run`).
	// `go run` spawns the compiled binary as a child and does NOT forward SIGKILL to it, so
	// CommandContext's cancel would orphan the real server and cmd.Wait() would block on its
	// still-open stdout pipe forever. Running the built binary directly means cancel/Kill
	// reaches the actual server process, so teardown is clean.
	bin := buildFuseBinary(t, repoRoot)

	// Pick a free ephemeral port, then hand it to the subprocess as a fixed --addr (the
	// subcommand does not print its bound address, so we cannot use :0 and learn it).
	port := freePort(t)
	addr := net.JoinHostPort("127.0.0.1", port)
	base := "http://" + addr

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "loop-serve-net", "--addr", addr)
	cmd.Dir = repoRoot
	cmd.Env = append(cmd.Environ(),
		"HOME="+t.TempDir(),        // no user config → built-in dev token (tenant _default)
		"LLM_GATEWAY_URL="+gwURL,   // scripted double
		"LLM_GATEWAY_KEY=test-key", // any non-empty value
	)
	// Capture output for a failure dump.
	var outBuf syncBuffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &outBuf
	if err := cmd.Start(); err != nil {
		t.Fatalf("start loop-serve-net: %v", err)
	}
	t.Cleanup(func() {
		cancel()               // CommandContext SIGKILLs the real server (direct child).
		_ = cmd.Process.Kill() // belt-and-suspenders: ensure it is gone.
		waitBounded(cmd, 10*time.Second)
	})

	// Wait for the server to accept connections (a direct exec launches near-instantly, but
	// give margin for the runtime to bind).
	waitForListen(t, addr, 30*time.Second, &outBuf)

	// Dev token → tenant _default (built-in when no loop_server.auth is configured).
	c := NewRemote(base, Credentials{Token: "fuse-dev-token", Tenant: string(event.DefaultTenant)})
	rctx := context.Background()

	id, err := c.StartLoop(rctx, StartLoopConfig{Task: "acceptance please"})
	if err != nil {
		t.Fatalf("StartLoop against real engine: %v\n--- server output ---\n%s", err, outBuf.String())
	}
	if id == "" {
		t.Fatal("real StartLoop returned empty loop id")
	}

	// Observe from 0 and drain until the one-shot loop reaches turn.end (the terminal
	// event of a non-interactive run). Record the seqs seen.
	lastSeq, seen1 := observeUntilTurnEnd(t, c, id)
	if len(seen1) == 0 {
		t.Fatal("first Observe saw no events from the real engine")
	}
	assertNoLossNoDup(t, seen1, 0, lastSeq, "real first observe")

	// Reconnect: re-open Observe(0). The full durable history replays; assert exactly-once
	// again (no loss, no dup) across the real subscribe->replay handoff from the client.
	seen2 := observeCollect(t, c, id, 0, lastSeq)
	assertNoLossNoDup(t, seen2, 0, lastSeq, "real reconnect observe")
	if len(seen2) < len(seen1) {
		t.Fatalf("reconnect replayed %d events, want >= first observe's %d (loss on reconnect)", len(seen2), len(seen1))
	}
}

// --- helpers -------------------------------------------------------------------

// observeUntilTurnEnd opens Observe(0) and drains until it sees a turn.end, returning
// the last seq and the ordered seqs seen up to and including it. It retries the whole
// Observe if the loop's turn.end has not yet persisted (the durable-flush race).
func observeUntilTurnEnd(t *testing.T, c *Client, id string) (Seq, []Seq) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		ch, unsub, err := c.Observe(ctx, id, 0)
		if err != nil {
			cancel()
			time.Sleep(50 * time.Millisecond)
			continue
		}
		var seqs []Seq
		var last Seq
		sawEnd := false
	drain:
		for {
			select {
			case e, ok := <-ch:
				if !ok {
					break drain
				}
				seqs = append(seqs, e.Seq)
				last = e.Seq
				if e.Kind == event.KindTurnEnd {
					sawEnd = true
					break drain
				}
			case <-ctx.Done():
				break drain
			}
		}
		unsub()
		cancel()
		if sawEnd {
			return last, seqs
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("timeout waiting for real loop turn.end")
	return 0, nil
}

// observeCollect opens Observe(from) and collects ordered seqs until it sees `through`.
func observeCollect(t *testing.T, c *Client, id string, from, through Seq) []Seq {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ch, unsub, err := c.Observe(ctx, id, from)
	if err != nil {
		t.Fatalf("Observe(collect): %v", err)
	}
	defer unsub()
	var seqs []Seq
	for {
		select {
		case e, ok := <-ch:
			if !ok {
				t.Fatalf("stream ended before seq %d; saw %v", through, seqs)
			}
			seqs = append(seqs, e.Seq)
			if e.Seq >= through {
				return seqs
			}
		case <-ctx.Done():
			t.Fatalf("timeout before seq %d; saw %v", through, seqs)
		}
	}
}

// buildFuseBinary compiles ./cmd/fuse to a temp path once and returns it, so the server
// can be exec'd directly (a killable child). A build failure fails the test loudly.
func buildFuseBinary(t *testing.T, repoRoot string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "fuse-acceptance")
	build := exec.Command("go", "build", "-o", bin, "./cmd/fuse")
	build.Dir = repoRoot
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build ./cmd/fuse: %v\n%s", err, out)
	}
	return bin
}

// waitBounded waits for cmd to exit but never blocks longer than d (so a wedged child
// cannot hang teardown). It is best-effort: after Kill the process should exit promptly.
func waitBounded(cmd *exec.Cmd, d time.Duration) {
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
	}
}

// newScriptedGateway starts an httptest-style gateway that returns a fixed non-streamed
// assistant reply, and returns its URL. Torn down via t.Cleanup. Mirrors cmd/fuse's
// newScriptedGateway (un-importable there). NEVER a real provider (project policy).
func newScriptedGateway(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("gateway listen: %v", err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"REPLY-ACCEPTANCE"}}]}`)
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return "http://" + ln.Addr().String()
}

// freePort binds an ephemeral port, closes it, and returns the number so the subprocess
// can bind it. There is a small reuse window; acceptable for a loopback test.
func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("freePort split: %v", err)
	}
	_ = ln.Close()
	return port
}

// waitForListen dials addr until it connects or the deadline passes.
func waitForListen(t *testing.T, addr string, timeout time.Duration, out *syncBuffer) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("loop-serve-net never accepted on %s within %s\n--- server output ---\n%s", addr, timeout, out.String())
}

// repoRootFromTest resolves the repo root from this test file's path (…/sdk/fuse).
func repoRootFromTest(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate test file")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
}

// syncBuffer is a tiny thread-safe buffer so the subprocess's concurrent stdout/stderr
// writes and the test goroutine's reads do not race.
type syncBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	return len(p), nil
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}
