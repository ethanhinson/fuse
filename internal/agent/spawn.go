package agent

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"time"
)

// ErrMaxDepthExceeded is returned when a spawn would exceed MaxDepth.
var ErrMaxDepthExceeded = errors.New("agent: max spawn depth exceeded")

// ErrSpawnBudgetExhausted is returned when a spawn would exceed the tree-global
// spawn budget (AgentTree.SetMaxSpawns). It is the backstop behind the injected
// budget line: the model is told the budget each spawn, and if it spawns past
// the ceiling anyway this refuses rather than letting fan-out run away.
var ErrSpawnBudgetExhausted = errors.New("agent: spawn budget exhausted")

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

// AgentHandle holds a reference to a spawned child agent. Cancellation goes
// through the tree (AgentNode.SetCancel / Tree.CancelNode), not the handle.
type AgentHandle struct {
	NodeID string
	Done   <-chan SpawnDone
}

// Wait blocks until the child agent completes and returns its result.
func (h *AgentHandle) Wait() SpawnDone { return <-h.Done }

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
	// Tree-global spawn budget backstop. The injected budget line should make
	// this rare; the refusal makes runaway fan-out impossible. Only enforced
	// when a budget is configured (max > 0).
	if s.tree != nil {
		if used, max := s.tree.SpawnBudget(); max > 0 && used >= max {
			return AgentHandle{}, fmt.Errorf("%w: %d/%d spawns used — proceed with the results you already have and do not spawn again", ErrSpawnBudgetExhausted, used, max)
		}
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

	return AgentHandle{NodeID: node.ID, Done: doneCh}, nil
}
