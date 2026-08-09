package mcp

import (
	"encoding/json"
	"log"
)

// notifyQueueSize bounds the pending-notification queue. Notifications are small
// and handlers are expected to be quick; this depth absorbs bursts without
// letting a stalled handler grow memory unbounded. On overflow the newest
// notification is DROPPED and logged, rather than blocking the read-pump (the
// whole point of moving handlers off it).
const notifyQueueSize = 256

// queuedNotification is one decoded inbound notification awaiting dispatch on
// the worker goroutine. A non-nil barrier makes it a synchronization sentinel
// (see waitNotifyDrained): the worker closes barrier instead of dispatching.
type queuedNotification struct {
	server  string
	method  string
	params  json.RawMessage
	barrier chan struct{}
}

// NotificationHandler receives a decoded inbound JSON-RPC notification: the
// name of the server that sent it and the raw params. Handlers run on the
// Manager's single notification-dispatch worker goroutine (no longer on the
// client read-pump), so a slow handler no longer blocks response dispatch. A
// handler should still avoid unbounded blocking, since one worker serves all
// servers and a stuck handler stalls the queue behind it.
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
// client read pump) to the dispatch worker via a bounded, NON-BLOCKING enqueue.
// It satisfies notificationRouter. The read-pump must never block here — if the
// queue is full (a stuck/slow handler), the notification is dropped and logged
// rather than wedging response dispatch on the connection.
func (m *Manager) dispatchNotification(server, method string, params json.RawMessage) {
	m.ensureNotifyWorker()
	// Copy params: a read-pump may reuse its decode buffers, and the frame is
	// handled later on the worker, so the slice must be owned by the queue entry.
	var p json.RawMessage
	if len(params) > 0 {
		p = append(json.RawMessage(nil), params...)
	}
	select {
	case m.notifyQueue <- queuedNotification{server: server, method: method, params: p}:
	default:
		log.Printf("[mcp] %s: notification queue full, dropping %q", server, method)
	}
}

// ensureNotifyWorker lazily creates the queue and starts the single dispatch
// worker on first use. Idempotent via sync.Once.
func (m *Manager) ensureNotifyWorker() {
	m.notifyStart.Do(func() {
		m.notifyQueue = make(chan queuedNotification, notifyQueueSize)
		m.notifyDone = make(chan struct{})
		m.notifyWG.Add(1)
		go m.notifyWorker()
	})
}

// notifyWorker drains the queue and invokes handlers sequentially, preserving
// enqueue order. It exits when notifyDone is closed AND the queue is drained, so
// notifications enqueued just before Close are not lost.
func (m *Manager) notifyWorker() {
	defer m.notifyWG.Done()
	for {
		select {
		case n := <-m.notifyQueue:
			m.invokeHandler(n)
		case <-m.notifyDone:
			// Drain whatever is still queued, then stop.
			for {
				select {
				case n := <-m.notifyQueue:
					m.invokeHandler(n)
				default:
					return
				}
			}
		}
	}
}

// invokeHandler looks up and calls the registered handler for one notification,
// or completes a barrier sentinel. An unregistered method is a silent no-op —
// the router is feature-generic and tolerates methods no subsystem subscribed to.
func (m *Manager) invokeHandler(n queuedNotification) {
	if n.barrier != nil {
		close(n.barrier) // synchronization sentinel — everything before it is done
		return
	}
	m.notifyMu.Lock()
	h := m.notifyHandlers[n.method]
	m.notifyMu.Unlock()
	if h == nil {
		return
	}
	h(n.server, n.params)
}

// drainNotifications satisfies callTracker: it blocks until every notification
// enqueued so far has been dispatched, so a streaming Execute can safely
// assemble its stream buffer after tools/call returns. Delegates to
// waitNotifyDrained.
func (m *Manager) drainNotifications() { m.waitNotifyDrained() }

// waitNotifyDrained blocks until every notification enqueued before this call
// has been dispatched. It pushes a barrier through the same FIFO queue and waits
// for the worker to reach it — so it observes strictly-earlier notifications as
// complete. Used by tests (and any caller needing a happens-before point after a
// batch of notifications) to assert handler effects deterministically without a
// sleep. No-op if the worker was never started.
func (m *Manager) waitNotifyDrained() {
	if m.notifyQueue == nil {
		return
	}
	done := make(chan struct{})
	select {
	case m.notifyQueue <- queuedNotification{barrier: done}:
		<-done
	case <-m.notifyDone:
		// Worker stopping/stopped; nothing more will be dispatched.
	}
}

// stopNotifyWorker signals the worker to drain-and-exit and waits for it. Safe to
// call when the worker was never started (Close on an idle Manager).
func (m *Manager) stopNotifyWorker() {
	if m.notifyDone == nil {
		return // worker never started
	}
	close(m.notifyDone)
	m.notifyWG.Wait()
}
