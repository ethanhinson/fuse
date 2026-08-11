package loopconnect

import (
	"context"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/loopwire/v1/loopv1connect"
)

// TestConnectESSmoke drives the REAL generated connect-es TS stub against a live
// connect-go server (change 0055, Task 6): it starts an httptest server over the
// handler wired to a scripted fake runtime (history seqs 1..3 + a live tail), then
// execs the node connect-es client (proto/smoke/client.ts via `npx tsx`) which does
// unary StartLoop, unary Send, a server-streaming Observe tracking seq, and a
// reconnect that re-opens Observe(from_seq). Success is the client printing SMOKE_OK.
//
// It SKIPS when the node toolchain or proto/node_modules is absent (a plain
// `go test ./...` without `cd proto && npm ci` still passes). The browser/Playwright
// run is a documented manual step in the results file; the wire exercised here is
// identical.
func TestConnectESSmoke(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not on PATH; connect-es TS smoke needs node (skipping)")
	}
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate test file")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
	protoDir := filepath.Join(repoRoot, "proto")
	clientTS := filepath.Join(protoDir, "smoke", "client.ts")
	// Require the installed toolchain (tsx + connect-node) under proto/node_modules.
	if !dirExists(filepath.Join(protoDir, "node_modules", "tsx")) {
		t.Skip("proto/node_modules missing (run `cd proto && npm ci`); skipping TS smoke")
	}
	if !dirExists(filepath.Join(protoDir, "node_modules", "@connectrpc", "connect-node")) {
		t.Skip("proto/node_modules missing @connectrpc/connect-node; skipping TS smoke")
	}

	// Scripted fake runtime: Observe hands back a live channel pre-filled with events
	// 1..3 (so Observe from 0 replays them via Attach) and Attach returns the same
	// history filtered by from_seq. The live channel also carries the events so a
	// reconnect from_seq=lastSeq-1 resumes.
	hist := []event.Event{
		{Seq: 1, Kind: event.KindTurnStart},
		{Seq: 2, Kind: event.KindModelCallStart},
		{Seq: 3, Kind: event.KindModelCallEnd},
	}
	f := &fakeRuntime{startID: "loop-smoke"}
	f.attachFn = func(ctx context.Context, tn event.TenantID, loopID string, from event.Seq) ([]event.Event, error) {
		var out []event.Event
		for _, e := range hist {
			if e.Seq > from {
				out = append(out, e)
			}
		}
		return out, nil
	}
	f.observeFn = func(ctx context.Context, tn event.TenantID, loopID string) (<-chan event.Event, func(), error) {
		// A fresh buffered channel per Observe, so the reconnect stream is independent.
		ch := make(chan event.Event, 8)
		return ch, func() {}, nil
	}

	_, h := loopv1connect.NewLoopServiceHandler(NewHandler(f).WithKeepalive(time.Hour))
	srv := httptest.NewServer(h)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// Use `node --import tsx` (not `npx tsx`): npx's resolution overhead/buffering can
	// stall the subprocess in CI; the direct node loader is deterministic. tsx is
	// resolved from proto/node_modules via cmd.Dir.
	cmd := exec.CommandContext(ctx, "node", "--import", "tsx", clientTS, srv.URL)
	cmd.Dir = protoDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("connect-es smoke client failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "SMOKE_OK") {
		t.Fatalf("smoke client did not report SMOKE_OK:\n%s", out)
	}
	t.Logf("connect-es smoke output: %s", strings.TrimSpace(string(out)))
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}
