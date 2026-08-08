package mcp

import (
	"context"
	"fmt"
	"sort"
)

// Subscribe subscribes to resource-updated pushes for a URI on a server. It is
// ref-counted: the first Subscribe of a URI sends the wire resources/subscribe;
// subsequent Subscribes of the same URI only bump the ref count. The call is
// capability-gated (D4): a server that does not advertise resources.subscribe
// returns an error on an explicit Subscribe (list/read still work — fail-open),
// and no ref count is recorded.
//
// STAGED LIBRARY SURFACE (change 0021): the ref-counted tracker and the wire
// resources/subscribe / resources/unsubscribe methods here are complete and
// tested, but this Subscribe (and Unsubscribe/ListResources/ReadResource) is not
// yet wired to a user-facing affordance against an EXTERNAL server. Change 0021
// is scoped to the dogfood loop — a fuse client subscribing to fuse's OWN
// mcp-server, which the internal/tui e2e exercises end-to-end. A user-facing
// subscribe affordance against arbitrary external servers is deferred follow-up.
// Do not read this as a live guarantee for external servers yet.
func (m *Manager) Subscribe(ctx context.Context, server, uri string) error {
	m.mu.Lock()
	ms, ok := m.servers[server]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("mcp: unknown server %q", server)
	}
	if !ms.caps.Supports("resources.subscribe") {
		return fmt.Errorf("mcp: server %q does not support resources.subscribe", server)
	}
	if ms.conn == nil {
		return fmt.Errorf("mcp: server %q is not connected", server)
	}

	first := m.incRef(server, uri)
	if !first {
		return nil // already subscribed at the wire level
	}
	if _, err := ms.conn.call(ctx, "resources/subscribe", map[string]any{"uri": uri}); err != nil {
		// Roll the ref count back so a failed wire subscribe doesn't leave a
		// phantom subscription that a later Unsubscribe would try to release.
		m.decRef(server, uri)
		return err
	}
	return nil
}

// Unsubscribe releases one subscription reference for a URI. The wire
// resources/unsubscribe is sent only on the final release (ref count 1→0). A URI
// that was never subscribed, or an unknown server, is a no-op returning nil.
func (m *Manager) Unsubscribe(ctx context.Context, server, uri string) error {
	last := m.decRef(server, uri)
	if !last {
		return nil // still referenced, or was never subscribed
	}
	conn, err := m.connFor(server)
	if err != nil {
		return nil // server gone — nothing to release on the wire
	}
	if _, err := conn.call(ctx, "resources/unsubscribe", map[string]any{"uri": uri}); err != nil {
		return err
	}
	return nil
}

// resubscribeAll re-sends resources/subscribe for every tracked URI on a server,
// used after a reconnect replaces the connection. Wire failures are aggregated
// into the returned error but every URI is attempted (a broken re-subscribe of
// one URI must not skip the others).
//
// STAGED LIBRARY SURFACE (change 0021): this is the tracker-side building block
// for reconnect-resubscribe and is fully tested (TestResubscribeOnReconnect), but
// it is NOT yet invoked from a live reconnect path — no production reconnect loop
// calls it today. Wiring resubscribeAll into an actual reconnect is deferred
// follow-up (0021 is scoped to the dogfood loop via fuse's own mcp-server, proven
// by the internal/tui e2e). Do not read the presence of this method as a live
// reconnect-resubscribe guarantee.
func (m *Manager) resubscribeAll(ctx context.Context, server string) error {
	conn, err := m.connFor(server)
	if err != nil {
		return err
	}
	uris := m.trackedURIs(server)
	var firstErr error
	for _, uri := range uris {
		if _, err := conn.call(ctx, "resources/subscribe", map[string]any{"uri": uri}); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// incRef bumps the ref count for a server/uri and reports whether this was the
// 0→1 transition (the first subscription, which must send the wire subscribe).
func (m *Manager) incRef(server, uri string) (first bool) {
	m.subMu.Lock()
	defer m.subMu.Unlock()
	if m.subRefs == nil {
		m.subRefs = map[string]map[string]int{}
	}
	byURI := m.subRefs[server]
	if byURI == nil {
		byURI = map[string]int{}
		m.subRefs[server] = byURI
	}
	byURI[uri]++
	return byURI[uri] == 1
}

// decRef drops the ref count for a server/uri and reports whether this was the
// 1→0 transition (the final release, which must send the wire unsubscribe). A URI
// that was never subscribed is a no-op returning false.
func (m *Manager) decRef(server, uri string) (last bool) {
	m.subMu.Lock()
	defer m.subMu.Unlock()
	byURI := m.subRefs[server]
	if byURI == nil || byURI[uri] == 0 {
		return false
	}
	byURI[uri]--
	if byURI[uri] == 0 {
		delete(byURI, uri)
		return true
	}
	return false
}

// trackedURIs returns the currently subscribed URIs for a server, sorted for a
// deterministic re-subscribe order.
func (m *Manager) trackedURIs(server string) []string {
	m.subMu.Lock()
	defer m.subMu.Unlock()
	byURI := m.subRefs[server]
	out := make([]string, 0, len(byURI))
	for uri := range byURI {
		out = append(out, uri)
	}
	sort.Strings(out)
	return out
}
