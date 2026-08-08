package mcp

import (
	"context"
	"encoding/json"
	"fmt"
)

// Resource is one entry from a server's resources/list — the client-facing
// projection of an MCP resource descriptor.
type Resource struct {
	URI      string
	Name     string
	MIMEType string
}

// ResourceContents is the decoded result of a resources/read: an ordered list of
// typed content blocks (D-constraint: render/carry ALL block types, never only
// text). A resource read may return multiple blocks (e.g. an embedded directory).
type ResourceContents struct {
	Contents []ResourceBlock
}

// ResourceBlock is a single content block from a resources/read result. The MCP
// spec's resource contents carry text OR base64 blob, plus uri/mimeType; this is
// the union so no field is silently dropped.
type ResourceBlock struct {
	URI      string `json:"uri"`
	MIMEType string `json:"mimeType"`
	Text     string `json:"text"`
	Blob     string `json:"blob"` // base64
}

// resourceDef is the wire shape of a single resources/list entry.
type resourceDef struct {
	URI      string `json:"uri"`
	Name     string `json:"name"`
	MIMEType string `json:"mimeType"`
}

// ResourceUpdatedEvent is a notifications/resources/updated correlated to the
// server that pushed it. The TUI subscribes via OnResource to surface a
// stale/updated indicator (D5).
type ResourceUpdatedEvent struct {
	Server string
	URI    string
}

// ResourceObserver receives ResourceUpdatedEvents. Observers run on the sending
// client's read-pump goroutine, so they must be non-blocking.
type ResourceObserver func(ResourceUpdatedEvent)

// resourceUpdatedMethod is the JSON-RPC method for MCP resource-updated pushes.
const resourceUpdatedMethod = "notifications/resources/updated"

// OnResource registers an observer for resource-updated events. Multiple
// observers may subscribe; each is called for every event. Concurrency-safe.
func (m *Manager) OnResource(obs ResourceObserver) {
	m.resourceMu.Lock()
	m.resourceObservers = append(m.resourceObservers, obs)
	m.resourceMu.Unlock()
}

// markStale records a URI as stale for a server (a pushed update arrived; the
// cached view is out of date). Concurrency-safe.
func (m *Manager) markStale(server, uri string) {
	m.resourceMu.Lock()
	defer m.resourceMu.Unlock()
	if m.staleURIs == nil {
		m.staleURIs = map[string]map[string]bool{}
	}
	byURI := m.staleURIs[server]
	if byURI == nil {
		byURI = map[string]bool{}
		m.staleURIs[server] = byURI
	}
	byURI[uri] = true
}

// clearStale removes a URI's stale flag (an explicit read fetched fresh).
func (m *Manager) clearStale(server, uri string) {
	m.resourceMu.Lock()
	defer m.resourceMu.Unlock()
	if byURI := m.staleURIs[server]; byURI != nil {
		delete(byURI, uri)
	}
}

// IsStale reports whether a URI on a server is currently flagged stale (a push
// arrived and no read has refreshed it since). Concurrency-safe.
func (m *Manager) IsStale(server, uri string) bool {
	m.resourceMu.Lock()
	defer m.resourceMu.Unlock()
	return m.staleURIs[server][uri]
}

// connFor returns the live connection for a managed server, or an error when the
// server is unknown or not connected.
func (m *Manager) connFor(server string) (mcpConn, error) {
	m.mu.Lock()
	ms, ok := m.servers[server]
	m.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("mcp: unknown server %q", server)
	}
	if ms.conn == nil {
		return nil, fmt.Errorf("mcp: server %q is not connected", server)
	}
	return ms.conn, nil
}

// ListResources calls resources/list on a server and returns its resources. A
// server that advertises no resources (empty list) returns an empty slice and no
// error — capability mismatches and empty sets fail open (D4).
func (m *Manager) ListResources(ctx context.Context, server string) ([]Resource, error) {
	conn, err := m.connFor(server)
	if err != nil {
		return nil, err
	}
	raw, err := conn.call(ctx, "resources/list", nil)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Resources []resourceDef `json:"resources"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("mcp: parse resources/list: %w", err)
	}
	out := make([]Resource, 0, len(resp.Resources))
	for _, r := range resp.Resources {
		out = append(out, Resource{URI: r.URI, Name: r.Name, MIMEType: r.MIMEType})
	}
	return out, nil
}

// ReadResource calls resources/read for a URI and returns its content blocks.
// Reading clears any stale flag the URI carried (D2: the next explicit read
// fetches fresh, and the client treats the URI as current again).
func (m *Manager) ReadResource(ctx context.Context, server, uri string) (ResourceContents, error) {
	conn, err := m.connFor(server)
	if err != nil {
		return ResourceContents{}, err
	}
	raw, err := conn.call(ctx, "resources/read", map[string]any{"uri": uri})
	if err != nil {
		return ResourceContents{}, err
	}
	var resp struct {
		Contents []ResourceBlock `json:"contents"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return ResourceContents{}, fmt.Errorf("mcp: parse resources/read: %w", err)
	}
	m.clearStale(server, uri)
	return ResourceContents{Contents: resp.Contents}, nil
}
