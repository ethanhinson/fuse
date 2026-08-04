//go:build integration

package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestIntegration_HTTP_NoAuth dials the @modelcontextprotocol/server-everything
// reference server directly over HTTP/SSE (no auth) and exercises tool discovery
// and a round-trip tools/call.
func TestIntegration_HTTP_NoAuth(t *testing.T) {
	requireServices(t)

	client, err := newHTTPClient("everything", everythingBaseURL, "")
	if err != nil {
		t.Fatalf("newHTTPClient: %v", err)
	}
	defer client.stop()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	raw, err := client.call(ctx, "tools/list", nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	var listResp struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &listResp); err != nil {
		t.Fatalf("parse tools/list: %v (raw: %s)", err, raw)
	}
	// server-everything exposes `echo` and `get-sum` (its add-equivalent), among
	// others. Assert both discovery anchors are present.
	for _, want := range []string{"echo", "get-sum"} {
		if !hasTool(listResp.Tools, want) {
			t.Fatalf("expected %q tool from server-everything; got %+v", want, listResp.Tools)
		}
	}

	// Round-trip 1: echo returns its input (result text is "Echo: <message>").
	const marker = "integration-roundtrip-42"
	echoRaw, err := client.call(ctx, "tools/call", map[string]any{
		"name":      "echo",
		"arguments": map[string]any{"message": marker},
	})
	if err != nil {
		t.Fatalf("tools/call echo: %v", err)
	}
	if !strings.Contains(string(echoRaw), marker) {
		t.Fatalf("echo round-trip did not return marker %q; raw: %s", marker, echoRaw)
	}

	// Round-trip 2: get-sum computes over structured numeric args.
	sumRaw, err := client.call(ctx, "tools/call", map[string]any{
		"name":      "get-sum",
		"arguments": map[string]any{"a": 2, "b": 3},
	})
	if err != nil {
		t.Fatalf("tools/call get-sum: %v", err)
	}
	if !strings.Contains(string(sumRaw), "5") {
		t.Fatalf("get-sum(2,3) did not return 5; raw: %s", sumRaw)
	}
}
