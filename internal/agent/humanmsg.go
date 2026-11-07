package agent

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethanhinson/fuse/internal/model"
)

// humanmsg.go is the unified substrate for human→agent messaging (ADR-0022).
// Every human utterance is a HumanMsg carried on a per-node queue owned by the
// HumanBus; a node self-pulls its queue at its turn boundary (HumanInjector.Poll),
// and a completion hook (HumanBus.OnNodeComplete) bubbles undelivered messages up
// the tree so none are silently stranded. Handles (@coder) are assigned by the
// HandleRegistry; /btw asides are answered by AnswerAside from live state with no
// model call. The LLM router is an injected seam (Router) so the loop and the bus
// stay model-agnostic and unit-testable.

// MsgMode is the delivery contract for one human message.
type MsgMode int

const (
	// ModeRespond answers an in-flight ask_user synchronously; it never enters a
	// queue (the ask overlay's RespCh is the delivery point).
	ModeRespond MsgMode = iota
	// ModeDirect targets one node by @handle; delivered at that node's turn boundary.
	ModeDirect
	// ModeBroadcast (@all) is delivered into every live node's queue.
	ModeBroadcast
	// ModeAside (/btw) is answered by the harness from state; NEVER delivered.
	ModeAside
	// ModeQueued is bare text default-routed to the selected/root node; the async
	// router may reclassify it to ModeDirect before delivery.
	ModeQueued
)

func (m MsgMode) String() string {
	switch m {
	case ModeRespond:
		return "respond"
	case ModeDirect:
		return "direct"
	case ModeBroadcast:
		return "broadcast"
	case ModeAside:
		return "aside"
	case ModeQueued:
		return "queued"
	default:
		return "unknown"
	}
}

// MsgStatus tracks a message's lifecycle so the TUI can render its fate — no
// human message is ever silently lost (ADR-0022).
type MsgStatus int

const (
	MsgPending       MsgStatus = iota // queued, not yet delivered
	MsgRouted                         // the async router has classified it
	MsgDelivered                      // drained at a turn boundary
	MsgStranded                       // target finished; bubbled to parent
	MsgUndeliverable                  // bubbled to root and root also finished
)

func (s MsgStatus) String() string {
	switch s {
	case MsgPending:
		return "pending"
	case MsgRouted:
		return "routed"
	case MsgDelivered:
		return "delivered"
	case MsgStranded:
		return "stranded"
	case MsgUndeliverable:
		return "undeliverable"
	default:
		return "unknown"
	}
}

// HumanMsg is the single value type carried on the bus. The queue entry is
// mutable (edit/reorder/delete) until delivered; a delivered copy is logged
// immutably. Ordering is by Seq (a global monotonic counter), never TS.
type HumanMsg struct {
	ID       string
	ToNodeID string // resolved at enqueue; "" for aside/broadcast fan-out source
	Mode     MsgMode
	Text     string
	Seq      uint64
	TS       time.Time
	Status   MsgStatus
	Handle   string // display-only; looked up fresh at render, survives rename
	// Source says who is speaking: SourceHuman.
	Source string
	// Interrupt marks a steer that must not wait for the in-flight model call or
	// tool batch to finish: the bus calls the node's interrupt hook on enqueue.
	Interrupt bool
}

// SourceHuman labels a message from the human. An empty Source reads the same.
const SourceHuman = "human"

// HumanBus owns the per-node human-message queues and the delivered log. It is a
// typed layer (its own mutex + Seq counter): the queue is a mutable staging buffer
// (ADR-0022). The TUI reads it directly through the snapshot methods.
type HumanBus struct {
	mu     sync.Mutex
	queues map[string][]HumanMsg // nodeID -> ordered pending messages
	log    []HumanMsg            // append-only delivered/terminal record
	seq    atomic.Uint64
	tree   *AgentTree // for parent lookup on completion bubbling; may be nil in tests

	// notify holds a per-node buffered (cap 1) signal channel used by an
	// INTERACTIVE loop to park at its turn boundary and wake when a message is
	// enqueued for it (WaitForMessage). It is orthogonal to Poll/Drain — a
	// non-interactive loop never calls WaitForMessage, so it is byte-identical to
	// the pre-existing poll-only bus. Lazily created per node on first use under mu.
	notify map[string]chan struct{}

	// interrupts holds each node's interrupt hook: the running loop registers a func
	// that cancels its in-flight model call / tool batch, and an Interrupt steer
	// fires it. Guarded by mu.
	interrupts map[string]func()
}

// SetInterrupt registers (or clears, with nil) the hook an Interrupt steer fires for
// a node. The agent loop sets it for the span of each turn.
func (b *HumanBus) SetInterrupt(nodeID string, fn func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.interrupts == nil {
		b.interrupts = map[string]func(){}
	}
	if fn == nil {
		delete(b.interrupts, nodeID)
		return
	}
	b.interrupts[nodeID] = fn
}

// NewHumanBus returns an empty bus bound to tree (nil-safe for unit tests).
func NewHumanBus(tree *AgentTree) *HumanBus {
	return &HumanBus{queues: map[string][]HumanMsg{}, tree: tree, notify: map[string]chan struct{}{}}
}

// notifyChanLocked returns (creating if absent) the cap-1 signal channel for a
// node. Caller must hold b.mu.
func (b *HumanBus) notifyChanLocked(nodeID string) chan struct{} {
	if b.notify == nil {
		b.notify = map[string]chan struct{}{}
	}
	ch, ok := b.notify[nodeID]
	if !ok {
		ch = make(chan struct{}, 1)
		b.notify[nodeID] = ch
	}
	return ch
}

// signalLocked wakes any WaitForMessage parked on this node's channel. The channel
// is cap-1 and coalescing: a pending signal is not duplicated (a parked waiter
// drains the whole queue on wake, so one wake covers N enqueues). Caller holds b.mu.
func (b *HumanBus) signalLocked(nodeID string) {
	ch := b.notifyChanLocked(nodeID)
	select {
	case ch <- struct{}{}:
	default: // already signaled and undrained — coalesce
	}
}

// hasPendingInterrupt reports whether an Interrupt steer is queued for the node.
func (b *HumanBus) hasPendingInterrupt(nodeID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, m := range b.queues[nodeID] {
		if m.Interrupt {
			return true
		}
	}
	return false
}

// nextSeq returns the next global monotonic sequence number.
func (b *HumanBus) nextSeq() uint64 { return b.seq.Add(1) }

// Enqueue appends a message to a node's queue with a fresh Seq and returns it.
// The caller supplies the resolved target node ID, mode, display handle, and text.
func (b *HumanBus) Enqueue(nodeID string, mode MsgMode, handle, text string) HumanMsg {
	return b.enqueue(nodeID, mode, handle, text, SourceHuman, false)
}

// Steer enqueues a direct steering message for a node. source labels who is speaking
// (SourceHuman). With interrupt set, the node's in-flight model
// call or tool batch is cancelled so the steer lands now instead of at the end of the
// current batch; without it the steer waits for the next turn boundary like any other
// message. Safe to call from any goroutine.
func (b *HumanBus) Steer(nodeID, source string, interrupt bool, text string) HumanMsg {
	handle := "@human"
	return b.enqueue(nodeID, ModeDirect, handle, text, source, interrupt)
}

func (b *HumanBus) enqueue(nodeID string, mode MsgMode, handle, text, source string, interrupt bool) HumanMsg {
	b.mu.Lock()
	msg := HumanMsg{
		ID:        fmt.Sprintf("hm-%d", b.seq.Add(1)),
		ToNodeID:  nodeID,
		Mode:      mode,
		Text:      text,
		Seq:       b.nextSeq(),
		TS:        time.Now(),
		Status:    MsgPending,
		Handle:    handle,
		Source:    source,
		Interrupt: interrupt,
	}
	b.queues[nodeID] = append(b.queues[nodeID], msg)
	// Wake an interactive loop parked in WaitForMessage on this node (no-op for a
	// non-interactive loop, which never parks). Signal happens under mu so it cannot
	// race the queue append a waiter will drain.
	b.signalLocked(nodeID)
	var hook func()
	if interrupt {
		hook = b.interrupts[nodeID]
	}
	b.mu.Unlock()
	// The hook only cancels a context, so it is cheap, but run it outside mu anyway.
	if hook != nil {
		hook()
	}
	return msg
}

// Broadcast enqueues an identical message into every live node's queue and
// returns the created messages. Each node drains its own copy independently.
func (b *HumanBus) Broadcast(liveNodeIDs []string, text string) []HumanMsg {
	out := make([]HumanMsg, 0, len(liveNodeIDs))
	for _, id := range liveNodeIDs {
		out = append(out, b.Enqueue(id, ModeBroadcast, "@all", text))
	}
	return out
}

// Pending returns a copy of a node's pending queue in Seq order.
func (b *HumanBus) Pending(nodeID string) []HumanMsg {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sortedLocked(nodeID)
}

// PendingAll returns a copy of every non-empty queue keyed by node ID (for the
// TUI queue editor to render across nodes).
func (b *HumanBus) PendingAll() map[string][]HumanMsg {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make(map[string][]HumanMsg, len(b.queues))
	for id, q := range b.queues {
		if len(q) == 0 {
			continue
		}
		cp := make([]HumanMsg, len(q))
		copy(cp, q)
		sort.Slice(cp, func(i, j int) bool { return cp[i].Seq < cp[j].Seq })
		out[id] = cp
	}
	return out
}

func (b *HumanBus) sortedLocked(nodeID string) []HumanMsg {
	q := b.queues[nodeID]
	cp := make([]HumanMsg, len(q))
	copy(cp, q)
	sort.Slice(cp, func(i, j int) bool { return cp[i].Seq < cp[j].Seq })
	return cp
}

// Edit replaces the text of a queued message (by ID) if still pending. Returns
// false if the message is not found (already delivered or never existed).
func (b *HumanBus) Edit(nodeID, msgID, newText string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i := range b.queues[nodeID] {
		if b.queues[nodeID][i].ID == msgID {
			b.queues[nodeID][i].Text = newText
			return true
		}
	}
	return false
}

// Delete removes a queued message by ID. Idempotent.
func (b *HumanBus) Delete(nodeID, msgID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	q := b.queues[nodeID]
	for i := range q {
		if q[i].ID == msgID {
			b.queues[nodeID] = append(q[:i], q[i+1:]...)
			return true
		}
	}
	return false
}

// Move re-sequences a queued message to a new position among its siblings
// (0-based index) by rewriting Seq values so the intended order is total and
// collision-free. Returns false if the message is not found.
func (b *HumanBus) Move(nodeID, msgID string, toIndex int) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	cur := b.sortedLocked(nodeID)
	idx := -1
	for i := range cur {
		if cur[i].ID == msgID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return false
	}
	msg := cur[idx]
	cur = append(cur[:idx], cur[idx+1:]...)
	if toIndex < 0 {
		toIndex = 0
	}
	if toIndex > len(cur) {
		toIndex = len(cur)
	}
	cur = append(cur[:toIndex], append([]HumanMsg{msg}, cur[toIndex:]...)...)
	// Reassign fresh monotonic Seqs preserving the new order.
	for i := range cur {
		cur[i].Seq = b.nextSeq()
	}
	b.queues[nodeID] = cur
	return true
}

// MoveToNode relocates a pending message (by ID) from one node's queue to
// another's, re-sequencing it to the end of the destination. Used by the async
// router to apply its verdict. Returns false if the message isn't pending on
// fromNode.
func (b *HumanBus) MoveToNode(fromNode, msgID, toNode string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	q := b.queues[fromNode]
	for i := range q {
		if q[i].ID == msgID {
			msg := q[i]
			b.queues[fromNode] = append(q[:i], q[i+1:]...)
			msg.ToNodeID = toNode
			msg.Seq = b.nextSeq()
			msg.Status = MsgRouted
			b.queues[toNode] = append(b.queues[toNode], msg)
			return true
		}
	}
	return false
}

// Drain removes and returns a node's pending queue in Seq order, appending each
// message to the delivered log with MsgDelivered. Called by HumanInjector.Poll
// (a self-pull by the node's own goroutine at its turn boundary).
func (b *HumanBus) Drain(nodeID string) []HumanMsg {
	b.mu.Lock()
	defer b.mu.Unlock()
	drained := b.sortedLocked(nodeID)
	if len(drained) == 0 {
		return nil
	}
	delete(b.queues, nodeID)
	for i := range drained {
		drained[i].Status = MsgDelivered
		b.log = append(b.log, drained[i])
	}
	return drained
}

// LogRespond records a synchronous ask_user answer for the transcript. Respond
// messages never enter a queue; this is their only bus footprint.
func (b *HumanBus) LogRespond(nodeID, text string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.log = append(b.log, HumanMsg{
		ID:       fmt.Sprintf("hm-%d", b.seq.Add(1)),
		ToNodeID: nodeID,
		Mode:     ModeRespond,
		Text:     text,
		Seq:      b.nextSeq(),
		TS:       time.Now(),
		Status:   MsgDelivered,
	})
}

// Log returns a copy of the delivered/terminal message log (for /questions-style
// recall and the TUI's delivery-confirmation display).
func (b *HumanBus) Log() []HumanMsg {
	b.mu.Lock()
	defer b.mu.Unlock()
	cp := make([]HumanMsg, len(b.log))
	copy(cp, b.log)
	return cp
}

// OnNodeComplete is the completion hook (ADR-0022): when a node finishes with a
// non-empty queue, its undelivered messages are bubbled to the parent (marked
// Stranded), so a node whose final turn emits no tool call — and thus never hits
// another turn boundary — cannot silently strand a late message. At the root
// (always the human's live partner) any residue is marked Undeliverable and kept
// for TUI visibility. Returns the bubbled/undeliverable messages for surfacing.
func (b *HumanBus) OnNodeComplete(nodeID string) []HumanMsg {
	b.mu.Lock()
	defer b.mu.Unlock()
	pending := b.sortedLocked(nodeID)
	if len(pending) == 0 {
		return nil
	}
	delete(b.queues, nodeID)

	parentID := ""
	rootID := ""
	if b.tree != nil {
		if n := b.tree.Node(nodeID); n != nil {
			parentID = n.ParentID
		}
		rootID = b.tree.RootID()
	}

	if parentID == "" || nodeID == rootID {
		// Root (or an orphan): surface as undeliverable, keep for the TUI.
		for i := range pending {
			pending[i].Status = MsgUndeliverable
			b.log = append(b.log, pending[i])
		}
		return pending
	}

	// Bubble to the parent: re-home and re-sequence so parent order is total.
	for i := range pending {
		pending[i].Status = MsgStranded
		pending[i].ToNodeID = parentID
		pending[i].Seq = b.nextSeq()
	}
	b.queues[parentID] = append(b.queues[parentID], pending...)
	return pending
}

// HumanInjector is the per-node delivery point wired into Agent.Run. Bound to a
// node's ID and the shared bus; its Poll is a self-pull at the turn boundary.
type HumanInjector struct {
	nodeID string
	bus    *HumanBus
}

// NewHumanInjector binds an injector to a node. A nil bus yields a no-op injector
// (Poll always returns nothing), so agents built without human messaging are
// unaffected.
func NewHumanInjector(nodeID string, bus *HumanBus) *HumanInjector {
	return &HumanInjector{nodeID: nodeID, bus: bus}
}

// Poll drains this node's queue and returns a SINGLE batched user message (queued
// segments joined by a separator) — not N consecutive user messages, which some
// models handle poorly. Returns (nil, false) when nothing is pending. It is safe
// to call every turn; a nil bus is a no-op.
func (h *HumanInjector) Poll() (model.Message, bool) {
	if h == nil || h.bus == nil {
		return model.Message{}, false
	}
	drained := h.bus.Drain(h.nodeID)
	if len(drained) == 0 {
		return model.Message{}, false
	}
	var b strings.Builder
	b.WriteString("[human message]\n")
	for i, msg := range drained {
		if i > 0 {
			b.WriteString("\n---\n")
		}
		b.WriteString(msg.Text)
	}
	return model.Message{Role: "user", Content: b.String()}, true
}

// setInterrupt registers fn as the hook an Interrupt steer fires for this node
// (nil clears it). It also reports whether an interrupt steer is already queued, so
// a caller that registers just after the message arrived still sees it.
func (h *HumanInjector) setInterrupt(fn func()) (pending bool) {
	if h == nil || h.bus == nil {
		return false
	}
	h.bus.SetInterrupt(h.nodeID, fn)
	if fn == nil {
		return false
	}
	return h.bus.hasPendingInterrupt(h.nodeID)
}
