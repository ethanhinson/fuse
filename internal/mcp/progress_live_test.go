package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/permissions"
	"github.com/ethanhinson/fuse/internal/tools"
)

// TestLiveProgressStreamReceivePath is the load-bearing end-to-end check: it
// drives the REAL Manager against a hermetic re-exec MCP server double that
// emits id-less $/progress and $/stream notifications MID tools/call, and
// asserts fuse RECEIVES and ROUTES them — the observer sees ProgressEvents and
// the stream buffer concatenates into the tool result. This proves the pump
// route works against the real cmd/fuse seam, not just the faked-pump unit.
func TestLiveProgressStreamReceivePath(t *testing.T) {
	reg := tools.NewRegistry()
	mgr, err := NewManager([]config.MCPServerConfig{{
		Name:      "live",
		Transport: "stdio",
		Command:   []string{os.Args[0], "-test.run=TestHermeticProgressMCPServerHelper"},
		Env:       map[string]string{"FUSE_LIVE_PROGRESS_MCP": "1"},
	}}, reg)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer mgr.Close()

	if !reg.Has("mcp:live/streamer") {
		var got []string
		for _, s := range reg.Schemas() {
			got = append(got, s.Name)
		}
		t.Fatalf("expected mcp:live/streamer registered, got: %v", got)
	}

	// The server must have advertised "streaming" so the tool minted a token.
	for _, st := range mgr.Status() {
		if st.Name == "live" {
			found := false
			for _, c := range st.Capabilities {
				if c == "streaming" {
					found = true
				}
			}
			if !found {
				t.Fatalf("live server must advertise streaming, caps=%v", st.Capabilities)
			}
		}
	}

	// Subscribe an observer BEFORE the call.
	var (
		mu   sync.Mutex
		evts []ProgressEvent
	)
	mgr.OnProgress(func(e ProgressEvent) {
		mu.Lock()
		evts = append(evts, e)
		mu.Unlock()
	})

	gate := permissions.New(config.PermissionsConfig{Mode: "off"}, reg, permissions.AlwaysApprove)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res := gate.Execute(ctx, "mcp:live/streamer", `{}`)
	if res.IsError {
		t.Fatalf("streamer returned error: %s", res.Output)
	}

	// The stream chunks the server emitted must have been concatenated into the
	// complete result (the loop-contract result), NOT the envelope placeholder.
	if res.Output != "alpha-beta-gamma" {
		t.Errorf("result = %q, want concatenated stream %q", res.Output, "alpha-beta-gamma")
	}

	// The observer must have seen the progress notifications routed off the real
	// pump.
	mu.Lock()
	got := append([]ProgressEvent(nil), evts...)
	mu.Unlock()
	if len(got) < 2 {
		t.Fatalf("observer saw %d progress events, want >= 2 (progress not routed off the real pump)", len(got))
	}
	for _, e := range got {
		if e.Server != "live" || e.Tool != "streamer" {
			t.Errorf("progress event mis-correlated: %+v", e)
		}
	}
}

// TestHermeticProgressMCPServerHelper is not a real test: re-executed with
// FUSE_LIVE_PROGRESS_MCP=1 it becomes a minimal stdio MCP server that advertises
// streaming and, on tools/call, emits id-less $/progress + $/stream frames mid
// call before responding. It os.Exit(0)s before the test framework can print a
// summary, keeping stdout a clean JSON-RPC stream. Env unset ⇒ immediate return.
func TestHermeticProgressMCPServerHelper(t *testing.T) {
	if os.Getenv("FUSE_LIVE_PROGRESS_MCP") != "1" {
		return
	}
	runHermeticProgressServer(os.Stdin, os.Stdout)
	os.Exit(0)
}

func runHermeticProgressServer(rd, wr *os.File) {
	dec := json.NewDecoder(bufio.NewReader(rd))
	enc := json.NewEncoder(wr)

	send := func(v any) { _ = enc.Encode(v) }

	for {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
				Meta      struct {
					ProgressToken json.RawMessage `json:"progressToken"`
				} `json:"_meta"`
			} `json:"params"`
		}
		if err := dec.Decode(&req); err != nil {
			return
		}
		if len(req.ID) == 0 {
			continue // notification (e.g. initialized)
		}
		switch req.Method {
		case "initialize":
			send(map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{
					"protocolVersion": "2025-03-26",
					"capabilities": map[string]any{
						"tools":     map[string]any{},
						"streaming": map[string]any{},
					},
					"serverInfo": map[string]any{"name": "live", "version": "0.1.0"},
				},
			})
		case "tools/list":
			send(map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{"tools": []map[string]any{
					{"name": "streamer", "description": "streams then completes",
						"inputSchema": map[string]any{"type": "object"}},
				}},
			})
		case "tools/call":
			token := req.Params.Meta.ProgressToken
			// Emit id-less $/progress (determinate) — must be routed by fuse.
			for i, p := range []float64{1, 2} {
				_ = i
				send(map[string]any{
					"jsonrpc": "2.0", "method": "$/progress",
					"params": map[string]any{"progressToken": token, "progress": p, "total": 3.0},
				})
			}
			// Emit id-less $/stream deltas — must assemble into the result.
			for _, d := range []string{"alpha-", "beta-", "gamma"} {
				send(map[string]any{
					"jsonrpc": "2.0", "method": "$/stream",
					"params": map[string]any{"progressToken": token, "delta": d},
				})
			}
			// The response envelope carries a placeholder the stream buffer overrides.
			send(map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{
					"content": []map[string]any{{"type": "text", "text": "PLACEHOLDER"}},
					"isError": false,
				},
			})
		default:
			send(map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
				"error": map[string]any{"code": -32601, "message": fmt.Sprintf("method not found: %s", req.Method)},
			})
		}
	}
}
