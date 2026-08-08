package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"

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

	// encMu serializes every write on the shared encoder. A tools/call carrying
	// _meta.progressToken emits id-less $/progress notifications MID-execution on
	// the same encoder as its eventual response — without this lock those writes
	// could interleave and corrupt the JSON-RPC stream.
	encMu sync.Mutex

	// subMu guards subscribed — the set of resource URIs this connection has
	// resources/subscribe'd to. pushResourceUpdated only writes a
	// notifications/resources/updated frame for a URI in this set (Task 5).
	subMu      sync.Mutex
	subscribed map[string]bool
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
		// tools/call may emit id-less $/progress frames mid-execution; route it
		// through the encoder-serialized path so those interleave cleanly with
		// the response.
		if req.Method == "tools/call" {
			if err := s.handleCallAndWrite(ctx, req); err != nil {
				return err
			}
			continue
		}
		resp := s.dispatch(ctx, req)
		if err := s.encode(resp); err != nil {
			return fmt.Errorf("mcp server: encode: %w", err)
		}
	}
}

// encode writes one frame under the encoder mutex.
func (s *Server) encode(v any) error {
	s.encMu.Lock()
	defer s.encMu.Unlock()
	return s.enc.Encode(v)
}

// handleCallAndWrite runs a tools/call — emitting any $/progress notifications
// its tool requests — then writes the response, all serialized on the encoder.
func (s *Server) handleCallAndWrite(ctx context.Context, req serverReq) error {
	resp := s.handleCall(ctx, req)
	if err := s.encode(resp); err != nil {
		return fmt.Errorf("mcp server: encode: %w", err)
	}
	return nil
}

func (s *Server) dispatch(ctx context.Context, req serverReq) serverResp {
	switch req.Method {
	case "initialize":
		return s.ok(req.ID, map[string]any{
			"protocolVersion": negotiateVersion(req.Params),
			"capabilities": map[string]any{
				"tools": map[string]any{},
				// streaming advertises id-less $/progress support so a client's
				// Supports("streaming") gate (D2) can pass against fuse's server.
				"streaming": map[string]any{},
				// resources advertises the fuse://tools resource surface with
				// subscribe support so a client's Supports("resources.subscribe")
				// gate (D5) passes against fuse's own server (dogfood).
				"resources": map[string]any{
					"subscribe":   true,
					"listChanged": false,
				},
			},
			"serverInfo": map[string]any{"name": "fuse", "version": "1.0.0"},
		})
	case "tools/list":
		return s.handleList(req.ID)
	case "tools/call":
		return s.handleCall(ctx, req)
	case "resources/list":
		return s.handleResourcesList(req.ID)
	case "resources/read":
		return s.handleResourcesRead(req)
	case "resources/subscribe":
		return s.handleResourcesSubscribe(req)
	case "resources/unsubscribe":
		return s.handleResourcesUnsubscribe(req)
	default:
		return s.errResp(req.ID, ErrMethodNotFound, "method not found: "+req.Method)
	}
}

// serverDefaultProtocolVersion is the MCP version fuse's server advertises when
// the client requests an unrecognized (or no) version.
const serverDefaultProtocolVersion = "2025-03-26"

// recognizedProtocolVersions are the versions fuse's server will echo back to a
// client (version negotiation, server side).
var recognizedProtocolVersions = map[string]bool{
	"2025-03-26": true,
	"2024-11-05": true,
}

// negotiateVersion echoes the client's requested protocolVersion when fuse
// recognizes it, else advertises the server default.
func negotiateVersion(params json.RawMessage) string {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if len(params) > 0 {
		_ = json.Unmarshal(params, &p)
	}
	if recognizedProtocolVersions[p.ProtocolVersion] {
		return p.ProtocolVersion
	}
	return serverDefaultProtocolVersion
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
	Meta      *callMeta       `json:"_meta"`
}

// callMeta carries the optional MCP request metadata. progressToken, when
// present, opts the call into $/progress streaming.
type callMeta struct {
	ProgressToken json.RawMessage `json:"progressToken"`
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
	// Only when the client requested progress (a non-empty _meta.progressToken)
	// do we install a reporter; otherwise a tool's ProgressFromContext is nil and
	// no $/progress frame is ever written (behavior byte-identical to today).
	if p.Meta != nil && len(p.Meta.ProgressToken) > 0 && string(p.Meta.ProgressToken) != "null" {
		ctx = withProgress(ctx, s.progressReporter(p.Meta.ProgressToken))
	}
	result := s.gate.Execute(ctx, p.Name, args)
	return s.ok(req.ID, map[string]any{
		"content": []map[string]any{{"type": "text", "text": result.Output}},
		"isError": result.IsError,
	})
}

// progressReporter returns a ProgressReporter that writes an id-less $/progress
// notification echoing the client's progressToken, serialized on the encoder so
// it never interleaves with the eventual response. An encode error is dropped —
// a broken transport surfaces when the response fails to write.
func (s *Server) progressReporter(token json.RawMessage) ProgressReporter {
	return func(progress float64, total *float64) {
		params := map[string]any{
			"progressToken": token,
			"progress":      progress,
		}
		if total != nil {
			params["total"] = *total
		}
		note := jsonrpcNotification{JSONRPC: "2.0", Method: "$/progress", Params: params}
		_ = s.encode(note)
	}
}

func (s *Server) ok(id json.RawMessage, v any) serverResp {
	raw, _ := json.Marshal(v)
	return serverResp{JSONRPC: "2.0", ID: id, Result: raw}
}

func (s *Server) errResp(id json.RawMessage, code int, msg string) serverResp {
	return serverResp{JSONRPC: "2.0", ID: id, Error: &jsonrpcError{Code: code, Message: msg}}
}
