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
	"github.com/ethanhinson/fuse/internal/artifact"
	artfs "github.com/ethanhinson/fuse/internal/artifact/fsstore"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/segment"
	"github.com/ethanhinson/fuse/internal/tools"
)

// The adversarial gate for the #0030 scope expansion: prove that when the model
// needs data that now lives in a gzipped archive, it recovers the SAME answer
// with NO degradation versus the uncompressed original.
//
// The needle fact lives ONLY in an archived, pruned-from-context segment. Two
// arms back that segment artifact differently — one gzip-compressed, one
// plaintext — and drive the REAL agent loop (a.Run) against a scripted
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

// seedSegments archives ONE segment (turns 3-5) carrying the needle in an
// artifact/fsstore under t.TempDir(), plus the matching index artifact, and
// returns the loop's binding. The segment artifact holds the SAME rendered bytes
// in every arm: plaintext, or gzip-compressed (gzipIt) by Go's gzip.Writer or,
// when systemGzip is set and available, by the SYSTEM gzip binary to prove
// cross-tool compatibility. The store adds its own at-rest compression on top;
// recovery must see through both layers.
func seedSegments(t *testing.T, gzipIt, systemGzip bool) artifact.Binding {
	t.Helper()
	b := artifact.Binding{Store: artfs.New(t.TempDir()), Tenant: event.DefaultTenant, Loop: "adv-root"}
	rendered := renderNeedleSegment()

	body := rendered
	if gzipIt {
		if systemGzip {
			gzipBin, err := exec.LookPath("gzip")
			if err != nil {
				t.Skip("system gzip binary not available")
			}
			plain := filepath.Join(t.TempDir(), "3-5-1.md")
			if err := os.WriteFile(plain, rendered, 0o600); err != nil {
				t.Fatal(err)
			}
			if out, err := exec.Command(gzipBin, plain).CombinedOutput(); err != nil {
				t.Fatalf("system gzip failed: %v (%s)", err, out)
			}
			gz, err := os.ReadFile(plain + ".gz")
			if err != nil {
				t.Fatal(err)
			}
			body = gz
		} else {
			var buf bytes.Buffer
			zw := gzip.NewWriter(&buf)
			if _, err := zw.Write(rendered); err != nil {
				t.Fatal(err)
			}
			if err := zw.Close(); err != nil {
				t.Fatal(err)
			}
			body = buf.Bytes()
		}
		if len(body) < 2 || body[0] != 0x1f || body[1] != 0x8b {
			t.Fatalf("gz arm body is not gzip (first bytes % x)", body[:min(2, len(body))])
		}
	}

	ctx := context.Background()
	meta, err := b.Put(ctx, artifact.KindSegment, bytes.NewReader(body), artifact.Meta{Turn: 3})
	if err != nil {
		t.Fatal(err)
	}
	idx := segment.Index{
		SessionID: "adv-root",
		Segments: []segment.IndexEntry{{
			TurnStart: 3, TurnEnd: 5, Tools: []string{"read_file"}, Path: meta.Ref.ID,
		}},
	}
	raw, _ := json.Marshal(idx)
	if _, err := b.PutID(ctx, artifact.KindSegmentIndex, segment.IndexArtifactID, bytes.NewReader(raw), artifact.Meta{}); err != nil {
		t.Fatal(err)
	}
	return b
}

// driveNeedleRecovery drives the real a.Run loop against a scripted gateway that
// must recover the needle via segment_read to answer, and returns the final
// answer text. The gateway plays a two-step agent: first turn it has no needle
// in context and issues a segment_read tool call; second turn (after the tool
// result carrying the recovered raw region is appended) it emits the answer.
func driveNeedleRecovery(t *testing.T, b artifact.Binding) string {
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

	// Bind the registry to the seeded loop (the production wiring): segment_read
	// resolves through the binding the registry stamps onto each tool call.
	reg := tools.NewRegistry()
	for _, tl := range tools.DefaultTools(nil) {
		reg.Register(tl)
	}
	reg.WithArtifacts(b.Store, b.Tenant, b.Loop)

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
	gzAnswer := driveNeedleRecovery(t, seedSegments(t, true, false))
	if !strings.Contains(gzAnswer, needleAnswer) {
		t.Fatalf("gz arm did not recover the needle: %q", gzAnswer)
	}

	// Arm B (control): plaintext-backed archive.
	plainAnswer := driveNeedleRecovery(t, seedSegments(t, false, false))
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
	sysGzAnswer := driveNeedleRecovery(t, seedSegments(t, true, true))
	plainAnswer := driveNeedleRecovery(t, seedSegments(t, false, false))
	if !strings.Contains(sysGzAnswer, needleAnswer) {
		t.Fatalf("system-gzip arm did not recover the needle: %q", sysGzAnswer)
	}
	if sysGzAnswer != plainAnswer {
		t.Fatalf("system-gzip vs plaintext answer DIVERGED:\n sysgz=%q\nplain=%q", sysGzAnswer, plainAnswer)
	}
}
