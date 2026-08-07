package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/ethanhinson/fuse/internal/tools"
)

// mcpToolDef is the shape of a single tool entry in the tools/list response.
type mcpToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// MCPTool wraps a single MCP server tool as a tools.Tool.
type MCPTool struct {
	client      mcpConn
	serverName  string
	toolName    string
	description string
	inputSchema map[string]any
}

func (t *MCPTool) Name() string               { return "mcp:" + t.serverName + "/" + t.toolName }
func (t *MCPTool) Description() string        { return t.description }
func (t *MCPTool) Parameters() map[string]any { return t.inputSchema }

// Execute calls tools/call on the MCP server and wraps the result.
func (t *MCPTool) Execute(ctx context.Context, args string) tools.Result {
	var params any
	if err := json.Unmarshal([]byte(args), &params); err != nil {
		return tools.Result{IsError: true, Output: fmt.Sprintf("bad arguments: %v", err)}
	}

	raw, err := t.client.call(ctx, "tools/call", map[string]any{
		"name":      t.toolName,
		"arguments": params,
	})
	if err != nil {
		// Surface a downstream server's JSON-RPC error code alongside the message
		// so the model can distinguish MCP-specific conditions (e.g. -32900 tool
		// not found) from a generic failure. The code is carried by *RPCError,
		// preserved through the client call path.
		var rpcErr *RPCError
		if errors.As(err, &rpcErr) {
			return tools.Result{IsError: true, Output: fmt.Sprintf("mcp %s/%s: [code %d] %s", t.serverName, t.toolName, rpcErr.Code, rpcErr.Message)}
		}
		return tools.Result{IsError: true, Output: fmt.Sprintf("mcp %s/%s: %v", t.serverName, t.toolName, err)}
	}

	return renderMCPResult(raw)
}

// mcpCallResult is the MCP v2025-03-26 tools/call response envelope. `content`
// is an ordered list of typed blocks; `structuredContent` is an optional JSON
// object for structured tools; `isError` marks a tool-level failure.
type mcpCallResult struct {
	Content           []mcpContentBlock `json:"content"`
	StructuredContent json.RawMessage   `json:"structuredContent"`
	IsError           bool              `json:"isError"`
}

// mcpContentBlock is a single content block. The MCP spec defines several types
// (text, image, audio, resource, resource_link); the fields are a union across
// them, populated per the block's `type`.
type mcpContentBlock struct {
	Type string `json:"type"`
	// text
	Text string `json:"text"`
	// image / audio
	Data     string `json:"data"` // base64
	MimeType string `json:"mimeType"`
	// resource_link
	URI         string `json:"uri"`
	Name        string `json:"name"`
	Description string `json:"description"`
	// embedded resource
	Resource *mcpResource `json:"resource"`
}

// mcpResource is the payload of an embedded `resource` content block.
type mcpResource struct {
	URI      string `json:"uri"`
	MimeType string `json:"mimeType"`
	Text     string `json:"text"`
	Blob     string `json:"blob"` // base64
}

// renderMCPResult turns an MCP tools/call response into a tools.Result. Every
// content block type is rendered to a faithful textual form — text verbatim,
// non-text blocks (image/audio/resource) as descriptors — so nothing is
// silently dropped (the transcript AND the model see what the tool returned).
// A payload that is not the standard envelope falls back to its raw JSON.
func renderMCPResult(raw json.RawMessage) tools.Result {
	var res mcpCallResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return tools.Result{Output: string(raw)}
	}

	parts := make([]string, 0, len(res.Content))
	for _, b := range res.Content {
		if s := renderContentBlock(b); s != "" {
			parts = append(parts, s)
		}
	}
	out := strings.Join(parts, "\n")

	// Surface structuredContent only when there are no content blocks — a
	// structured tool SHOULD also emit a text block, so this avoids duplication.
	if out == "" && len(res.StructuredContent) > 0 {
		out = "[structured content]\n" + prettyJSON(res.StructuredContent)
	}
	if out == "" {
		// A recognized-but-empty envelope (a "content" key was present, or
		// isError/structuredContent) reads as "[no content]". A payload that
		// isn't the standard envelope at all falls back to its raw JSON so we
		// never hide a tool's output.
		recognized := res.Content != nil || len(res.StructuredContent) > 0 || res.IsError
		if !recognized {
			if trimmed := strings.TrimSpace(string(raw)); trimmed != "" && trimmed != "{}" && trimmed != "null" {
				out = trimmed
			}
		}
		if out == "" {
			out = "[no content]"
		}
	}
	return tools.Result{Output: out, IsError: res.IsError}
}

// renderContentBlock renders one content block to text.
func renderContentBlock(b mcpContentBlock) string {
	switch b.Type {
	case "text":
		return b.Text
	case "image":
		return fmt.Sprintf("[image: %s, %s]", orValue(b.MimeType, "unknown type"), humanBytes(b64DecodedLen(b.Data)))
	case "audio":
		return fmt.Sprintf("[audio: %s, %s]", orValue(b.MimeType, "unknown type"), humanBytes(b64DecodedLen(b.Data)))
	case "resource_link":
		return renderResourceLink(b)
	case "resource":
		return renderEmbeddedResource(b.Resource)
	case "":
		return "" // malformed block with no type — skip
	default:
		return fmt.Sprintf("[unsupported content: %q]", b.Type)
	}
}

func renderResourceLink(b mcpContentBlock) string {
	label := orValue(b.Name, b.URI)
	s := "[resource: " + orValue(label, "(unnamed)")
	if b.URI != "" && b.URI != label {
		s += " — " + b.URI
	}
	if b.MimeType != "" {
		s += " (" + b.MimeType + ")"
	}
	s += "]"
	if b.Description != "" {
		s += " " + b.Description
	}
	return s
}

func renderEmbeddedResource(r *mcpResource) string {
	if r == nil {
		return "[resource: (empty)]"
	}
	if r.Text != "" {
		return fmt.Sprintf("[resource: %s]\n%s", orValue(r.URI, "inline"), r.Text)
	}
	return fmt.Sprintf("[resource: %s (%s, %s)]", orValue(r.URI, "inline"), orValue(r.MimeType, "unknown type"), humanBytes(b64DecodedLen(r.Blob)))
}

// prettyJSON re-indents a JSON payload; on any failure it returns the input.
func prettyJSON(raw json.RawMessage) string {
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return string(raw)
	}
	return buf.String()
}

// b64DecodedLen returns the decoded byte length of a standard base64 string
// without allocating the decoded buffer.
func b64DecodedLen(s string) int {
	n := len(s)
	if n == 0 {
		return 0
	}
	pad := 0
	switch {
	case strings.HasSuffix(s, "=="):
		pad = 2
	case strings.HasSuffix(s, "="):
		pad = 1
	}
	return n/4*3 - pad
}

// humanBytes formats a byte count as a short human-readable string.
func humanBytes(n int) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	default:
		return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
	}
}

// orValue returns v, or fallback when v is empty.
func orValue(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// compile-time interface check
var _ tools.Tool = (*MCPTool)(nil)
