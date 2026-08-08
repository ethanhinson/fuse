package mcp

import (
	"strconv"
	"sync"
)

// callTracking is the per-call state bound to a progress token while a streaming
// tools/call is in flight: which server/tool it belongs to. Task 6 adds the
// bounded ring buffer that assembles $/stream chunks.
type callTracking struct {
	server string
	tool   string

	mu sync.Mutex
}

// beginCall registers per-call tracking under a freshly minted progress token
// and returns the token plus an end func. The end func removes the tracking
// entry and returns the concatenated stream buffer (empty when no $/stream
// chunks arrived). It satisfies callTracker.
func (m *Manager) beginCall(server, tool string) (string, func() string) {
	token := "p" + strconv.FormatUint(m.tokenCounter.Add(1), 10)
	ct := &callTracking{server: server, tool: tool}

	m.trackMu.Lock()
	if m.tracking == nil {
		m.tracking = map[string]*callTracking{}
	}
	m.tracking[token] = ct
	m.trackMu.Unlock()

	return token, func() string {
		m.trackMu.Lock()
		delete(m.tracking, token)
		m.trackMu.Unlock()
		// Task 6 returns the concatenated $/stream buffer here.
		return ""
	}
}

// trackingFor returns the tracking entry for a token, or nil if absent (a late
// or unmatched notification after the call returned). Concurrency-safe.
func (m *Manager) trackingFor(token string) *callTracking {
	m.trackMu.Lock()
	defer m.trackMu.Unlock()
	return m.tracking[token]
}
