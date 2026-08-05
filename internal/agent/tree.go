package agent

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethanhinson/fuse/internal/secrets"
)

// MaxDepth is the maximum allowed spawn depth.
const MaxDepth = 5

// NodeStatus represents the lifecycle state of an agent node.
type NodeStatus int

const (
	StatusPending   NodeStatus = iota
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
	KindSpawned   EventKind = iota
	KindAssistant
	KindToolCall
	KindToolResult
	KindTokens
	KindDone
	KindError
)

func (k EventKind) String() string {
	switch k {
	case KindSpawned:
		return "spawned"
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
	Seq     int64
}

// AgentNode represents one agent in the spawn tree.
type AgentNode struct {
	ID          string
	ParentID    string
	Label       string
	Model       string
	Status      NodeStatus
	Depth       int
	StartedAt   time.Time
	EndedAt     time.Time
	TokensIn    int
	TokensOut   int
	CostUSD     float64
	Events      []AgentEvent
	RemoteExec  bool
	RemoteJobID string
	children    []string
	mu          sync.Mutex
}

// AddEvent appends an event to the node, thread-safe.
func (n *AgentNode) AddEvent(e AgentEvent) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.Events = append(n.Events, e)
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

// TreeUpdate signals a node state change for TUI subscribers.
type TreeUpdate struct {
	NodeID string
}

// AgentTree is the shared state for the spawn tree.
type AgentTree struct {
	mu     sync.RWMutex
	nodes  map[string]*AgentNode
	rootID string
	out    chan TreeUpdate // buffered 256
	seq    atomic.Int64
	dirty  map[string]bool

	remotes   map[string]RemoteExecutor
	remotesMu sync.RWMutex
	intents   map[string]IntentPlugin
	intentsMu sync.RWMutex

	secretsSt secrets.SecretsStore
	secretsMu sync.RWMutex
}

// NewAgentTree creates a new tree with the given root node label and model.
func NewAgentTree(rootLabel, rootModel string) *AgentTree {
	root := &AgentNode{
		ID:        newNodeID(),
		Label:     rootLabel,
		Model:     rootModel,
		Status:    StatusRunning,
		StartedAt: time.Now(),
	}
	return &AgentTree{
		nodes:   map[string]*AgentNode{root.ID: root},
		rootID:  root.ID,
		out:     make(chan TreeUpdate, 256),
		dirty:   map[string]bool{},
		remotes: map[string]RemoteExecutor{},
		intents: map[string]IntentPlugin{},
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

// Nodes returns all nodes in depth-first insertion order.
func (t *AgentTree) Nodes() []*AgentNode {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]*AgentNode, 0, len(t.nodes))
	var visit func(id string)
	visit = func(id string) {
		n, ok := t.nodes[id]
		if !ok {
			return
		}
		out = append(out, n)
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

// NextSeq returns the next global event sequence number.
func (t *AgentTree) NextSeq() int64 {
	return t.seq.Add(1)
}

// SetSecrets installs the secrets store. Thread-safe.
func (t *AgentTree) SetSecrets(s secrets.SecretsStore) {
	t.secretsMu.Lock()
	defer t.secretsMu.Unlock()
	t.secretsSt = s
}

// secretsStore returns the installed store, defaulting to &EnvSecretsStore{}.
func (t *AgentTree) secretsStore() secrets.SecretsStore {
	t.secretsMu.RLock()
	defer t.secretsMu.RUnlock()
	if t.secretsSt != nil {
		return t.secretsSt
	}
	return &secrets.EnvSecretsStore{}
}

// RegisterRemote adds a named RemoteExecutor (id="" for the default).
func (t *AgentTree) RegisterRemote(id string, exec RemoteExecutor) {
	t.remotesMu.Lock()
	defer t.remotesMu.Unlock()
	t.remotes[id] = exec
}

// lookupRemote finds a RemoteExecutor by id.
func (t *AgentTree) lookupRemote(id string) RemoteExecutor {
	t.remotesMu.RLock()
	defer t.remotesMu.RUnlock()
	return t.remotes[id]
}

// RegisterIntent adds a named IntentPlugin (id="" for the default).
func (t *AgentTree) RegisterIntent(id string, plugin IntentPlugin) {
	t.intentsMu.Lock()
	defer t.intentsMu.Unlock()
	t.intents[id] = plugin
}

// lookupIntent finds an IntentPlugin by id, falling back to NilIntentPlugin.
func (t *AgentTree) lookupIntent(id string) IntentPlugin {
	t.intentsMu.RLock()
	defer t.intentsMu.RUnlock()
	if p, ok := t.intents[id]; ok {
		return p
	}
	return NilIntentPlugin{}
}

// newNodeID generates a time-ordered unique node ID.
func newNodeID() string {
	var b [6]byte
	rand.Read(b[:])
	return fmt.Sprintf("%013x%s", time.Now().UnixMilli(), hex.EncodeToString(b[:]))
}
