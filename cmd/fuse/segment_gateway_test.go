package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/artifact"
	artfs "github.com/ethanhinson/fuse/internal/artifact/fsstore"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/segment"
	"github.com/ethanhinson/fuse/internal/segment/artsink"
	"github.com/ethanhinson/fuse/internal/tools"
)

// TestSegmentGatewaySeamArchivesAndRecovers drives a real agent turn through the
// production segment wiring (real model.Adapters POSTing to a scripted httptest
// gateway, the store-backed artsink threaded per-loop into EnableSummarization
// exactly as installSummarizer does with its segSink value, and the registry
// bound to the same loop as the shell binds it) and asserts the loop wiring
// end-to-end at the gateway seam (learning verify-tool-loop-at-gateway-seam):
//
//	(a) one index entry, and the segment body under
//	    <base>/<tenant>/<loop>/segment/ is gzip at rest
//	(b) the post-compaction MAIN request carries the summary WITH the
//	    "Recovery: grep your past at art://" line
//	(c) a subsequent segment_read call round-trips the archived raw content
//	(d) grep on the recovery pointer ref finds the raw content.
func TestSegmentGatewaySeamArchivesAndRecovers(t *testing.T) {
	const cannedSummary = "Objective: verify seam\nState: compacted\nNext: proceed\nFiles: none"

	var (
		mu          sync.Mutex
		mainReqBody string
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		isSummarizer := strings.Contains(string(b), `"tool_choice":"none"`) && strings.Contains(string(b), "Objective:")
		w.Header().Set("Content-Type", "application/json")
		if isSummarizer {
			resp := map[string]any{
				"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": cannedSummary}}},
				"usage":   map[string]int{"prompt_tokens": 10, "completion_tokens": 5},
			}
			_ = json.NewEncoder(w).Encode(resp)
			return
		}
		mu.Lock()
		mainReqBody = string(b)
		mu.Unlock()
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"done"}}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`)
	}))
	defer srv.Close()

	// One loop's store binding keyed by a fixed root id, wired the shell's way:
	// the registry is bound first and the sink is built from its binding.
	base := t.TempDir()
	const rootID = "root-seam"
	reg := tools.NewRegistry()
	for _, tl := range tools.DefaultTools(nil) {
		reg.Register(tl)
	}
	reg.WithArtifacts(artfs.New(base), event.DefaultTenant, rootID)
	sink := artsink.New(reg.Artifacts())
	if sink == nil {
		t.Fatal("artsink.New returned nil for the bound registry")
	}

	mainAdapter := model.NewAdapter(srv.URL, "tkn", srv.Client())
	a := agent.New(mainAdapter, reg, gwNopRenderer{}, "cloud/model", "", 2, 128)
	a.ContextWindow = 4000
	// Wire Tier 2 exactly as installSummarizer now does, threading the real sink as a
	// per-loop value (change 0046 — not a process-global).
	sumAdapter := model.NewAdapter(srv.URL, "tkn", srv.Client())
	a.EnableSummarization(sumAdapter, "cloud/model", 2000, sink)

	big := strings.Repeat("RAWDATA-", 1000) // ~8000 bytes each, unique marker
	history := []model.Message{
		{Role: "user", Content: "task"},
		{Role: "assistant", ToolCalls: []model.ToolCall{{ID: "1", Name: "read_file"}}},
		{Role: "tool", ToolCallID: "1", Name: "read_file", Content: big},
		{Role: "assistant", ToolCalls: []model.ToolCall{{ID: "2", Name: "read_file"}}},
		{Role: "tool", ToolCallID: "2", Name: "read_file", Content: big},
		{Role: "assistant", ToolCalls: []model.ToolCall{{ID: "3", Name: "grep"}}},
		{Role: "tool", ToolCallID: "3", Name: "grep", Content: big},
	}

	if _, err := a.Run(context.Background(), history); err != nil {
		t.Fatalf("agent run: %v", err)
	}

	// (a) one index entry; its segment body is gzip at rest on disk.
	idx, err := segment.NewArtifactSource(reg.Artifacts()).ReadIndex(context.Background())
	if err != nil || len(idx.Segments) != 1 {
		t.Fatalf("index should have exactly 1 entry, got %+v (err %v)", idx, err)
	}
	segID := idx.Segments[0].Path
	raw, err := os.ReadFile(filepath.Join(base, string(event.DefaultTenant), rootID, string(artifact.KindSegment), segID))
	if err != nil {
		t.Fatalf("segment body not under <base>/<tenant>/<loop>/segment/: %v", err)
	}
	if len(raw) < 2 || raw[0] != 0x1f || raw[1] != 0x8b {
		t.Errorf("segment body not gzip at rest (first bytes % x)", raw[:min(2, len(raw))])
	}

	// (b) post-compaction main request carries the summary + recovery pointer.
	mu.Lock()
	body := mainReqBody
	mu.Unlock()
	if body == "" {
		t.Fatal("main request never reached the gateway")
	}
	if !strings.Contains(body, "Objective: verify seam") {
		t.Errorf("post-compaction main request missing the injected summary")
	}
	pointer := artifact.Ref{Tenant: event.DefaultTenant, Loop: rootID, Kind: artifact.KindSegment, ID: segID}.String()
	if !strings.Contains(body, "Recovery: grep your past at art://") {
		t.Errorf("post-compaction main request missing the art:// recovery pointer line:\n%s", body)
	}
	if !strings.Contains(body, pointer) {
		t.Errorf("recovery pointer does not name the archived segment %q", pointer)
	}

	// (c) segment_read round-trips the raw content the sink archived, resolved
	// through the binding the registry stamps onto the call.
	e := idx.Segments[0]
	args := `{"turn_range":"` + itoaTest(e.TurnStart) + `-` + itoaTest(e.TurnEnd) + `"}`
	res := reg.Execute(context.Background(), "segment_read", args)
	if res.IsError {
		t.Fatalf("segment_read error: %s", res.Output)
	}
	if !strings.Contains(res.Output, "RAWDATA-") {
		t.Errorf("segment_read did not round-trip the archived raw region; got:\n%s", tailStr(res.Output, 200))
	}

	// (d) grep on the recovery pointer finds the raw content.
	gargs, _ := json.Marshal(map[string]string{"pattern": "RAWDATA", "path": pointer})
	gres := reg.Execute(context.Background(), "grep", string(gargs))
	if gres.IsError || !strings.Contains(gres.Output, "RAWDATA") {
		t.Errorf("grep on %s did not find the raw content: %s", pointer, tailStr(gres.Output, 200))
	}
}

func itoaTest(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func tailStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
