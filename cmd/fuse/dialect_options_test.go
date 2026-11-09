package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ethanhinson/fuse/internal/runtime"
)

func TestDialectOptionsReachTheWire(t *testing.T) {
	raw := &rawCapture{next: &recordingGateway{}}
	rt := runtime.New(harnessServerWith(t, `
models:
  default: fast
  fast: {id: cloud/fast, dialect_options: {cache_markers: true}}
max_turns: 2
throughput: {requests_per_minute: 6000}
`, raw))
	h, err := rt.StartLoop(context.Background(), runtime.LoopConfig{Task: "go"})
	if err != nil {
		t.Fatalf("StartLoop: %v", err)
	}
	_, _ = h.Wait()
	raw.mu.Lock()
	defer raw.mu.Unlock()
	if len(raw.bodies) == 0 {
		t.Fatal("no model call recorded")
	}
	var body struct {
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(raw.bodies[0], &body); err != nil {
		t.Fatal(err)
	}
	parts, ok := body.Messages[0].Content.([]any)
	if !ok || len(parts) == 0 {
		t.Fatalf("system content = %#v, want a content-part array", body.Messages[0].Content)
	}
	if _, ok := parts[0].(map[string]any)["cache_control"]; !ok {
		t.Errorf("system part has no cache_control: %v", parts[0])
	}
}
