package mcp

import (
	"encoding/json"
)

// ToolsResourceURI is the URI of fuse's dogfood resource: the live tool catalog
// derived from the server's registry (D5). A client can resources/read it to
// snapshot the exposed tools and resources/subscribe to be pushed a
// notifications/resources/updated whenever the catalog changes (config reload).
const ToolsResourceURI = "fuse://tools"

// PushResourceUpdated writes an id-less notifications/resources/updated frame for
// a URI to this connection when it is subscribed (a no-op otherwise). Exported so
// the `fuse mcp-server` config-watch (cmd/fuse) can push on a tool-catalog change.
func (s *Server) PushResourceUpdated(uri string) { s.pushResourceUpdated(uri) }

// handleResourcesList returns the server's exposed resources — currently only the
// fuse://tools catalog.
func (s *Server) handleResourcesList(id json.RawMessage) serverResp {
	return s.ok(id, map[string]any{
		"resources": []map[string]any{{
			"uri":         ToolsResourceURI,
			"name":        "Fuse tool catalog",
			"description": "The live catalog of tools this fuse server exposes; pushes an update when the catalog changes.",
			"mimeType":    "application/json",
		}},
	})
}

// resourceReadParams is the wire shape of a resources/read request's params.
type resourceReadParams struct {
	URI string `json:"uri"`
}

// handleResourcesRead returns the contents of a resource. Only fuse://tools is
// exposed; any other URI is an MCP resource-not-found error.
func (s *Server) handleResourcesRead(req serverReq) serverResp {
	var p resourceReadParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return s.errResp(req.ID, ErrInvalidParams, "invalid params: "+err.Error())
	}
	if p.URI != ToolsResourceURI {
		return s.errResp(req.ID, ErrResourceNotFound, "resource not found: "+p.URI)
	}
	catalog := s.toolCatalogJSON()
	return s.ok(req.ID, map[string]any{
		"contents": []map[string]any{{
			"uri":      ToolsResourceURI,
			"mimeType": "application/json",
			"text":     catalog,
		}},
	})
}

// handleResourcesSubscribe records a URI on this connection's subscription set so
// a later pushResourceUpdated delivers a notification to it. Subscribing to an
// unknown URI is a resource-not-found error (an explicit attempt against a
// resource fuse does not expose).
func (s *Server) handleResourcesSubscribe(req serverReq) serverResp {
	var p resourceReadParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return s.errResp(req.ID, ErrInvalidParams, "invalid params: "+err.Error())
	}
	if p.URI != ToolsResourceURI {
		return s.errResp(req.ID, ErrResourceNotFound, "resource not found: "+p.URI)
	}
	s.subMu.Lock()
	if s.subscribed == nil {
		s.subscribed = map[string]bool{}
	}
	s.subscribed[p.URI] = true
	s.subMu.Unlock()
	return s.ok(req.ID, map[string]any{})
}

// handleResourcesUnsubscribe removes a URI from this connection's subscription
// set. Unsubscribing a URI that was never subscribed is a no-op success.
func (s *Server) handleResourcesUnsubscribe(req serverReq) serverResp {
	var p resourceReadParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return s.errResp(req.ID, ErrInvalidParams, "invalid params: "+err.Error())
	}
	s.subMu.Lock()
	delete(s.subscribed, p.URI)
	s.subMu.Unlock()
	return s.ok(req.ID, map[string]any{})
}

// pushResourceUpdated writes an id-less notifications/resources/updated frame for
// a URI, but only when this connection is subscribed to it. The frame is
// serialized on the encoder (encMu) so it never interleaves with a concurrent
// response. An unsubscribed URI is a no-op — nothing is written.
func (s *Server) pushResourceUpdated(uri string) {
	s.subMu.Lock()
	subscribed := s.subscribed[uri]
	s.subMu.Unlock()
	if !subscribed {
		return
	}
	note := jsonrpcNotification{
		JSONRPC: "2.0",
		Method:  resourceUpdatedMethod,
		Params:  map[string]any{"uri": uri},
	}
	_ = s.encode(note)
}

// toolCatalogJSON renders the current registry as the fuse://tools resource body:
// a JSON object with a "tools" array of {name, description}. Deterministic order
// (the registry's registration order) so an unchanged registry produces byte-
// identical output — the config-watch push (Task 6) diffs on this.
func (s *Server) toolCatalogJSON() string {
	schemas := s.reg.Schemas()
	tools := make([]map[string]any, 0, len(schemas))
	for _, sc := range schemas {
		tools = append(tools, map[string]any{
			"name":        sc.Name,
			"description": sc.Description,
		})
	}
	b, err := json.Marshal(map[string]any{"tools": tools})
	if err != nil {
		return "{}"
	}
	return string(b)
}
