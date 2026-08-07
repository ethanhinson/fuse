package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/internal/mcp"
)

func TestRenderLiveStatus(t *testing.T) {
	var buf bytes.Buffer
	renderLiveStatus(&buf, []mcp.ServerStatus{
		{
			Name:            "ctx",
			Transport:       "stdio",
			AuthType:        "none",
			Connected:       true,
			Tools:           []string{"a", "b"},
			ProtocolVersion: "2025-03-26",
			Capabilities:    []string{"resources.subscribe", "logging"},
		},
		{
			Name:            "bare",
			Transport:       "stdio",
			Connected:       true,
			ProtocolVersion: "2024-11-05",
			Capabilities:    nil,
		},
		{
			Name:      "down",
			Transport: "http",
			Connected: false,
			Error:     "boom",
		},
	})
	out := buf.String()

	if !strings.Contains(out, "PROTO") || !strings.Contains(out, "CAPS") {
		t.Errorf("header must include PROTO and CAPS columns:\n%s", out)
	}
	if !strings.Contains(out, "2025-03-26") || !strings.Contains(out, "resources.subscribe,logging") {
		t.Errorf("connected server must show its version and capabilities:\n%s", out)
	}
	// A connected server advertising no capabilities shows "none", not blank.
	bareLine := lineWith(out, "bare")
	if !strings.Contains(bareLine, "2024-11-05") || !strings.Contains(bareLine, "none") {
		t.Errorf("bare server line should show version + none: %q", bareLine)
	}
	// A disconnected server shows "-" for proto/caps.
	downLine := lineWith(out, "down")
	if !strings.Contains(downLine, "error") {
		t.Errorf("down server should show error status: %q", downLine)
	}
}

func lineWith(out, needle string) string {
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, needle) {
			return l
		}
	}
	return ""
}
