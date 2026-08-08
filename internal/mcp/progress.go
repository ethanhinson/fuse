package mcp

import (
	"encoding/json"
)

// ProgressEvent is a single MCP $/progress notification correlated back to the
// tool call that minted its token. Total is nil for indeterminate progress
// (no total advertised) and set for determinate progress. The TUI layer
// subscribes via OnProgress and correlates the event to the running tool node
// by (Server, Tool) — no node handle flows through MCPTool.Execute.
type ProgressEvent struct {
	Server   string
	Tool     string
	Progress float64
	Total    *float64
}

// ProgressObserver receives ProgressEvents. Observers run on the sending
// client's read-pump goroutine, so they must be non-blocking.
type ProgressObserver func(ProgressEvent)

// progressNotifyMethod is the JSON-RPC method for MCP progress notifications.
const progressNotifyMethod = "$/progress"

// progressParams is the wire shape of a $/progress notification's params.
type progressParams struct {
	ProgressToken string   `json:"progressToken"`
	Progress      float64  `json:"progress"`
	Total         *float64 `json:"total"`
}

// OnProgress registers an observer for correlated progress events. Multiple
// observers may subscribe; each is called for every event. Concurrency-safe
// with dispatch.
func (m *Manager) OnProgress(obs ProgressObserver) {
	m.progressMu.Lock()
	m.progressObservers = append(m.progressObservers, obs)
	m.progressMu.Unlock()
}

// handleProgress is the router handler for "$/progress". It decodes the params,
// correlates the token to an in-flight call, and fans a ProgressEvent to all
// observers. An unmatched, late, or malformed notification is silently dropped
// (fail-open) — it never panics and never mutates a cleared tracking entry.
func (m *Manager) handleProgress(server string, params json.RawMessage) {
	var p progressParams
	if err := json.Unmarshal(params, &p); err != nil || p.ProgressToken == "" {
		return
	}
	ct := m.trackingFor(p.ProgressToken)
	if ct == nil {
		return // unmatched or late — the call already ended
	}
	evt := ProgressEvent{
		Server:   ct.server,
		Tool:     ct.tool,
		Progress: p.Progress,
		Total:    p.Total,
	}
	m.progressMu.Lock()
	obs := make([]ProgressObserver, len(m.progressObservers))
	copy(obs, m.progressObservers)
	m.progressMu.Unlock()
	for _, o := range obs {
		o(evt)
	}
}
