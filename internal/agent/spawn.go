package agent

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
	"time"
)

// ErrMaxDepthExceeded is returned when a spawn would exceed MaxDepth.
var ErrMaxDepthExceeded = errors.New("agent: max spawn depth exceeded")

// SpawnOpts configures a child agent spawn.
type SpawnOpts struct {
	Label        string
	Task         string
	SystemPrompt string
	Tools        []string
	ModelID      string
	MaxTurns     int
	MaxTokens    int
}

// SpawnDone carries the result of a completed child agent.
type SpawnDone struct {
	Result string
	Err    error
}

// AgentHandle holds a reference to a spawned child agent.
type AgentHandle struct {
	NodeID string
	Done   <-chan SpawnDone
	cancel context.CancelFunc
}

// Cancel cancels the child agent's context.
func (h *AgentHandle) Cancel() { h.cancel() }

// Wait blocks until the child agent completes and returns its result.
func (h *AgentHandle) Wait() SpawnDone { return <-h.Done }

// SpawnGroup coordinates multiple concurrent child spawns.
type SpawnGroup struct {
	mu      sync.Mutex
	handles []AgentHandle
	spawner func(ctx context.Context, opts SpawnOpts) (AgentHandle, error)
}

// NewSpawnGroup creates a SpawnGroup using the provided spawner function.
func NewSpawnGroup(spawner func(ctx context.Context, opts SpawnOpts) (AgentHandle, error)) *SpawnGroup {
	return &SpawnGroup{spawner: spawner}
}

// Spawn adds a child agent to the group.
func (g *SpawnGroup) Spawn(ctx context.Context, opts SpawnOpts) (AgentHandle, error) {
	h, err := g.spawner(ctx, opts)
	if err != nil {
		return AgentHandle{}, err
	}
	g.mu.Lock()
	g.handles = append(g.handles, h)
	g.mu.Unlock()
	return h, nil
}

// Join waits for all spawned agents to complete and returns their results.
func (g *SpawnGroup) Join(ctx context.Context) ([]SpawnDone, error) {
	g.mu.Lock()
	handles := make([]AgentHandle, len(g.handles))
	copy(handles, g.handles)
	g.mu.Unlock()

	results := make([]SpawnDone, len(handles))
	var wg sync.WaitGroup
	for i, h := range handles {
		wg.Add(1)
		go func(i int, h AgentHandle) {
			defer wg.Done()
			select {
			case done := <-h.Done:
				results[i] = done
			case <-ctx.Done():
				results[i] = SpawnDone{Err: ctx.Err()}
			}
		}(i, h)
	}
	wg.Wait()
	return results, nil
}

// ChildBuilder runs a local child agent. node is the child's already-created
// AgentNode in the tree. It returns the final result text.
type ChildBuilder func(ctx context.Context, opts SpawnOpts, node *AgentNode, tree *AgentTree) (string, error)

// Option configures a Spawner.
type Option func(*Spawner)

// WithTree sets the agent tree on a Spawner.
func WithTree(t *AgentTree) Option { return func(s *Spawner) { s.tree = t } }

// WithNode sets the current (parent) node on a Spawner.
func WithNode(n *AgentNode) Option { return func(s *Spawner) { s.node = n } }

// WithSpawnDepth sets the current spawn depth.
func WithSpawnDepth(depth int) Option { return func(s *Spawner) { s.depth = depth } }

// WithChildBuilder injects the local child-agent runner into a Spawner.
func WithChildBuilder(fn ChildBuilder) Option { return func(s *Spawner) { s.buildChild = fn } }

// Spawner provides the Spawn method for creating child agents.
type Spawner struct {
	tree       *AgentTree
	node       *AgentNode
	depth      int
	buildChild ChildBuilder
}

// NewSpawner creates a Spawner with the provided options.
func NewSpawner(opts ...Option) *Spawner {
	s := &Spawner{}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Spawn creates a child agent running locally in a goroutine.
func (s *Spawner) Spawn(ctx context.Context, opts SpawnOpts) (AgentHandle, error) {
	newDepth := s.depth + 1
	if newDepth > MaxDepth {
		return AgentHandle{}, fmt.Errorf("%w: depth %d > %d", ErrMaxDepthExceeded, newDepth, MaxDepth)
	}
	return s.spawnLocal(ctx, opts, newDepth)
}

func (s *Spawner) spawnLocal(ctx context.Context, opts SpawnOpts, depth int) (AgentHandle, error) {
	parentID := ""
	if s.node != nil {
		parentID = s.node.ID
	}
	node := &AgentNode{
		ID:       newNodeID(),
		ParentID: parentID,
		Label:    opts.Label,
		Model:    opts.ModelID,
		Status:   StatusPending,
		Depth:    depth,
	}
	if s.tree != nil {
		s.tree.addNode(node)
		s.tree.Emit(TreeUpdate{NodeID: node.ID})
	}

	doneCh := make(chan SpawnDone, 1)
	childCtx, cancel := context.WithCancel(ctx)
	node.SetCancel(cancel) // allows tree.CancelNode to stop this node

	go func() {
		defer cancel()

		// Width cap: wait for a spawn slot while the node stays visibly pending.
		// Depth limits alone don't bound load when the model fans out widely.
		if !s.tree.acquireSpawnSlot(childCtx) {
			node.Finish(StatusCancelled, "")
			if s.tree != nil {
				s.tree.Emit(TreeUpdate{NodeID: node.ID})
			}
			doneCh <- SpawnDone{Err: childCtx.Err()}
			return
		}
		defer s.tree.releaseSpawnSlot()

		node.mu.Lock()
		node.Status = StatusRunning
		node.StartedAt = time.Now()
		node.mu.Unlock()
		if s.tree != nil {
			s.tree.Emit(TreeUpdate{NodeID: node.ID})
		}

		var result string
		var runErr error

		if s.buildChild != nil {
			// Backstop: a panic on a child goroutine kills the whole process,
			// TUI included. Convert it into a child error instead.
			func() {
				defer func() {
					if rec := recover(); rec != nil {
						stack := debug.Stack()
						if len(stack) > 2048 {
							stack = stack[:2048]
						}
						runErr = fmt.Errorf("child agent panicked: %v\n%s", rec, stack)
					}
				}()
				result, runErr = s.buildChild(childCtx, opts, node, s.tree)
			}()
		}

		if runErr != nil {
			if errors.Is(runErr, context.Canceled) {
				node.Finish(StatusCancelled, "")
			} else {
				node.Finish(StatusError, runErr.Error())
			}
		} else {
			node.Finish(StatusDone, "")
		}
		if s.tree != nil {
			s.tree.Emit(TreeUpdate{NodeID: node.ID})
		}
		doneCh <- SpawnDone{Result: result, Err: runErr}
	}()

	return AgentHandle{NodeID: node.ID, Done: doneCh, cancel: cancel}, nil
}
