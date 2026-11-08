// Package runtime drives one agent loop in this process: it builds the loop's root
// agent through the binding's factory, runs it in a goroutine, and owns the loop's
// event store and teardown. The one-shot and research-probe bindings go through it.
//
// It is policy-free: renderers, approval gates and MCP stay in the binding, which
// reaches the runtime only through the Deps closures.
package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/artifact"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/event/fsstore"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/observe"
	"github.com/ethanhinson/fuse/internal/tools"
)

// Runtime starts agent loops.
type Runtime interface {
	// StartLoop constructs one agent loop from cfg, runs it in a goroutine, and
	// returns a handle to await it. A construction failure is returned here and no
	// run starts.
	StartLoop(ctx context.Context, cfg LoopConfig) (LoopHandle, error)
}

// LoopConfig describes one loop to start.
type LoopConfig struct {
	Task    string // the initial user task
	ModelID string // resolved gateway model id; passed to the agent factory
	// Attachments are images sent with the task message.
	Attachments []model.Attachment
}

// LoopHandle is a running loop.
type LoopHandle interface {
	// ID is the loop's id (the tree root id).
	ID() string
	// Wait blocks until the run ends and returns its messages and error. It may be
	// called more than once.
	Wait() ([]model.Message, error)
}

// Deps are the binding-supplied collaborators the Runtime composes. None are
// renderer/TUI/gate types; a binding wires those around the Runtime, not into it.
type Deps struct {
	// Observer receives timing-only lifecycle signals. Nil is identical to a no-op.
	Observer observe.Observer
	// BuildAgent builds the loop's root agent, bound to the loop's own event store,
	// tree and tool registry.
	BuildAgent func(store event.EventStore, tree *agent.AgentTree, modelID string, toolReg *tools.Registry) (*agent.Agent, error)
	// NewToolRegistry builds the loop's tool registry. Nil means an empty one.
	NewToolRegistry func() *tools.Registry
	// Artifacts is the store for spilled tool output and compaction segments. Nil
	// means an in-memory store with a per-loop cap. StartLoop binds the loop's tool
	// registry to it under the loop's own key, overriding any earlier binding.
	Artifacts artifact.Store
	// BaseDir is where the loop's fsstore event log lives. Empty means no event log.
	BaseDir string
	// MaxConcurrent seeds the tree's scheduler concurrency when Tree is nil.
	MaxConcurrent int
	// Tree, when non-nil, is the agent tree StartLoop drives instead of creating one.
	// A binding that reads the tree after the run supplies its own.
	Tree *agent.AgentTree
	// LoopTeardown, when non-nil, runs once when the loop ends, with the loop's tool
	// registry, to release what the binding attached to it. It also runs when
	// construction fails after the registry was built.
	LoopTeardown func(toolReg *tools.Registry)
}

// inProcRuntime is the concrete Runtime.
type inProcRuntime struct {
	deps Deps
}

// New builds an in-process Runtime.
func New(deps Deps) Runtime {
	if deps.Observer == nil {
		deps.Observer = observe.NoopObserver{}
	}
	if deps.Artifacts == nil {
		deps.Artifacts = artifact.NewMemStore(artifact.WithPerLoopCap(artifact.DefaultMemLoopCap))
	}
	return &inProcRuntime{deps: deps}
}

// StartLoop implements Runtime.
func (r *inProcRuntime) StartLoop(ctx context.Context, cfg LoopConfig) (LoopHandle, error) {
	loopCtx, loopSpan := r.deps.Observer.Start(ctx, observe.Descriptor{
		Kind:   observe.OperationLoop,
		Name:   "run",
		Fields: []observe.Field{{Key: "tenant", Value: string(event.DefaultTenant)}},
	})
	end := endOnce(loopSpan)

	tree := r.deps.Tree
	if tree == nil {
		tree = agent.NewAgentTreeWithConcurrency(cfg.ModelID, cfg.ModelID, r.deps.MaxConcurrent)
	}
	rootID := tree.RootID()
	rootNode := tree.Node(rootID)

	var store event.EventStore = event.NoopStore{}
	if r.deps.BaseDir != "" {
		fs, err := fsstore.NewFSEventStore(r.deps.BaseDir, rootID)
		if err != nil {
			end(observe.OutcomeError)
			return nil, fmt.Errorf("runtime: open event store: %w", err)
		}
		store = fs
	}

	toolReg := tools.NewRegistry()
	if r.deps.NewToolRegistry != nil {
		toolReg = r.deps.NewToolRegistry()
	}
	toolReg.WithArtifacts(r.deps.Artifacts, event.DefaultTenant, event.LoopID(rootID))

	a, err := r.deps.BuildAgent(store, tree, cfg.ModelID, toolReg)
	if err != nil {
		closeStore(store)
		if r.deps.LoopTeardown != nil {
			r.deps.LoopTeardown(toolReg)
		}
		end(observe.OutcomeError)
		return nil, fmt.Errorf("runtime: build agent: %w", err)
	}
	a.SetObserver(r.deps.Observer)
	a.SetEventSink(store)
	a.SetNodeIdentity(rootNode.ID, rootNode.ParentID, rootNode.Depth)
	// The runtime owns the root's usage sink and call gate (it holds the tree);
	// children are wired in their builders.
	a.SetUsageSink(rootNode.UpdateTokens)
	a.SetCallGate(func() error { return tree.Scheduler().TokenQuotaDenied(rootNode.ID) })

	h := &loopHandle{id: rootID, done: make(chan struct{})}
	go func() {
		defer close(h.done)
		msgs, rerr := a.Run(loopCtx, []model.Message{{Role: "user", Content: cfg.Task, Attachments: cfg.Attachments}})
		end(observeOutcome(rerr))
		h.msgs, h.err = msgs, rerr
		closeStore(store)
		if r.deps.LoopTeardown != nil {
			r.deps.LoopTeardown(toolReg)
		}
	}()
	return h, nil
}

// closeStore releases the loop's event store: the fsstore write handle and its live
// subscribers. The no-op store has nothing to close.
func closeStore(store event.EventStore) {
	if c, ok := store.(interface{ Close() error }); ok {
		_ = c.Close()
	}
}

// observeOutcome maps a run error to the outcome recorded on the loop span.
func observeOutcome(err error) observe.Outcome {
	switch {
	case err == nil:
		return observe.OutcomeSuccess
	case errors.Is(err, context.DeadlineExceeded):
		return observe.OutcomeTimeout
	case errors.Is(err, context.Canceled):
		return observe.OutcomeCanceled
	default:
		return observe.OutcomeError
	}
}

// endOnce wraps a span handle so it ends at most once.
func endOnce(h observe.Handle) func(observe.Outcome) {
	var once sync.Once
	return func(out observe.Outcome) {
		once.Do(func() { h.End(out) })
	}
}

// loopHandle awaits one running loop. done is closed exactly once, after which msgs
// and err are stable.
type loopHandle struct {
	id   string
	done chan struct{}
	msgs []model.Message
	err  error
}

func (h *loopHandle) ID() string { return h.id }

func (h *loopHandle) Wait() ([]model.Message, error) {
	<-h.done
	return h.msgs, h.err
}
