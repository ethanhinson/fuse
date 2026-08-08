package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/internal/tools"
)

func resReq(t *testing.T, method string, params any) serverReq {
	t.Helper()
	var raw json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			t.Fatalf("marshal params: %v", err)
		}
		raw = b
	}
	return serverReq{JSONRPC: "2.0", ID: json.RawMessage(`"1"`), Method: method, Params: raw}
}

// TestServerResourcesListIncludesToolsResource: resources/list on the server
// includes the fuse://tools resource.
func TestServerResourcesListIncludesToolsResource(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(echoTool{})
	s := newTestServer(reg)

	resp := s.dispatch(context.Background(), resReq(t, "resources/list", nil))
	if resp.Error != nil {
		t.Fatalf("resources/list errored: %+v", resp.Error)
	}
	var r struct {
		Resources []struct {
			URI      string `json:"uri"`
			Name     string `json:"name"`
			MimeType string `json:"mimeType"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(resp.Result, &r); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	found := false
	for _, res := range r.Resources {
		if res.URI == "fuse://tools" {
			found = true
			if res.MimeType != "application/json" {
				t.Errorf("fuse://tools mimeType = %q, want application/json", res.MimeType)
			}
		}
	}
	if !found {
		t.Errorf("resources/list must include fuse://tools, got %+v", r.Resources)
	}
}

// TestServerResourcesReadToolCatalog: resources/read of fuse://tools returns the
// current tool catalog JSON derived from the registry.
func TestServerResourcesReadToolCatalog(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(echoTool{})
	s := newTestServer(reg)

	resp := s.dispatch(context.Background(), resReq(t, "resources/read", map[string]any{"uri": "fuse://tools"}))
	if resp.Error != nil {
		t.Fatalf("resources/read errored: %+v", resp.Error)
	}
	var r struct {
		Contents []struct {
			URI      string `json:"uri"`
			MimeType string `json:"mimeType"`
			Text     string `json:"text"`
		} `json:"contents"`
	}
	if err := json.Unmarshal(resp.Result, &r); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if len(r.Contents) != 1 {
		t.Fatalf("resources/read returned %d contents, want 1", len(r.Contents))
	}
	c := r.Contents[0]
	if c.URI != "fuse://tools" || c.MimeType != "application/json" {
		t.Errorf("content uri/mime = %q/%q, want fuse://tools/application/json", c.URI, c.MimeType)
	}
	if !strings.Contains(c.Text, "echo") {
		t.Errorf("tool catalog text must name the registered tool, got: %s", c.Text)
	}
	// The catalog is valid JSON that reflects the registry.
	var catalog struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal([]byte(c.Text), &catalog); err != nil {
		t.Fatalf("tool catalog is not valid JSON: %v", err)
	}
	if len(catalog.Tools) != 1 || catalog.Tools[0].Name != "echo" {
		t.Errorf("catalog tools = %+v, want [echo]", catalog.Tools)
	}
}

// TestServerResourcesReadUnknownURI: resources/read of an unknown URI returns an
// MCP resource-not-found error.
func TestServerResourcesReadUnknownURI(t *testing.T) {
	s := newTestServer(tools.NewRegistry())
	resp := s.dispatch(context.Background(), resReq(t, "resources/read", map[string]any{"uri": "fuse://nope"}))
	if resp.Error == nil {
		t.Fatalf("unknown resource uri should error, got result: %s", resp.Result)
	}
	if resp.Error.Code != ErrResourceNotFound {
		t.Errorf("error code = %d, want %d (ErrResourceNotFound)", resp.Error.Code, ErrResourceNotFound)
	}
}

// TestServerAdvertisesResourcesCapability: the initialize result advertises the
// resources capability with subscribe=true so a client's Supports gate passes.
func TestServerAdvertisesResourcesCapability(t *testing.T) {
	s := newTestServer(tools.NewRegistry())
	resp := s.dispatch(context.Background(), initReq(t, "2025-03-26"))
	var r struct {
		Capabilities map[string]json.RawMessage `json:"capabilities"`
	}
	if err := json.Unmarshal(resp.Result, &r); err != nil {
		t.Fatalf("unmarshal init result: %v", err)
	}
	caps := ServerCapabilities{raw: r.Capabilities}
	if !caps.Supports("resources") {
		t.Errorf("server must advertise resources capability, got %v", r.Capabilities)
	}
	if !caps.Supports("resources.subscribe") {
		t.Errorf("server must advertise resources.subscribe, got %v", r.Capabilities)
	}
}
