package agent

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// human_handles.go: @handle addressing, the /btw structured aside parser and
// harness answerer, and the async router seam (ADR-0022).

// HandleRegistry maps human-typeable @handles to node IDs. Handles are auto-
// derived from a node's label at spawn (collisions get -2/-3 suffixes) and may be
// human-renamed; the node ID is the stable identity, the handle is a label.
type HandleRegistry struct {
	mu       sync.RWMutex
	byHandle map[string]string // "@coder-2" -> nodeID
	byNode   map[string]string // nodeID -> "@coder-2"
	counters map[string]int    // per-base collision counter
}

// NewHandleRegistry returns an empty registry.
func NewHandleRegistry() *HandleRegistry {
	return &HandleRegistry{
		byHandle: map[string]string{},
		byNode:   map[string]string{},
		counters: map[string]int{},
	}
}

// sanitizeHandle turns a node label into a handle base: lowercase, spaces and
// runs of non-alphanumerics collapsed to single dashes, trimmed. "Read A" ->
// "read-a"; "" -> "agent".
func sanitizeHandle(label string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(label)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "agent"
	}
	return out
}

// Register assigns a handle to nodeID derived from label, disambiguating
// collisions, and returns the assigned handle (including the leading "@").
func (r *HandleRegistry) Register(nodeID, label string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	base := sanitizeHandle(label)
	handle := "@" + base
	if _, exists := r.byHandle[handle]; exists {
		r.counters[base]++
		handle = fmt.Sprintf("@%s-%d", base, r.counters[base]+1)
		// Extremely unlikely secondary collision (a prior rename took the -N slot):
		// keep bumping until free.
		for _, taken := r.byHandle[handle]; taken; _, taken = r.byHandle[handle] {
			r.counters[base]++
			handle = fmt.Sprintf("@%s-%d", base, r.counters[base]+1)
		}
	}
	r.byHandle[handle] = nodeID
	r.byNode[nodeID] = handle
	return handle
}

// Rename re-points oldHandle to newHandle for the same node. Returns false if
// oldHandle is unknown or newHandle is already taken. NodeID is unchanged, so
// already-enqueued messages (bound by ID) are unaffected.
func (r *HandleRegistry) Rename(oldHandle, newHandle string) bool {
	oldHandle = ensureAt(oldHandle)
	newHandle = ensureAt(newHandle)
	r.mu.Lock()
	defer r.mu.Unlock()
	nodeID, ok := r.byHandle[oldHandle]
	if !ok {
		return false
	}
	if _, taken := r.byHandle[newHandle]; taken {
		return false
	}
	delete(r.byHandle, oldHandle)
	r.byHandle[newHandle] = nodeID
	r.byNode[nodeID] = newHandle
	return true
}

// Resolve returns the node ID for a handle (with or without leading "@").
func (r *HandleRegistry) Resolve(handle string) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	id, ok := r.byHandle[ensureAt(handle)]
	return id, ok
}

// HandleFor returns the current handle for a node ID (for fresh render lookups).
func (r *HandleRegistry) HandleFor(nodeID string) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	h, ok := r.byNode[nodeID]
	return h, ok
}

// Handles returns all registered handles, sorted — for tab-completion.
func (r *HandleRegistry) Handles() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.byHandle))
	for h := range r.byHandle {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

func ensureAt(h string) string {
	if !strings.HasPrefix(h, "@") {
		return "@" + h
	}
	return h
}

// --- /btw structured aside ------------------------------------------------------

// AsideKind is the parsed intent of a /btw question. Deterministic structural
// parse, not NLP — the harness answers each kind from live state, no model call.
type AsideKind int

const (
	AsideUnknown AsideKind = iota
	AsideStatus            // "is X running", "X status", "did X finish"
	AsideLastTool          // "what is X doing", "X's last tool"
	AsideWrites            // "what did X write", "X's blackboard"
	AsideCount             // "how many running", "active count"
	AsideTree              // "show the tree", "who's spawned"
)

// AsideQuery is a parsed /btw question: an intent plus an optional @handle target.
type AsideQuery struct {
	Kind   AsideKind
	Target string // handle (without validation); "" for global queries
	Raw    string
}

// ParseAside deterministically extracts a target @handle and classifies intent.
// Order matters: tree/count (global) are checked before target-scoped kinds so a
// bare "how many running" isn't misread as a status query.
func ParseAside(text string) AsideQuery {
	q := AsideQuery{Raw: text}
	// Pull out an @handle mention anywhere in the text.
	for _, tok := range strings.Fields(text) {
		if strings.HasPrefix(tok, "@") && len(tok) > 1 {
			q.Target = strings.TrimRight(tok, "?.,!")
			break
		}
	}
	lower := strings.ToLower(text)
	switch {
	case containsAny(lower, "how many", "active count", "count "):
		q.Kind = AsideCount
	case containsAny(lower, "tree", "spawned", "who's", "whos ", "nodes"):
		q.Kind = AsideTree
	case containsAny(lower, "doing", "last tool", "working on", "current tool"):
		q.Kind = AsideLastTool
	case containsAny(lower, "wrote", "write", "blackboard", "output"):
		q.Kind = AsideWrites
	case containsAny(lower, "running", "status", "still going", "finished", "done", "stuck"):
		q.Kind = AsideStatus
	default:
		q.Kind = AsideUnknown
	}
	return q
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// AnswerAside answers a parsed /btw query from live tree/blackboard state with NO
// model call and NO node interruption — it reads only race-safe snapshots. The
// returned string is rendered as a transcript line and never delivered to a node.
func AnswerAside(q AsideQuery, tree *AgentTree, bb *Blackboard, reg *HandleRegistry) string {
	switch q.Kind {
	case AsideCount:
		running, pending := tree.ActiveCounts()
		return fmt.Sprintf("%d running, %d pending across the tree", running, pending)

	case AsideTree:
		return renderAsideTree(tree, reg)

	case AsideStatus:
		nv, ok := resolveNode(q.Target, tree, reg)
		if !ok {
			return asideNoTarget(q.Target, tree, reg, "status")
		}
		return fmt.Sprintf("%s: %s%s", handleOf(nv.ID, reg, nv.Label), nv.Status, elapsedSuffix(nv))

	case AsideLastTool:
		nv, ok := resolveNode(q.Target, tree, reg)
		if !ok {
			return asideNoTarget(q.Target, tree, reg, "last-tool")
		}
		if node := tree.Node(nv.ID); node != nil {
			evs := node.CopyEvents()
			for i := len(evs) - 1; i >= 0; i-- {
				if evs[i].Kind == KindToolCall {
					return fmt.Sprintf("%s last tool: %s", handleOf(nv.ID, reg, nv.Label), evs[i].Name)
				}
			}
		}
		return fmt.Sprintf("%s: no tool calls yet", handleOf(nv.ID, reg, nv.Label))

	case AsideWrites:
		nv, ok := resolveNode(q.Target, tree, reg)
		if !ok {
			return asideNoTarget(q.Target, tree, reg, "writes")
		}
		var keys []string
		for k, e := range bb.Snapshot() {
			if e.WriterID == nv.ID || e.WriterLabel == nv.Label {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		if len(keys) == 0 {
			return fmt.Sprintf("%s wrote no blackboard keys", handleOf(nv.ID, reg, nv.Label))
		}
		return fmt.Sprintf("%s wrote %d blackboard key(s): %s",
			handleOf(nv.ID, reg, nv.Label), len(keys), strings.Join(keys, ", "))

	default: // AsideUnknown
		return "/btw can report: status, last-tool, writes, count, tree. " +
			"Examples: /btw status of @coder · /btw what is @coder doing · /btw how many running · /btw show tree"
	}
}

func renderAsideTree(tree *AgentTree, reg *HandleRegistry) string {
	views := tree.SnapshotAll()
	if len(views) == 0 {
		return "no agents"
	}
	var b strings.Builder
	for i, nv := range views {
		if i > 0 {
			b.WriteString("\n")
		}
		indent := strings.Repeat("  ", nv.Depth)
		b.WriteString(fmt.Sprintf("%s%s [%s]%s", indent, handleOf(nv.ID, reg, nv.Label), nv.Status, elapsedSuffix(nv)))
	}
	return b.String()
}

// resolveNode finds a node by handle (preferred) or, if no target was given and
// exactly one non-root node is live, that node. Returns its NodeView.
func resolveNode(target string, tree *AgentTree, reg *HandleRegistry) (NodeView, bool) {
	if target != "" && reg != nil {
		if id, ok := reg.Resolve(target); ok {
			if node := tree.Node(id); node != nil {
				return node.Snapshot(), true
			}
		}
		return NodeView{}, false
	}
	// No explicit target: if exactly one non-root node exists, use it.
	views := tree.SnapshotAll()
	var only NodeView
	count := 0
	for _, nv := range views {
		if nv.ID == tree.RootID() {
			continue
		}
		only = nv
		count++
	}
	if count == 1 {
		return only, true
	}
	return NodeView{}, false
}

func asideNoTarget(target string, tree *AgentTree, reg *HandleRegistry, kind string) string {
	if target != "" {
		live := "none"
		if reg != nil {
			if hs := reg.Handles(); len(hs) > 0 {
				live = strings.Join(hs, ", ")
			}
		}
		return fmt.Sprintf("no node %s. Live handles: %s", target, live)
	}
	return fmt.Sprintf("which node? try: /btw %s of @<handle>", kind)
}

func handleOf(nodeID string, reg *HandleRegistry, label string) string {
	if reg != nil {
		if h, ok := reg.HandleFor(nodeID); ok {
			return h
		}
	}
	if label != "" {
		return label
	}
	return nodeID
}

func elapsedSuffix(nv NodeView) string {
	if nv.StartedAt.IsZero() {
		return ""
	}
	end := nv.EndedAt
	if end.IsZero() {
		end = time.Now()
	}
	return fmt.Sprintf(", elapsed %s", end.Sub(nv.StartedAt).Round(time.Second))
}

// --- async advisory router ------------------------------------------------------

// RouteDecision is the router's classification of a bare-text human message.
type RouteDecision struct {
	Mode   MsgMode `json:"-"`
	Handle string  `json:"handle,omitempty"`
	// ModeStr is the wire form the classifier LLM emits ("direct" | "queued").
	ModeStr string `json:"mode"`
}

// NodeInfo is the compact per-node context handed to the router.
type NodeInfo struct {
	Handle   string
	Label    string
	Status   string
	Depth    int
	LastTool string
}

// RouterLLM is the injected classification seam. A real implementation calls a
// cheap structured-output model; tests supply a deterministic fake. It must be
// safe to call concurrently and honor ctx cancellation/timeout.
type RouterLLM interface {
	Classify(ctx context.Context, text string, live []NodeInfo, selected string) (RouteDecision, error)
}

// LiveNodeInfo builds the router/aside context from the tree and handle registry:
// one NodeInfo per live (running or pending) non-terminal node.
func LiveNodeInfo(tree *AgentTree, reg *HandleRegistry) []NodeInfo {
	var out []NodeInfo
	for _, nv := range tree.SnapshotAll() {
		if nv.Status != StatusRunning && nv.Status != StatusPending {
			continue
		}
		info := NodeInfo{
			Handle: handleOf(nv.ID, reg, nv.Label),
			Label:  nv.Label,
			Status: nv.Status.String(),
			Depth:  nv.Depth,
		}
		if node := tree.Node(nv.ID); node != nil {
			evs := node.CopyEvents()
			for i := len(evs) - 1; i >= 0; i-- {
				if evs[i].Kind == KindToolCall {
					info.LastTool = evs[i].Name
					break
				}
			}
		}
		out = append(out, info)
	}
	return out
}
