package mcp

import (
	"encoding/json"
)

// NotificationHandler receives a decoded inbound JSON-RPC notification: the
// name of the server that sent it and the raw params. Handlers run on the
// sending client's read-pump goroutine, so they must be non-blocking and
// concurrency-safe.
type NotificationHandler func(server string, params json.RawMessage)

// OnNotification registers a handler for an inbound notification method (e.g.
// "$/progress"). A method may have at most one handler; re-registering
// overwrites the previous one. Registration is concurrency-safe with dispatch.
func (m *Manager) OnNotification(method string, h NotificationHandler) {
	m.notifyMu.Lock()
	defer m.notifyMu.Unlock()
	if m.notifyHandlers == nil {
		m.notifyHandlers = map[string]NotificationHandler{}
	}
	m.notifyHandlers[method] = h
}

// dispatchNotification routes an inbound id-less notification frame (from a
// client read pump) to the handler registered for its method. An unregistered
// method is a silent no-op — the router is feature-generic and tolerates
// methods no subsystem has subscribed to. It satisfies notificationRouter.
func (m *Manager) dispatchNotification(server, method string, params json.RawMessage) {
	m.notifyMu.Lock()
	h := m.notifyHandlers[method]
	m.notifyMu.Unlock()
	if h == nil {
		return
	}
	h(server, params)
}
