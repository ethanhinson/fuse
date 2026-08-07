package mcp

import (
	"encoding/json"
	"reflect"
	"testing"
)

func capsFrom(t *testing.T, capabilities string) ServerCapabilities {
	t.Helper()
	caps, _ := parseInitializeResult(json.RawMessage(`{"capabilities":` + capabilities + `}`))
	return caps
}

func TestServerCapabilitiesSupports(t *testing.T) {
	tests := []struct {
		name         string
		capabilities string
		key          string
		want         bool
	}{
		{"bare present object", `{"logging":{}}`, "logging", true},
		{"bare absent", `{"logging":{}}`, "streaming", false},
		{"bare null value", `{"logging":null}`, "logging", false},
		{"bare explicit true", `{"streaming":true}`, "streaming", true},
		{"bare explicit false", `{"streaming":false}`, "streaming", false},
		{"nested subscribe true", `{"resources":{"subscribe":true}}`, "resources.subscribe", true},
		{"nested subscribe false", `{"resources":{"subscribe":false}}`, "resources.subscribe", false},
		{"nested key missing", `{"resources":{"listChanged":true}}`, "resources.subscribe", false},
		{"nested parent absent", `{"logging":{}}`, "resources.subscribe", false},
		{"nested parent malformed", `{"resources":"nope"}`, "resources.subscribe", false},
		{"empty capabilities", `{}`, "resources.subscribe", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			caps := capsFrom(t, tc.capabilities)
			if got := caps.Supports(tc.key); got != tc.want {
				t.Errorf("Supports(%q) on %s = %v, want %v", tc.key, tc.capabilities, got, tc.want)
			}
		})
	}
}

func TestParseInitializeResultFull(t *testing.T) {
	raw := json.RawMessage(`{
		"protocolVersion": "2025-03-26",
		"capabilities": {"logging":{}, "resources":{"subscribe":true,"listChanged":false}}
	}`)
	caps, ver := parseInitializeResult(raw)
	if ver != "2025-03-26" {
		t.Errorf("protocolVersion = %q, want 2025-03-26", ver)
	}
	if !caps.Supports("logging") {
		t.Error("expected logging supported")
	}
	if !caps.Supports("resources.subscribe") {
		t.Error("expected resources.subscribe supported")
	}
	if caps.Supports("resources.listChanged") {
		t.Error("resources.listChanged is false — must not be supported")
	}
}

func TestParseInitializeResultMinimalNoCapabilities(t *testing.T) {
	// A pre-2025-03-26 server returns no capabilities field at all.
	raw := json.RawMessage(`{"protocolVersion":"2024-11-05","serverInfo":{"name":"old"}}`)
	caps, ver := parseInitializeResult(raw)
	if ver != "2024-11-05" {
		t.Errorf("protocolVersion = %q, want 2024-11-05", ver)
	}
	if caps.Supports("resources.subscribe") || caps.Supports("logging") {
		t.Error("a server advertising no capabilities must support nothing optional")
	}
}

func TestParseInitializeResultGarbageCapabilities(t *testing.T) {
	// capabilities is a string, not an object — must fail open, not panic/error.
	raw := json.RawMessage(`{"protocolVersion":"2025-03-26","capabilities":"broken"}`)
	caps, ver := parseInitializeResult(raw)
	if ver != "2025-03-26" {
		t.Errorf("protocolVersion = %q, want 2025-03-26", ver)
	}
	if caps.Supports("logging") {
		t.Error("garbage capabilities must support nothing")
	}
}

func TestServerCapabilitiesKeys(t *testing.T) {
	caps := capsFrom(t, `{"logging":{}, "resources":{"subscribe":true,"listChanged":false}, "tools":{}}`)
	got := caps.Keys()
	want := []string{"logging", "resources", "resources.subscribe", "tools"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Keys() = %v, want %v", got, want)
	}
}
