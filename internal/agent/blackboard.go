package agent

import (
	"context"
	"fmt"
	"path"
	"sort"
	"sync"
	"time"

	"github.com/ethanhinson/fuse/internal/tools"
)

// BlackboardEntry is one value stored on the blackboard, with the provenance of
// the agent that last wrote it (change 0023, D4). Provenance backs the TUI
// wrote-by indicator and is stamped by the store, never supplied by the model.
type BlackboardEntry struct {
	Value       any       // JSON-decoded structured value
	WriterID    string    // AgentNode.ID of the writer
	WriterLabel string    // AgentNode label, for display
	WrittenAt   time.Time // wall-clock of the last Put
}

// Blackboard is a session-scoped, in-memory, thread-safe key/value store shared
// by every agent in an AgentTree (change 0023). It enables ensemble, debate, and
// producer/consumer coordination without touching the spawn/collect architecture:
// agents read and write structured values to shared keys, and Wait blocks a
// consumer until a producer writes a key (or a timeout elapses).
//
// A single sync.RWMutex guards entries and waiters. The tree back-reference is
// used only by Wait, to yield the calling agent's scheduler slot while blocked
// (the D1 anti-deadlock contract); it may be nil in unit tests of the store in
// isolation, in which case Wait skips the yield but still blocks/times-out.
type Blackboard struct {
	mu      sync.RWMutex
	entries map[string]BlackboardEntry
	waiters map[string][]chan struct{}
	tree    *AgentTree
}

// NewBlackboard returns an empty blackboard bound to tree (which may be nil).
func NewBlackboard(tree *AgentTree) *Blackboard {
	return &Blackboard{
		entries: map[string]BlackboardEntry{},
		waiters: map[string][]chan struct{}{},
		tree:    tree,
	}
}

// Put upserts key with value, stamping provenance, and wakes any waiters parked
// on key. Last-writer-wins (no ACLs — any agent may overwrite any key, per the
// confirmed out-of-scope list). Waiter channels are signalled under the lock so a
// Put cannot slip between a waiter's registration and its select (no lost wakeup).
func (b *Blackboard) Put(key string, value any, writerID, writerLabel string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.entries[key] = BlackboardEntry{
		Value:       value,
		WriterID:    writerID,
		WriterLabel: writerLabel,
		WrittenAt:   time.Now(),
	}
	if chans := b.waiters[key]; len(chans) > 0 {
		for _, ch := range chans {
			close(ch)
		}
		delete(b.waiters, key)
	}
}

// Get returns the entry for key under RLock. The bool is false for a missing key.
func (b *Blackboard) Get(key string) (BlackboardEntry, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	e, ok := b.entries[key]
	return e, ok
}

// Delete removes key. It does NOT cancel outstanding waits on key — a subsequent
// Put still wakes them; delete-then-never-write is a timeout case (spec).
func (b *Blackboard) Delete(key string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.entries, key)
}

// Keys returns the keys matching pattern (path.Match glob semantics), sorted for
// stable display and testing. An empty pattern or "*" returns all keys. A pattern
// path.Match rejects as malformed matches nothing.
func (b *Blackboard) Keys(pattern string) []string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	all := pattern == "" || pattern == "*"
	out := make([]string, 0, len(b.entries))
	for k := range b.entries {
		if all {
			out = append(out, k)
			continue
		}
		if ok, err := path.Match(pattern, k); err == nil && ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// Snapshot returns a shallow, top-level-independent copy of every entry under
// RLock, for race-free TUI reads. The returned map is safe to insert into,
// delete from, or replace entries in without affecting the store, and each
// BlackboardEntry is a value copy (its scalar fields are independent). BUT a
// reference-typed Value (a map/slice, as produced by JSON object/array writes)
// is SHARED with the store — this is a shallow copy, not a deep one. That is
// sufficient because the only consumer, the TUI, treats snapshot values as
// read-only. Callers MUST NOT mutate a snapshot entry's Value; doing so would
// race a concurrent writer. (Pinned by TestSnapshotSharesNestedReference.)
func (b *Blackboard) Snapshot() map[string]BlackboardEntry {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make(map[string]BlackboardEntry, len(b.entries))
	for k, v := range b.entries {
		out[k] = v
	}
	return out
}

// Wait blocks until key is set (returning its value), ctx is cancelled (returning
// ctx.Err()), or timeout elapses (returning a timeout error). It is the enabler
// for producer/consumer patterns (change 0023, D1).
//
// The waiter channel is registered BEFORE the has-key re-check so a Put landing
// between the initial read and the registration still wakes this waiter (the
// lost-wakeup guard). While blocked, Wait yields node's scheduler slot via
// tree.YieldSlot and reacquires it via tree.UnyieldSlot in a defer on every
// return path, so a blocked consumer never starves the concurrency cap (the D1
// anti-deadlock contract, reusing 0012's machinery). When tree or node is nil the
// yield is skipped but Wait still blocks/times-out correctly.
func (b *Blackboard) Wait(ctx context.Context, key string, timeout time.Duration, node *AgentNode) (any, error) {
	// Fast path: already set.
	if e, ok := b.Get(key); ok {
		return e.Value, nil
	}

	// Register a waiter channel, then re-check under the lock. Registering before
	// the re-check closes the lost-wakeup race: a Put between the fast-path Get and
	// here either happened before we take the lock (caught by the re-check) or after
	// (it will close our registered channel).
	ch := make(chan struct{})
	b.mu.Lock()
	if e, ok := b.entries[key]; ok {
		b.mu.Unlock()
		return e.Value, nil
	}
	b.waiters[key] = append(b.waiters[key], ch)
	b.mu.Unlock()

	// Yield the caller's scheduler slot while blocked; reacquire on every exit path.
	// Nil-safe: a nil tree/node (unit tests, or the root at depth 0) skips the yield
	// but still blocks/times-out.
	if b.tree != nil && node != nil {
		b.tree.YieldSlot(node)
		defer b.tree.UnyieldSlot(ctx, node)
	}

	select {
	case <-ch:
		// Woken by a Put; read the value it wrote.
		if e, ok := b.Get(key); ok {
			return e.Value, nil
		}
		// Extremely unlikely (a Delete raced the Put's wakeup); treat as unset.
		return nil, fmt.Errorf("blackboard: key %q signalled but not set", key)
	case <-ctx.Done():
		b.removeWaiter(key, ch)
		return nil, ctx.Err()
	case <-time.After(timeout):
		b.removeWaiter(key, ch)
		return nil, fmt.Errorf("blackboard: timed out after %s waiting for key %q", timeout, key)
	}
}

// InboxKey shapes a directed-message key for target's inbox at sequence seq:
// "inbox/<target>/<seq>". The inbox is a pure convention over the existing store
// (change 0023, Task 6) — no new structure, no blocking, no delivery guarantees.
// A sender Put/Writes at InboxKey; a target discovers messages via
// Keys(InboxPattern(self)) and reads each with Get/Read.
func InboxKey(target, seq string) string {
	return "inbox/" + target + "/" + seq
}

// InboxPattern is the glob a target passes to Keys to list its own inbox:
// "inbox/<self>/*" (path.Match semantics).
func InboxPattern(self string) string {
	return "inbox/" + self + "/*"
}

// ForNode returns a tools.BlackboardStore handle bound to node: Write stamps
// node's ID/label as provenance and Wait yields node's scheduler slot while
// blocked. This is the seam cmd/fuse wires per agent — the tool package stays
// agent-free (it only sees tools.BlackboardStore), and provenance/yielding are
// bound here where the node is known. One handle per (blackboard, node).
func (b *Blackboard) ForNode(node *AgentNode) tools.BlackboardStore {
	return &nodeBlackboard{bb: b, node: node}
}

// nodeBlackboard adapts *Blackboard to tools.BlackboardStore for a single node,
// carrying that node's provenance (Write) and yield handle (Wait).
type nodeBlackboard struct {
	bb   *Blackboard
	node *AgentNode
}

func (n *nodeBlackboard) Write(key string, value any) {
	n.bb.Put(key, value, n.node.ID, n.node.Label)
}

func (n *nodeBlackboard) Read(key string) (value any, writerLabel string, ok bool) {
	e, found := n.bb.Get(key)
	if !found {
		return nil, "", false
	}
	return e.Value, e.WriterLabel, true
}

func (n *nodeBlackboard) Delete(key string) { n.bb.Delete(key) }

func (n *nodeBlackboard) Keys(pattern string) []string { return n.bb.Keys(pattern) }

func (n *nodeBlackboard) Wait(ctx context.Context, key string, timeout time.Duration) (any, error) {
	return n.bb.Wait(ctx, key, timeout, n.node)
}

var _ tools.BlackboardStore = (*nodeBlackboard)(nil)

// removeWaiter unregisters ch from key's waiter list (on cancel/timeout) so a
// later Put does not close an abandoned channel and the map does not grow
// unbounded. Idempotent: a channel already removed (closed by Put) is a no-op.
func (b *Blackboard) removeWaiter(key string, ch chan struct{}) {
	b.mu.Lock()
	defer b.mu.Unlock()
	chans := b.waiters[key]
	for i, c := range chans {
		if c == ch {
			b.waiters[key] = append(chans[:i], chans[i+1:]...)
			break
		}
	}
	if len(b.waiters[key]) == 0 {
		delete(b.waiters, key)
	}
}
