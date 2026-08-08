package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/permissions"
	"github.com/ethanhinson/fuse/internal/tools"
)

// progressEmittingTool invokes the progress callback (via ProgressFromContext)
// during Execute, then returns a normal result.
type progressEmittingTool struct{}

func (progressEmittingTool) Name() string        { return "prog" }
func (progressEmittingTool) Description() string  { return "emits progress" }
func (progressEmittingTool) Parameters() map[string]any {
	return map[string]any{"type": "object"}
}
func (progressEmittingTool) Execute(ctx context.Context, _ string) tools.Result {
	if report := ProgressFromContext(ctx); report != nil {
		total := 2.0
		report(1, &total)
		report(2, &total)
	}
	return tools.Result{Output: "done"}
}

func newProgressTestServer(t *testing.T, w *bytes.Buffer) *Server {
	t.Helper()
	reg := tools.NewRegistry()
	reg.Register(progressEmittingTool{})
	approve := func(context.Context, permissions.ApprovalRequest) (bool, bool, error) {
		return true, false, nil
	}
	gate := permissions.New(config.PermissionsConfig{Mode: "off"}, reg, approve)
	return NewServer(bytes.NewReader(nil), w, reg, gate)
}

func progressCallReq(t *testing.T, withToken bool) serverReq {
	t.Helper()
	args := json.RawMessage(`{}`)
	params := map[string]any{"name": "prog", "arguments": args}
	if withToken {
		params["_meta"] = map[string]any{"progressToken": "srv-tok"}
	}
	b, _ := json.Marshal(params)
	return serverReq{JSONRPC: "2.0", ID: json.RawMessage(`"1"`), Method: "tools/call", Params: b}
}

// TestServerEmitsProgressWhenTokenPresent: a tools/call carrying
// _meta.progressToken makes the server write id-less $/progress frames (echoing
// the token) before the response.
func TestServerEmitsProgressWhenTokenPresent(t *testing.T) {
	var buf bytes.Buffer
	s := newProgressTestServer(t, &buf)

	s.handleCallAndWrite(context.Background(), progressCallReq(t, true))

	frames := decodeFrames(t, buf.Bytes())
	var progress, responses int
	for _, f := range frames {
		if f["method"] == "$/progress" {
			progress++
			params, _ := f["params"].(map[string]any)
			meta, _ := params["progressToken"].(string)
			if meta != "srv-tok" {
				t.Errorf("$/progress token = %q, want srv-tok", meta)
			}
			if _, hasID := f["id"]; hasID {
				t.Errorf("$/progress frame must be id-less: %v", f)
			}
		}
		if _, hasID := f["id"]; hasID && f["method"] == nil {
			responses++
		}
	}
	if progress != 2 {
		t.Errorf("emitted %d $/progress frames, want 2", progress)
	}
	if responses != 1 {
		t.Errorf("emitted %d response frames, want 1", responses)
	}
}

// TestServerNoProgressWithoutToken: with no _meta.progressToken, the server
// writes no $/progress frame — only the response.
func TestServerNoProgressWithoutToken(t *testing.T) {
	var buf bytes.Buffer
	s := newProgressTestServer(t, &buf)

	s.handleCallAndWrite(context.Background(), progressCallReq(t, false))

	frames := decodeFrames(t, buf.Bytes())
	for _, f := range frames {
		if f["method"] == "$/progress" {
			t.Errorf("no token ⇒ no $/progress frame, got %v", f)
		}
	}
}

// TestServerAdvertisesStreamingCapability: the initialize result advertises a
// "streaming" capability so a client's Supports("streaming") gate can pass.
func TestServerAdvertisesStreamingCapability(t *testing.T) {
	s := newTestServer(tools.NewRegistry())
	resp := s.dispatch(context.Background(), initReq(t, "2025-03-26"))
	var r struct {
		Capabilities map[string]json.RawMessage `json:"capabilities"`
	}
	if err := json.Unmarshal(resp.Result, &r); err != nil {
		t.Fatalf("unmarshal init result: %v", err)
	}
	caps := ServerCapabilities{raw: r.Capabilities}
	if !caps.Supports("streaming") {
		t.Errorf("server must advertise streaming capability, got %v", r.Capabilities)
	}
	if !caps.Supports("tools") {
		t.Errorf("server must still advertise tools capability, got %v", r.Capabilities)
	}
}

// TestServerEncoderSerialization: concurrent progress writes and the response
// write are serialized (no interleaved/corrupt JSON). Each decoded frame is
// well-formed.
func TestServerEncoderSerialization(t *testing.T) {
	var buf bytes.Buffer
	s := newProgressTestServer(t, &buf)

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.handleCallAndWrite(context.Background(), progressCallReq(t, true))
		}()
	}
	wg.Wait()

	// If encoding interleaved, decodeFrames would fail on a corrupt boundary.
	frames := decodeFrames(t, buf.Bytes())
	if len(frames) != 4*3 { // each call: 2 progress + 1 response
		t.Errorf("decoded %d frames, want 12", len(frames))
	}
}

// decodeFrames splits a stream of newline-delimited JSON frames.
func decodeFrames(t *testing.T, b []byte) []map[string]any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(b))
	var out []map[string]any
	for {
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			if strings.Contains(err.Error(), "EOF") {
				break
			}
			t.Fatalf("decode frame: %v (buf: %s)", err, b)
		}
		out = append(out, m)
	}
	return out
}
