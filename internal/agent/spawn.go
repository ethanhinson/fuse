package agent

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
	"time"

	"github.com/ethanhinson/fuse/internal/secrets"
)

// SpawnOpts configures a child agent spawn.
type SpawnOpts struct {
	Label          string
	Task           string
	SystemPrompt   string
	Tools          []string
	ModelID        string
	MaxTurns       int
	MaxTokens      int
	Remote         bool
	RemoteID       string
	IntentPluginID string
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

// Spawn creates a child agent. For local spawns it runs in a goroutine; for
// remote spawns it dispatches via the tree's registered RemoteExecutor.
func (s *Spawner) Spawn(ctx context.Context, opts SpawnOpts) (AgentHandle, error) {
	newDepth := s.depth + 1
	if newDepth > MaxDepth {
		return AgentHandle{}, fmt.Errorf("%w: depth %d > %d", ErrMaxDepthExceeded, newDepth, MaxDepth)
	}
	if opts.Remote {
		return s.spawnRemote(ctx, opts, newDepth)
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

func (s *Spawner) spawnRemote(ctx context.Context, opts SpawnOpts, depth int) (AgentHandle, error) {
	if s.tree == nil {
		return AgentHandle{}, fmt.Errorf("%w: no agent tree configured", ErrNoRemoteExecutor)
	}

	exec := s.tree.lookupRemote(opts.RemoteID)
	if exec == nil {
		return AgentHandle{}, fmt.Errorf("%w: id=%q", ErrNoRemoteExecutor, opts.RemoteID)
	}

	plugin := s.tree.lookupIntent(opts.IntentPluginID)

	// Resolve intent synchronously before node creation.
	rc, err := plugin.Resolve(ctx, opts)
	if err != nil {
		return AgentHandle{}, fmt.Errorf("%w: %v", ErrIntentResolveFailed, err)
	}

	// Resolve secrets synchronously before node creation.
	store := s.tree.secretsStore()
	sseExec, isSse := exec.(*SSERemoteExecutor)

	var bearerToken string
	if isSse && sseExec.TokenSecret != "" {
		tok, err := store.Get(ctx, sseExec.TokenSecret)
		if err != nil {
			if errors.Is(err, secrets.ErrSecretNotFound) {
				return AgentHandle{}, fmt.Errorf("%w: %s", ErrSecretNotFound, sseExec.TokenSecret)
			}
			return AgentHandle{}, fmt.Errorf("%w: %v", ErrSecretsStoreFailure, err)
		}
		bearerToken = tok
	}

	if rc.Clone != nil && rc.Clone.TokenSecret != "" {
		tok, err := store.Get(ctx, rc.Clone.TokenSecret)
		if err != nil {
			if errors.Is(err, secrets.ErrSecretNotFound) {
				return AgentHandle{}, fmt.Errorf("%w: %s", ErrSecretNotFound, rc.Clone.TokenSecret)
			}
			return AgentHandle{}, fmt.Errorf("%w: %v", ErrSecretsStoreFailure, err)
		}
		rc.Clone.ResolvedToken = tok
	}

	var writeBackToken string
	if rc.WriteBackTokenSecret != "" {
		tok, err := store.Get(ctx, rc.WriteBackTokenSecret)
		if err != nil {
			if errors.Is(err, secrets.ErrSecretNotFound) {
				return AgentHandle{}, fmt.Errorf("%w: %s", ErrSecretNotFound, rc.WriteBackTokenSecret)
			}
			return AgentHandle{}, fmt.Errorf("%w: %v", ErrSecretsStoreFailure, err)
		}
		writeBackToken = tok
	}

	var containerSecrets *secrets.ContainerSecrets
	if len(rc.SecretRefs) > 0 {
		pubKey := ""
		if isSse {
			pubKey = sseExec.PublicKey
		}
		cs, err := store.ExportForContainer(ctx, rc.SecretRefs, pubKey)
		if err != nil {
			return AgentHandle{}, fmt.Errorf("%w: export secrets: %v", ErrSecretsStoreFailure, err)
		}
		containerSecrets = cs
	}

	// Create node after all secret resolution succeeds.
	parentID := ""
	if s.node != nil {
		parentID = s.node.ID
	}
	node := &AgentNode{
		ID:         newNodeID(),
		ParentID:   parentID,
		Label:      opts.Label,
		Model:      opts.ModelID,
		Status:     StatusPending,
		Depth:      depth,
		StartedAt:  time.Now(),
		RemoteExec: true,
	}
	s.tree.addNode(node)
	s.tree.Emit(TreeUpdate{NodeID: node.ID})

	// Build the dispatch request.
	req := RemoteDispatchRequest{
		Label:           opts.Label,
		Task:            opts.Task,
		SystemPrompt:    opts.SystemPrompt,
		Tools:           opts.Tools,
		ModelID:         opts.ModelID,
		MaxTurns:        opts.MaxTurns,
		MaxTokens:       opts.MaxTokens,
		ParentNodeID:    parentID,
		Depth:           depth,
		Image:           rc.Image,
		Env:             rc.Env,
		Bundle:          rc.Bundle,
		WriteBackBranch: rc.WriteBackBranch,
		WriteBackRemote: rc.WriteBackRemote,
		WriteBackToken:  writeBackToken,
		Files:           rc.Files,
	}
	if rc.Clone != nil {
		req.Clone = &GitCloneSpec{
			URL:           rc.Clone.URL,
			Ref:           rc.Clone.Ref,
			ResolvedToken: rc.Clone.ResolvedToken,
		}
	}
	if containerSecrets != nil {
		if containerSecrets.EncryptedBundle != nil {
			req.EncryptedSecrets = containerSecrets.EncryptedBundle
			req.SecretsAlgorithm = containerSecrets.BundleAlgorithm
		} else {
			req.Secrets = containerSecrets.Env
		}
	}

	// Clone SSE executor with the resolved token.
	var dispatchExec RemoteExecutor = exec
	if isSse {
		copy := *sseExec
		copy.resolvedToken = bearerToken
		dispatchExec = &copy
	}

	childCtx, cancel := context.WithCancel(ctx)
	eventCh, err := dispatchExec.Dispatch(childCtx, req)
	if err != nil {
		cancel()
		return AgentHandle{}, fmt.Errorf("%w: %v", ErrRemoteDispatchFailed, err)
	}

	doneCh := make(chan SpawnDone, 1)
	go func() {
		defer cancel()
		node.mu.Lock()
		node.Status = StatusRunning
		node.mu.Unlock()
		s.tree.Emit(TreeUpdate{NodeID: node.ID})

		var result string
		var spawnErr error
		var sawTerminal bool

		for evt := range eventCh {
			node.AddEvent(evt)
			s.tree.Emit(TreeUpdate{NodeID: node.ID})

			switch evt.Kind {
			case KindDone:
				sawTerminal = true
				if r, ok := evt.Payload["result"].(string); ok {
					result = r
				}
			case KindError:
				sawTerminal = true
				// The stream-lost sentinel is matched by the locally synthesized
				// event's Name — never by error text, which the remote controls.
				if evt.Name == streamLostEventName {
					spawnErr = ErrRemoteStreamLost
				} else if e, ok := evt.Payload["error"].(string); ok {
					spawnErr = errors.New(e)
				} else {
					spawnErr = fmt.Errorf("remote error event %q", evt.Name)
				}
			}
		}

		// The channel can close with NO terminal event — on ctx cancellation the
		// stream goroutine just returns. That must not be reported as success:
		// a cancelled job is cancelled, and any other silent close is a lost
		// stream, not a completed one.
		if !sawTerminal {
			if childCtx.Err() != nil {
				node.Finish(StatusCancelled, "")
				s.tree.Emit(TreeUpdate{NodeID: node.ID})
				doneCh <- SpawnDone{Err: context.Canceled}
				return
			}
			spawnErr = ErrRemoteStreamLost
		}

		if spawnErr != nil {
			node.Finish(StatusError, spawnErr.Error())
		} else {
			node.Finish(StatusDone, "")
		}
		s.tree.Emit(TreeUpdate{NodeID: node.ID})

		finalResult := SpawnDone{Result: result, Err: spawnErr}
		doneCh <- finalResult

		// Async collect (non-fatal).
		go func() {
			collectErr := plugin.Collect(context.Background(), finalResult, rc)
			if collectErr != nil {
				node.AddEvent(AgentEvent{
					Kind:    KindError,
					Name:    "collect",
					Payload: map[string]any{"error": collectErr.Error()},
					TS:      time.Now(),
				})
				s.tree.Emit(TreeUpdate{NodeID: node.ID})
			}
		}()
	}()

	return AgentHandle{NodeID: node.ID, Done: doneCh, cancel: cancel}, nil
}
