package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/ethanhinson/fuse/internal/permissions"
	"github.com/ethanhinson/fuse/internal/tools"
)

// serverReq is the server-side JSON-RPC 2.0 request frame. ID is RawMessage
// so it roundtrips correctly for both string and integer IDs from external clients
// (Claude Code uses strings; Cursor/Codex may use integers).
type serverReq struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// serverResp is the server-side JSON-RPC 2.0 response frame.
type serverResp struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *jsonrpcError   `json:"error,omitempty"`
}

// Server is an MCP server over stdio (JSON-RPC 2.0). It exposes a tool
// registry through a permission gate, designed to be spawned as
// `fuse mcp-server` by external AI assistants via their MCP configuration.
type Server struct {
	reg  *tools.Registry
	gate *permissions.PermissionGate
	enc  *json.Encoder
	dec  *json.Decoder
}

// NewServer creates a Server that reads JSON-RPC 2.0 from r and writes to w.
func NewServer(r io.Reader, w io.Writer, reg *tools.Registry, gate *permissions.PermissionGate) *Server {
	return &Server{
		reg:  reg,
		gate: gate,
		enc:  json.NewEncoder(w),
		dec:  json.NewDecoder(r),
	}
}

// Serve reads and dispatches JSON-RPC 2.0 requests until EOF or decode error.
func (s *Server) Serve(ctx context.Context) error {
	for {
		var req serverReq
		if err := s.dec.Decode(&req); err != nil {
			if err == io.EOF {
				return nil
			}
			return fmt.Errorf("mcp server: decode: %w", err)
		}
		// Notifications (e.g. "initialized") have no id — don't respond.
		if len(req.ID) == 0 {
			continue
		}
		resp := s.dispatch(ctx, req)
		if err := s.enc.Encode(resp); err != nil {
			return fmt.Errorf("mcp server: encode: %w", err)
		}
	}
}

func (s *Server) dispatch(ctx context.Context, req serverReq) serverResp {
	switch req.Method {
	case "initialize":
		return s.ok(req.ID, map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "fuse", "version": "1.0.0"},
		})
	case "tools/list":
		return s.handleList(req.ID)
	case "tools/call":
		return s.handleCall(ctx, req)
	default:
		return s.errResp(req.ID, ErrMethodNotFound, "method not found: "+req.Method)
	}
}

func (s *Server) handleList(id json.RawMessage) serverResp {
	schemas := s.reg.Schemas()
	list := make([]map[string]any, 0, len(schemas))
	for _, sc := range schemas {
		list = append(list, map[string]any{
			"name":        sc.Name,
			"description": sc.Description,
			"inputSchema": sc.Parameters,
		})
	}
	return s.ok(id, map[string]any{"tools": list})
}

type callParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

func (s *Server) handleCall(ctx context.Context, req serverReq) serverResp {
	var p callParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		// Invalid params is -32602 in JSON-RPC 2.0 (not -32600, which is
		// "Invalid Request" — a malformed Request object, a different condition).
		return s.errResp(req.ID, ErrInvalidParams, "invalid params: "+err.Error())
	}
	// An unknown tool name is an MCP-specific protocol error (-32900), distinct
	// from a registered-but-disabled tool, which still flows through the gate and
	// returns an isError tool result.
	if !s.reg.Has(p.Name) {
		return s.errResp(req.ID, ErrToolNotFound, "tool not found: "+p.Name)
	}
	args := string(p.Arguments)
	if args == "" || args == "null" {
		args = "{}"
	}
	result := s.gate.Execute(ctx, p.Name, args)
	return s.ok(req.ID, map[string]any{
		"content": []map[string]any{{"type": "text", "text": result.Output}},
		"isError": result.IsError,
	})
}

func (s *Server) ok(id json.RawMessage, v any) serverResp {
	raw, _ := json.Marshal(v)
	return serverResp{JSONRPC: "2.0", ID: id, Result: raw}
}

func (s *Server) errResp(id json.RawMessage, code int, msg string) serverResp {
	return serverResp{JSONRPC: "2.0", ID: id, Error: &jsonrpcError{Code: code, Message: msg}}
}
