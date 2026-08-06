package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

// MaxDepth is the maximum allowed spawn depth.
const MaxDepth = 5

// MaxConcurrentSpawns is the DEFAULT width cap on concurrently RUNNING local
// child agents across the whole tree, used when config does not set
// agents.max_concurrent. Depth alone doesn't bound load: a model told to
// "spawn aggressively" can emit dozens of spawns per level. Queued children
// stay visible as pending nodes until a slot frees.
const MaxConcurrentSpawns = 16

// NodeStatus represents the lifecycle state of an agent node.
type NodeStatus int

const (
	StatusPending NodeStatus = iota
	StatusRunning
	StatusDone
	StatusError
	StatusCancelled
)

func (s NodeStatus) String() string {
	switch s {
	case StatusPending:
		return "pending"
	case StatusRunning:
		return "running"
	case StatusDone:
		return "done"
	case StatusError:
		return "error"
	case StatusCancelled:
		return "cancelled"
	default:
		return "unknown"
	}
}

// EventKind classifies an AgentEvent.
type EventKind int

const (
	KindAssistant EventKind = iota
	KindToolCall
	KindToolResult
	KindTokens
	KindDone
	KindError
)

func (k EventKind) String() string {
	switch k {
	case KindAssistant:
		return "assistant"
	case KindToolCall:
		return "tool_call"
	case KindToolResult:
		return "tool_result"
	case KindTokens:
		return "tokens"
	case KindDone:
		return "done"
	case KindError:
		return "error"
	default:
		return "unknown"
	}
}

// AgentEvent is a single event emitted by an agent node.
type AgentEvent struct {
	Kind    EventKind
	Name    string
	Payload map[string]any
	TS      time.Time
}

// AgentNode represents one agent in the spawn tree.
type AgentNode struct {
	ID        string
	ParentID  string
	Label     string
	Model     string
	Status    NodeStatus
	Depth     int
	StartedAt time.Time
	EndedAt   time.Time
	TokensIn  int
	TokensOut int
	Events    []AgentEvent
	children  []string
	cancel    func() // set by Spawner; called by CancelNode
	yields    int    // concurrent spawn_agent waits currently yielding this node's slot
	mu        sync.Mutex
}

// AddEvent appends an event to the node, thread-safe.
func (n *AgentNode) AddEvent(e AgentEvent) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.Events = append(n.Events, e)
}

// CopyEvents returns a thread-safe snapshot of the node's event list.
func (n *AgentNode) CopyEvents() []AgentEvent {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]AgentEvent, len(n.Events))
	copy(out, n.Events)
	return out
}

// UpdateTokens atomically increments the node's token counters.
func (n *AgentNode) UpdateTokens(in, out int) {
	n.mu.Lock()
	n.TokensIn += in
	n.TokensOut += out
	n.mu.Unlock()
}

// SetCancel stores the context cancel func so CancelNode can stop this node.
func (n *AgentNode) SetCancel(f func()) {
	n.mu.Lock()
	n.cancel = f
	n.mu.Unlock()
}

// Finish transitions the node to a terminal state.
func (n *AgentNode) Finish(status NodeStatus, errMsg string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.Status = status
	n.EndedAt = time.Now()
	if errMsg != "" {
		n.Events = append(n.Events, AgentEvent{
			Kind:    KindError,
			Name:    "finish",
			Payload: map[string]any{"error": errMsg},
			TS:      time.Now(),
		})
	}
}

// NodeView is an immutable point-in-time snapshot of an AgentNode's mutable
// state, taken under the node's lock. UI code must consume NodeView (or
// CopyEvents) rather than reading AgentNode fields directly: Status, the
// timestamps, and the token counters are written concurrently by agent
// goroutines, and the Events slice header races with AddEvent appends.
// Events are deliberately NOT included — they can be large and most consumers
// only need counters; call CopyEvents when the log itself is needed.
type NodeView struct {
	ID        string
	ParentID  string
	Label     string
	Model     string
	Status    NodeStatus
	Depth     int
	StartedAt time.Time
	EndedAt   time.Time
	TokensIn  int
	TokensOut int
}

// Snapshot returns a consistent view of the node's mutable state.
func (n *AgentNode) Snapshot() NodeView {
	n.mu.Lock()
	defer n.mu.Unlock()
	return NodeView{
		ID:        n.ID,
		ParentID:  n.ParentID,
		Label:     n.Label,
		Model:     n.Model,
		Status:    n.Status,
		Depth:     n.Depth,
		StartedAt: n.StartedAt,
		EndedAt:   n.EndedAt,
		TokensIn:  n.TokensIn,
		TokensOut: n.TokensOut,
	}
}

// TreeUpdate signals a node state change for TUI subscribers.
type TreeUpdate struct {
	NodeID string
}

// AgentTree is the shared state for the spawn tree.
type AgentTree struct {
	mu        sync.RWMutex
	nodes     map[string]*AgentNode
	rootID    string
	out       chan TreeUpdate // buffered 256
	dirty     map[string]bool
	spawnSem  chan struct{} // width cap for concurrently running local children
	maxSpawns int           // tree-global spawn budget (total children ever); 0 = unset

}

// SetMaxSpawns sets the tree-global spawn budget — the total number of child
// agents any spawn may create over the whole tree's life. 0 leaves it unset
// (no budget enforced). Set once at construction time, before any spawn.
func (t *AgentTree) SetMaxSpawns(n int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.maxSpawns = n
}

// SpawnBudget reports how much of the tree-global spawn budget is used and its
// ceiling. `used` is the number of child agents created so far (every node
// except the root — the tree is append-only, so this only grows); `max` is the
// configured ceiling, 0 when unset. This is the count the runtime injects into
// each spawn_agent result so the model never has to tally its own spawns.
func (t *AgentTree) SpawnBudget() (used, max int) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	used = len(t.nodes) - 1 // exclude the root node
	if used < 0 {
		used = 0
	}
	return used, t.maxSpawns
}

// NewAgentTree creates a new tree with the given root node label and model,
// using the default concurrency cap (MaxConcurrentSpawns).
func NewAgentTree(rootLabel, rootModel string) *AgentTree {
	return NewAgentTreeWithConcurrency(rootLabel, rootModel, MaxConcurrentSpawns)
}

// NewAgentTreeWithConcurrency creates a tree whose spawn semaphore is sized to
// maxConcurrent (the number of children that may run at once). A value <= 0
// falls back to MaxConcurrentSpawns. Yield/unyield mechanics are unaffected —
// only the semaphore's capacity changes.
func NewAgentTreeWithConcurrency(rootLabel, rootModel string, maxConcurrent int) *AgentTree {
	if maxConcurrent <= 0 {
		maxConcurrent = MaxConcurrentSpawns
	}
	// StartedAt stays zero until the first BeginTurn(): the agents tab renders an
	// idle root (no elapsed time) before the first prompt, rather than a timer
	// counting time-since-launch.
	root := &AgentNode{
		ID:     newNodeID(),
		Label:  rootLabel,
		Model:  rootModel,
		Status: StatusRunning,
	}
	return &AgentTree{
		nodes:    map[string]*AgentNode{root.ID: root},
		rootID:   root.ID,
		out:      make(chan TreeUpdate, 256),
		dirty:    map[string]bool{},
		spawnSem: make(chan struct{}, maxConcurrent),
	}
}

// RootID returns the root node's ID.
func (t *AgentTree) RootID() string { return t.rootID }

// Node returns the node with the given ID, or nil if not found.
func (t *AgentTree) Node(id string) *AgentNode {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.nodes[id]
}

// SnapshotAll returns snapshots of every node in depth-first insertion order.
// This is the race-safe form of Nodes() for UI consumption.
func (t *AgentTree) SnapshotAll() []NodeView {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]NodeView, 0, len(t.nodes))
	var visit func(id string)
	visit = func(id string) {
		n, ok := t.nodes[id]
		if !ok {
			return
		}
		out = append(out, n.Snapshot())
		n.mu.Lock()
		children := make([]string, len(n.children))
		copy(children, n.children)
		n.mu.Unlock()
		for _, cid := range children {
			visit(cid)
		}
	}
	visit(t.rootID)
	return out
}

// BeginTurn marks the root node running with a fresh clock. Called when a
// user prompt starts a turn: the root's elapsed time measures the turn, not
// the session (without this the agents view counts up forever).
func (t *AgentTree) BeginTurn() {
	root := t.Node(t.rootID)
	if root == nil {
		return
	}
	root.mu.Lock()
	root.Status = StatusRunning
	root.StartedAt = time.Now()
	root.EndedAt = time.Time{}
	root.mu.Unlock()
	t.Emit(TreeUpdate{NodeID: t.rootID})
}

// SetRootModel updates the root node's rendered model and label to the given
// model. Called when the session model switches (/model) so the agents tab shows
// the current selection rather than the model snapshotted at construction.
func (t *AgentTree) SetRootModel(model string) {
	root := t.Node(t.rootID)
	if root == nil {
		return
	}
	root.mu.Lock()
	root.Model = model
	root.Label = model
	root.mu.Unlock()
	t.Emit(TreeUpdate{NodeID: t.rootID})
}

// EndTurn freezes the root node's clock when the agent loop finishes.
func (t *AgentTree) EndTurn(failed bool) {
	root := t.Node(t.rootID)
	if root == nil {
		return
	}
	status := StatusDone
	if failed {
		status = StatusError
	}
	root.mu.Lock()
	root.Status = status
	root.EndedAt = time.Now()
	root.mu.Unlock()
	t.Emit(TreeUpdate{NodeID: t.rootID})
}

// ActiveCounts returns how many child agents (root excluded) are currently
// running and how many are queued pending a spawn slot. Drives the shell
// status bar so fan-out is visible without opening the agents overlay.
func (t *AgentTree) ActiveCounts() (running, pending int) {
	for _, v := range t.SnapshotAll() {
		if v.Depth == 0 {
			continue
		}
		switch v.Status {
		case StatusRunning:
			running++
		case StatusPending:
			pending++
		}
	}
	return running, pending
}

// addNode adds a node to the tree under its ParentID.
func (t *AgentTree) addNode(n *AgentNode) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.nodes[n.ID] = n
	if n.ParentID != "" {
		if p, ok := t.nodes[n.ParentID]; ok {
			p.mu.Lock()
			p.children = append(p.children, n.ID)
			p.mu.Unlock()
		}
	}
}

// Emit sends a tree update non-blocking. If the buffer is full the update is
// coalesced via the dirty set.
func (t *AgentTree) Emit(update TreeUpdate) {
	select {
	case t.out <- update:
	default:
		t.mu.Lock()
		t.dirty[update.NodeID] = true
		t.mu.Unlock()
	}
}

// Updates returns the channel for receiving tree updates.
func (t *AgentTree) Updates() <-chan TreeUpdate { return t.out }

// acquireSpawnSlot blocks until a spawn slot is free or ctx ends. Returns
// false when the context was cancelled first. Nil-safe: without a tree (or a
// sem) there is no cap.
func (t *AgentTree) acquireSpawnSlot(ctx context.Context) bool {
	if t == nil || t.spawnSem == nil {
		return true
	}
	select {
	case t.spawnSem <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

// releaseSpawnSlot frees a slot taken by acquireSpawnSlot.
func (t *AgentTree) releaseSpawnSlot() {
	if t == nil || t.spawnSem == nil {
		return
	}
	<-t.spawnSem
}

// YieldSlot releases node's spawn slot while it blocks waiting on a child.
// Without this, width capping deadlocks: parents hold every slot while their
// children queue for one (observed live — 8 blocked parents, all children
// pending, zero progress). A slot must mean "actively working", not "alive".
// Safe under parallel spawn batches: only the first concurrent wait releases.
// No-op for the root (depth 0 — it never holds a slot) and nil trees.
func (t *AgentTree) YieldSlot(node *AgentNode) {
	if t == nil || t.spawnSem == nil || node == nil || node.Depth == 0 {
		return
	}
	node.mu.Lock()
	node.yields++
	first := node.yields == 1
	node.mu.Unlock()
	if first {
		t.releaseSpawnSlot()
	}
}

// UnyieldSlot re-acquires node's slot after a child wait completes; only the
// last of the node's concurrent waits re-acquires (blocking until a slot is
// free, which is the fair queueing point for resumed parents). Returns false
// if ctx ended first.
func (t *AgentTree) UnyieldSlot(ctx context.Context, node *AgentNode) bool {
	if t == nil || t.spawnSem == nil || node == nil || node.Depth == 0 {
		return true
	}
	node.mu.Lock()
	node.yields--
	last := node.yields == 0
	node.mu.Unlock()
	if last {
		return t.acquireSpawnSlot(ctx)
	}
	return true
}

// RegisterRemote adds a named RemoteExecutor (id="" for the default).
// CancelNode calls the cancel func stored on the named node, stopping the
// node and (via context propagation) its entire subtree.
func (t *AgentTree) CancelNode(id string) {
	t.mu.RLock()
	n := t.nodes[id]
	t.mu.RUnlock()
	if n == nil {
		return
	}
	n.mu.Lock()
	f := n.cancel
	n.mu.Unlock()
	if f != nil {
		f()
	}
}

// StartDirtyFlusher starts a background goroutine that periodically flushes
// coalesced dirty-map updates into the out channel. ctx cancels the flusher.
func (t *AgentTree) StartDirtyFlusher(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				t.mu.Lock()
				dirty := t.dirty
				t.dirty = map[string]bool{}
				t.mu.Unlock()
				for id := range dirty {
					select {
					case t.out <- TreeUpdate{NodeID: id}:
					default:
						t.mu.Lock()
						t.dirty[id] = true
						t.mu.Unlock()
					}
				}
			case <-ctx.Done():
				return
			}
		}
	}()
}

// newNodeID generates a time-ordered unique node ID.
func newNodeID() string {
	var b [6]byte
	rand.Read(b[:])
	return fmt.Sprintf("%013x%s", time.Now().UnixMilli(), hex.EncodeToString(b[:]))
}
