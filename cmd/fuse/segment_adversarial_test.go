package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/segment"
	"github.com/ethanhinson/fuse/internal/tools"
)

// The adversarial gate for the #0030 scope expansion: prove that when the model
// needs data that now lives in a gzipped archive, it recovers the SAME answer
// with NO degradation versus the uncompressed original.
//
// The needle fact lives ONLY in an archived, pruned-from-context segment. Two
// arms back that segment differently — one gzip-compressed (.md.gz), one
// plaintext (.md) — and drive the REAL agent loop (a.Run) against a scripted
// gateway that must call segment_read to answer. The two arms MUST produce the
// same final answer (4217). Any divergence = FAIL.

const needleFact = "the API key rotation interval is 4217 seconds"
const needleAnswer = "4217"

// renderNeedleSegment builds the on-disk rendered segment body carrying the
// needle in an OLD (turn 3-5) tool region, exactly as the sink would render it.
func renderNeedleSegment() []byte {
	seg := segment.Segment{
		TurnStart: 3,
		TurnEnd:   5,
		Tools:     []string{"read_file"},
		Summary:   "Read the rotation config; details in the raw region.",
		Messages: []model.Message{
			{Role: "assistant", ToolCalls: []model.ToolCall{{ID: "old1", Name: "read_file"}}},
			{Role: "tool", Name: "read_file", ToolCallID: "old1", Content: "config dump:\n" + needleFact + "\nother=irrelevant"},
		},
	}
	return []byte(segment.RenderSegment(seg))
}

// seedSegmentsDir writes a segments dir with ONE segment (turns 3-5) carrying
// the needle, backed by the requested on-disk name/encoding, plus a matching
// index.json. It returns the segments dir. gzipIt controls compression;
// systemGzip (when true and available) uses the SYSTEM gzip binary to prove
// cross-tool compatibility.
func seedSegmentsDir(t *testing.T, gzipIt, systemGzip bool) string {
	t.Helper()
	base := t.TempDir()
	segDir := segment.SegmentsDir(base, "adv-root")
	if err := os.MkdirAll(segDir, 0o700); err != nil {
		t.Fatal(err)
	}
	rendered := renderNeedleSegment()

	var onDiskName string
	if gzipIt {
		onDiskName = "3-5-1.md.gz"
		gzPath := filepath.Join(segDir, onDiskName)
		if systemGzip {
			gzipBin, err := exec.LookPath("gzip")
			if err != nil {
				t.Skip("system gzip binary not available")
			}
			plain := filepath.Join(segDir, "3-5-1.md")
			if err := os.WriteFile(plain, rendered, 0o600); err != nil {
				t.Fatal(err)
			}
			if out, err := exec.Command(gzipBin, plain).CombinedOutput(); err != nil {
				t.Fatalf("system gzip failed: %v (%s)", err, out)
			}
			// gzip leaves "3-5-1.md.gz" — the name we indexed.
		} else {
			var buf bytes.Buffer
			zw := gzip.NewWriter(&buf)
			if _, err := zw.Write(rendered); err != nil {
				t.Fatal(err)
			}
			if err := zw.Close(); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(gzPath, buf.Bytes(), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	} else {
		onDiskName = "3-5-1.md"
		if err := os.WriteFile(filepath.Join(segDir, onDiskName), rendered, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	idx := segment.Index{
		SessionID: "adv-root",
		Segments: []segment.IndexEntry{{
			TurnStart: 3, TurnEnd: 5, Tools: []string{"read_file"}, Path: onDiskName,
		}},
	}
	b, _ := json.Marshal(idx)
	if err := os.WriteFile(filepath.Join(segDir, segment.IndexFileName), b, 0o600); err != nil {
		t.Fatal(err)
	}
	return segDir
}

// driveNeedleRecovery drives the real a.Run loop against a scripted gateway that
// must recover the needle via segment_read to answer, and returns the final
// answer text. The gateway plays a two-step agent: first turn it has no needle
// in context and issues a segment_read tool call; second turn (after the tool
// result carrying the recovered raw region is appended) it emits the answer.
func driveNeedleRecovery(t *testing.T, segDir string) string {
	t.Helper()

	var (
		mu        sync.Mutex
		sawNeedle bool // the segment_read result reached the gateway with the needle
		mainTurns int
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")

		mu.Lock()
		mainTurns++
		// Once the recovered segment text (with the needle) has been appended to
		// the request as a tool result, the model can answer.
		if strings.Contains(string(body), needleFact) {
			sawNeedle = true
		}
		answerable := sawNeedle
		mu.Unlock()

		if answerable {
			// The model has the recovered detail; answer with the needle.
			io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"The interval is `+needleAnswer+` seconds."}}],"usage":{"prompt_tokens":10,"completion_tokens":3}}`)
			return
		}
		// No needle yet: recover it from the archived segment via segment_read.
		resp := map[string]any{
			"choices": []map[string]any{{"message": map[string]any{
				"role":    "assistant",
				"content": "",
				"tool_calls": []map[string]any{{
					"id": "rec1", "type": "function",
					"function": map[string]any{"name": "segment_read", "arguments": `{"turn_range":"3-5"}`},
				}},
			}}},
			"usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 5},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	// Point segment_read at the seeded archive dir (the production wiring).
	tools.SetSegmentsDir(segDir)
	t.Cleanup(func() { tools.SetSegmentsDir("") })

	reg := tools.NewRegistry()
	for _, tl := range tools.DefaultTools(nil) {
		reg.Register(tl)
	}

	adapter := model.NewAdapter(srv.URL, "tkn", srv.Client())
	a := agent.New(adapter, reg, gwNopRenderer{}, "cloud/model", "", 4, 128)

	// The summary already replaced the raw region in live context; the model must
	// go back to the archive for the detail. The user asks a question ONLY
	// answerable from the needle.
	history := []model.Message{
		{Role: "user", Content: "What is the API key rotation interval in seconds? Recover it from the archived segment if needed."},
		{Role: "assistant", Content: "Earlier I read the rotation config (turns 3-5), now compacted; the raw detail was archived."},
	}
	hist, err := a.Run(context.Background(), history)
	if err != nil {
		t.Fatalf("run error: %v", err)
	}

	// Anti-vacuity: segment_read must actually have been called and returned the
	// needle to the gateway.
	mu.Lock()
	defer mu.Unlock()
	if !sawNeedle {
		t.Fatal("segment_read never delivered the needle to the model — recovery did not happen, assertion is vacuous")
	}

	// Return the final assistant answer.
	for i := len(hist) - 1; i >= 0; i-- {
		if hist[i].Role == "assistant" && hist[i].Content != "" {
			return hist[i].Content
		}
	}
	t.Fatal("no final assistant answer in history")
	return ""
}

// TestAdversarialGzVsPlaintextParity is the gate: the needle answer must be
// identical whether the archived segment is gzip-compressed or plaintext.
func TestAdversarialGzVsPlaintextParity(t *testing.T) {
	// Arm A: gz-backed archive (Go gzip.Writer).
	gzAnswer := driveNeedleRecovery(t, seedSegmentsDir(t, true, false))
	if !strings.Contains(gzAnswer, needleAnswer) {
		t.Fatalf("gz arm did not recover the needle: %q", gzAnswer)
	}

	// Arm B (control): plaintext-backed archive.
	plainAnswer := driveNeedleRecovery(t, seedSegmentsDir(t, false, false))
	if !strings.Contains(plainAnswer, needleAnswer) {
		t.Fatalf("plaintext control arm did not recover the needle: %q", plainAnswer)
	}

	// The gate: ANY divergence between the two arms fails.
	if gzAnswer != plainAnswer {
		t.Fatalf("gz vs plaintext answer DIVERGED (degradation!):\n gz=%q\nplain=%q", gzAnswer, plainAnswer)
	}
}

// TestAdversarialSystemGzipParity proves cross-tool compatibility: a segment
// compressed by the SYSTEM gzip binary recovers the SAME answer as plaintext.
func TestAdversarialSystemGzipParity(t *testing.T) {
	sysGzAnswer := driveNeedleRecovery(t, seedSegmentsDir(t, true, true))
	plainAnswer := driveNeedleRecovery(t, seedSegmentsDir(t, false, false))
	if !strings.Contains(sysGzAnswer, needleAnswer) {
		t.Fatalf("system-gzip arm did not recover the needle: %q", sysGzAnswer)
	}
	if sysGzAnswer != plainAnswer {
		t.Fatalf("system-gzip vs plaintext answer DIVERGED:\n sysgz=%q\nplain=%q", sysGzAnswer, plainAnswer)
	}
}
