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
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/segment"
	"github.com/ethanhinson/fuse/internal/segment/fssink"
	"github.com/ethanhinson/fuse/internal/tools"
)

// TestSegmentGatewaySeamArchivesAndRecovers drives a real agent turn through the
// production segment wiring (real model.Adapters POSTing to a scripted httptest
// gateway, the concrete FSSegmentSink installed via the same
// setActiveSegmentSink → currentSegmentSink → EnableSummarization path
// installSummarizer uses) and asserts the loop wiring end-to-end at the gateway
// seam (learning verify-tool-loop-at-gateway-seam):
//
//	(a) a segment file + index.json entry are written under ~/.fuse/sessions/<id>/
//	(b) the post-compaction MAIN request carries the summary WITH the
//	    "Recovery: grep your past at <path>" line
//	(c) a subsequent segment_read call round-trips the archived raw content.
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

	// Session dir keyed by a fixed root id, wired the production way.
	base := t.TempDir()
	const rootID = "root-seam"
	sink := fssink.NewFSSegmentSink(base, rootID)
	prev := activeSegmentSink
	t.Cleanup(func() { activeSegmentSink = prev })
	setActiveSegmentSink(sink)

	segDir := segment.SegmentsDir(base, rootID)
	tools.SetSegmentsDir(segDir)
	t.Cleanup(func() { tools.SetSegmentsDir("") })

	reg := tools.NewRegistry()
	for _, tl := range tools.DefaultTools() {
		reg.Register(tl)
	}

	mainAdapter := model.NewAdapter(srv.URL, "tkn", srv.Client())
	a := agent.New(mainAdapter, reg, gwNopRenderer{}, "cloud/model", "", 2, 128)
	a.ContextWindow = 4000
	// Wire Tier 2 exactly as installSummarizer does, threading the real sink via
	// currentSegmentSink() (not nil).
	sumAdapter := model.NewAdapter(srv.URL, "tkn", srv.Client())
	a.EnableSummarization(sumAdapter, "cloud/model", 2000, currentSegmentSink())

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

	// (a) segment file + index.json under the session dir.
	entries, err := os.ReadDir(segDir)
	if err != nil {
		t.Fatalf("segments dir not created: %v", err)
	}
	var mdFile, idxFile string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".md") {
			mdFile = e.Name()
		}
		if e.Name() == segment.IndexFileName {
			idxFile = e.Name()
		}
	}
	if mdFile == "" {
		t.Fatal("no segment .md file written under the session dir")
	}
	if idxFile == "" {
		t.Fatal("no index.json written under the session dir")
	}
	idx, err := segment.ReadIndex(segDir)
	if err != nil || len(idx.Segments) != 1 {
		t.Fatalf("index.json should have exactly 1 entry, got %+v (err %v)", idx, err)
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
	if !strings.Contains(body, "Recovery: grep your past at ") {
		t.Errorf("post-compaction main request missing the recovery pointer line:\n%s", body)
	}
	wantPath := filepath.Join(segDir, mdFile)
	if !strings.Contains(body, wantPath) {
		t.Errorf("recovery pointer does not point at the written segment file %q", wantPath)
	}

	// (c) segment_read round-trips the raw content the sink archived.
	var readTool tools.Tool
	for _, tl := range reg.Tools() {
		if tl.Name() == "segment_read" {
			readTool = tl
		}
	}
	if readTool == nil {
		t.Fatal("segment_read not registered")
	}
	e := idx.Segments[0]
	args := `{"turn_range":"` + itoaTest(e.TurnStart) + `-` + itoaTest(e.TurnEnd) + `"}`
	res := readTool.Execute(context.Background(), args)
	if res.IsError {
		t.Fatalf("segment_read error: %s", res.Output)
	}
	if !strings.Contains(res.Output, "RAWDATA-") {
		t.Errorf("segment_read did not round-trip the archived raw region; got:\n%s", tailStr(res.Output, 200))
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
