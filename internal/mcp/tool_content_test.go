package mcp

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func TestRenderMCPResult(t *testing.T) {
	img := base64.StdEncoding.EncodeToString(make([]byte, 2048)) // 2 KB decoded

	tests := []struct {
		name       string
		raw        string
		wantSubs   []string // substrings that must appear
		wantAbsent []string // substrings that must NOT appear
		wantErr    bool
	}{
		{
			name:     "text block verbatim",
			raw:      `{"content":[{"type":"text","text":"hello world"}]}`,
			wantSubs: []string{"hello world"},
		},
		{
			name:     "multiple text blocks joined by newline",
			raw:      `{"content":[{"type":"text","text":"line one"},{"type":"text","text":"line two"}]}`,
			wantSubs: []string{"line one\nline two"},
		},
		{
			name:     "image rendered as descriptor with size (not dropped)",
			raw:      `{"content":[{"type":"image","mimeType":"image/png","data":"` + img + `"}]}`,
			wantSubs: []string{"[image: image/png", "2.0 KB"},
		},
		{
			name:     "audio descriptor",
			raw:      `{"content":[{"type":"audio","mimeType":"audio/wav","data":"AAAA"}]}`,
			wantSubs: []string{"[audio: audio/wav"},
		},
		{
			name:     "text plus image both surface",
			raw:      `{"content":[{"type":"text","text":"see chart"},{"type":"image","mimeType":"image/png","data":"AAAA"}]}`,
			wantSubs: []string{"see chart", "[image: image/png"},
		},
		{
			name:     "embedded resource with text surfaces the text",
			raw:      `{"content":[{"type":"resource","resource":{"uri":"file:///a.txt","mimeType":"text/plain","text":"file body"}}]}`,
			wantSubs: []string{"[resource: file:///a.txt]", "file body"},
		},
		{
			name:     "embedded resource blob shows descriptor",
			raw:      `{"content":[{"type":"resource","resource":{"uri":"file:///a.bin","mimeType":"application/octet-stream","blob":"AAAAAAAA"}}]}`,
			wantSubs: []string{"[resource: file:///a.bin", "application/octet-stream"},
		},
		{
			name:     "resource_link",
			raw:      `{"content":[{"type":"resource_link","name":"README","uri":"file:///README.md","mimeType":"text/markdown","description":"the readme"}]}`,
			wantSubs: []string{"[resource: README", "file:///README.md", "text/markdown", "the readme"},
		},
		{
			name:     "unknown content type is labeled, not dropped",
			raw:      `{"content":[{"type":"hologram"}]}`,
			wantSubs: []string{`[unsupported content: "hologram"]`},
		},
		{
			name:     "isError propagates",
			raw:      `{"content":[{"type":"text","text":"boom"}],"isError":true}`,
			wantSubs: []string{"boom"},
			wantErr:  true,
		},
		{
			name:     "structuredContent shown when no content blocks",
			raw:      `{"structuredContent":{"temp":21,"unit":"C"}}`,
			wantSubs: []string{"[structured content]", "\"temp\": 21"},
		},
		{
			name:     "empty envelope yields a placeholder, never blank",
			raw:      `{"content":[]}`,
			wantSubs: []string{"[no content]"},
		},
		{
			name:     "image-only result is never blank (the core repair)",
			raw:      `{"content":[{"type":"image","mimeType":"image/jpeg","data":"AAAA"}]}`,
			wantSubs: []string{"[image: image/jpeg"},
			wantAbsent: []string{
				"[no content]",
			},
		},
		{
			name:     "non-envelope JSON falls back to raw",
			raw:      `{"weird":"payload"}`,
			wantSubs: []string{"weird", "payload"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := renderMCPResult(json.RawMessage(tc.raw))
			if res.IsError != tc.wantErr {
				t.Errorf("IsError = %v, want %v", res.IsError, tc.wantErr)
			}
			for _, sub := range tc.wantSubs {
				if !strings.Contains(res.Output, sub) {
					t.Errorf("output missing %q\n---\n%s", sub, res.Output)
				}
			}
			for _, sub := range tc.wantAbsent {
				if strings.Contains(res.Output, sub) {
					t.Errorf("output should not contain %q\n---\n%s", sub, res.Output)
				}
			}
			if strings.TrimSpace(res.Output) == "" {
				t.Error("output is blank — MCP results must never render empty")
			}
		})
	}
}

func TestHumanBytes(t *testing.T) {
	cases := map[int]string{0: "0 B", 512: "512 B", 2048: "2.0 KB", 5 * 1024 * 1024: "5.0 MB"}
	for n, want := range cases {
		if got := humanBytes(n); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", n, got, want)
		}
	}
}
