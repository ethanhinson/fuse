package pipeline

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/agent"
)

// fakeSpawner builds a real agent.Spawner whose child builder is scripted: it
// invokes fn(opts) to produce the child's result text, honoring cancellation.
func fakeSpawner(t *testing.T, tree *agent.AgentTree, fn func(opts agent.SpawnOpts) (string, error)) *agent.Spawner {
	t.Helper()
	root := tree.Node(tree.RootID())
	return agent.NewSpawner(
		agent.WithTree(tree),
		agent.WithNode(root),
		agent.WithSpawnDepth(0),
		agent.WithChildBuilder(func(ctx context.Context, opts agent.SpawnOpts, _ *agent.AgentNode, _ *agent.AgentTree) (string, error) {
			return fn(opts)
		}),
	)
}

// TestEngineChainOrder: a->b->c runs strictly in order (each records its start
// only after its predecessor's output is on the blackboard).
func TestEngineChainOrder(t *testing.T) {
	tree := agent.NewAgentTree("root", "m")
	bb := agent.NewBlackboard(tree)

	var mu sync.Mutex
	var order []string

	sp := fakeSpawner(t, tree, func(opts agent.SpawnOpts) (string, error) {
		mu.Lock()
		order = append(order, opts.Label)
		mu.Unlock()
		return "out:" + opts.Label, nil
	})

	p := &Pipeline{Name: "chain", Steps: []Step{
		{Name: "a", Worker: "w", Prompt: "do a", Outputs: []string{"oa"}},
		{Name: "b", Worker: "w", Prompt: "use {{oa}}", Inputs: []string{"oa"}, Outputs: []string{"ob"}, DependsOn: []string{"a"}},
		{Name: "c", Worker: "w", Prompt: "use {{ob}}", Inputs: []string{"ob"}, Outputs: []string{"oc"}, DependsOn: []string{"b"}},
	}}

	st, err := Run(context.Background(), p, sp, bb)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if st.State != StateCompleted {
		t.Fatalf("want completed, got %+v", st)
	}
	if strings.Join(order, ",") != "a,b,c" {
		t.Fatalf("order wrong: %v", order)
	}
}

// TestEngineDiamondConcurrency: in a>{b,c}>d, b and c must overlap in flight.
func TestEngineDiamondConcurrency(t *testing.T) {
	tree := agent.NewAgentTreeWithConcurrency("root", "m", 8)
	bb := agent.NewBlackboard(tree)

	var inFlight, maxInFlight int32
	var bcOverlap int32

	sp := fakeSpawner(t, tree, func(opts agent.SpawnOpts) (string, error) {
		cur := atomic.AddInt32(&inFlight, 1)
		for {
			m := atomic.LoadInt32(&maxInFlight)
			if cur <= m || atomic.CompareAndSwapInt32(&maxInFlight, m, cur) {
				break
			}
		}
		if opts.Label == "b" || opts.Label == "c" {
			// Hold long enough for the sibling to also be in flight.
			time.Sleep(30 * time.Millisecond)
			if atomic.LoadInt32(&inFlight) >= 2 {
				atomic.StoreInt32(&bcOverlap, 1)
			}
		}
		atomic.AddInt32(&inFlight, -1)
		return "out:" + opts.Label, nil
	})

	p := &Pipeline{Name: "diamond", Steps: []Step{
		{Name: "a", Worker: "w", Outputs: []string{"oa"}},
		{Name: "b", Worker: "w", DependsOn: []string{"a"}, Outputs: []string{"ob"}},
		{Name: "c", Worker: "w", DependsOn: []string{"a"}, Outputs: []string{"oc"}},
		{Name: "d", Worker: "w", DependsOn: []string{"b", "c"}, Outputs: []string{"od"}},
	}}

	st, err := Run(context.Background(), p, sp, bb)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if st.State != StateCompleted {
		t.Fatalf("want completed, got %+v", st)
	}
	if atomic.LoadInt32(&bcOverlap) != 1 {
		t.Fatalf("b and c did not overlap (maxInFlight=%d)", maxInFlight)
	}
}

// TestEngineInputsToPromptAndOutputs: inputs are substituted into the prompt and
// the single step's result lands on each literal output key.
func TestEngineInputsToPromptAndOutputs(t *testing.T) {
	tree := agent.NewAgentTree("root", "m")
	bb := agent.NewBlackboard(tree)
	bb.Put("topic", "otters", "seed", "seed")

	var gotPrompt string
	sp := fakeSpawner(t, tree, func(opts agent.SpawnOpts) (string, error) {
		gotPrompt = opts.Task
		return "result-text", nil
	})

	p := &Pipeline{Name: "io", Steps: []Step{
		{Name: "s", Worker: "w", Prompt: "research {{topic}} now", Inputs: []string{"topic"}, Outputs: []string{"out1", "out2"}},
	}}
	if _, err := Run(context.Background(), p, sp, bb); err != nil {
		t.Fatalf("run: %v", err)
	}
	if gotPrompt != "research otters now" {
		t.Fatalf("prompt substitution wrong: %q", gotPrompt)
	}
	for _, k := range []string{"out1", "out2"} {
		e, ok := bb.Get(k)
		if !ok || e.Value != "result-text" {
			t.Fatalf("output %q not written: %+v ok=%v", k, e, ok)
		}
	}
}

// TestEngineFanoutGlobRoundTrip: a hits/* fanout writes hits/0..N-1, and a
// downstream step reading hits/* sees them all substituted.
func TestEngineFanoutGlobRoundTrip(t *testing.T) {
	tree := agent.NewAgentTreeWithConcurrency("root", "m", 8)
	bb := agent.NewBlackboard(tree)

	var spawnCount int32
	sp := fakeSpawner(t, tree, func(opts agent.SpawnOpts) (string, error) {
		if opts.Label == "gather" || strings.HasPrefix(opts.Label, "gather") {
			atomic.AddInt32(&spawnCount, 1)
		}
		return "hit-for:" + opts.Task, nil
	})

	p := &Pipeline{Name: "fan", Steps: []Step{
		{Name: "gather", Worker: "w", Prompt: "search", Outputs: []string{"hits/*"}, Fanout: 3},
		{Name: "rank", Worker: "w", Prompt: "rank {{hits/*}}", Inputs: []string{"hits/*"}, Outputs: []string{"ranked"}, DependsOn: []string{"gather"}},
	}}
	st, err := Run(context.Background(), p, sp, bb)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if st.State != StateCompleted {
		t.Fatalf("want completed, got %+v", st)
	}
	keys := bb.Keys("hits/*")
	if len(keys) != 3 {
		t.Fatalf("want 3 hits keys, got %v", keys)
	}
	if atomic.LoadInt32(&spawnCount) != 3 {
		t.Fatalf("want 3 gather spawns, got %d", spawnCount)
	}
}

// TestEngineFanoutSpawnCount: fanout:3 => exactly 3 spawns for that step.
func TestEngineFanoutSpawnCount(t *testing.T) {
	tree := agent.NewAgentTreeWithConcurrency("root", "m", 8)
	bb := agent.NewBlackboard(tree)
	var n int32
	sp := fakeSpawner(t, tree, func(opts agent.SpawnOpts) (string, error) {
		atomic.AddInt32(&n, 1)
		return "x", nil
	})
	p := &Pipeline{Name: "f", Steps: []Step{
		{Name: "s", Worker: "w", Prompt: "go", Outputs: []string{"o/*"}, Fanout: 3},
	}}
	if _, err := Run(context.Background(), p, sp, bb); err != nil {
		t.Fatalf("run: %v", err)
	}
	if atomic.LoadInt32(&n) != 3 {
		t.Fatalf("want 3 spawns, got %d", n)
	}
}

// TestEngineOnErrorFail: a failing step with default policy fails the pipeline
// and names the offending step.
func TestEngineOnErrorFail(t *testing.T) {
	tree := agent.NewAgentTree("root", "m")
	bb := agent.NewBlackboard(tree)
	sp := fakeSpawner(t, tree, func(opts agent.SpawnOpts) (string, error) {
		if opts.Label == "boom" {
			return "", fmt.Errorf("kaboom")
		}
		return "ok", nil
	})
	p := &Pipeline{Name: "p", Steps: []Step{
		{Name: "boom", Worker: "w", Prompt: "x", OnError: ErrorPolicy{Kind: ErrorFail}},
	}}
	st, err := Run(context.Background(), p, sp, bb)
	if err == nil {
		t.Fatal("expected pipeline error")
	}
	if st.State != StateFailed || st.FailedStep != "boom" {
		t.Fatalf("want failed at boom, got %+v", st)
	}
	if e, ok := bb.Get("pipeline.status"); !ok || !strings.Contains(fmt.Sprint(e.Value), "failed") {
		t.Fatalf("pipeline.status not failed: %+v ok=%v", e, ok)
	}
}

// TestEngineOnErrorFailCancelsInFlightSiblings: under on_error:fail, when one
// step fails fast, a slow in-flight sibling running on the shared ctx must
// observe cancellation rather than run to completion (FIX 3, change 0026). The
// two steps have no dependency, so they launch concurrently.
func TestEngineOnErrorFailCancelsInFlightSiblings(t *testing.T) {
	tree := agent.NewAgentTreeWithConcurrency("root", "m", 8)
	bb := agent.NewBlackboard(tree)

	var siblingCancelled int32
	sp := fakeSpawner(t, tree, func(opts agent.SpawnOpts) (string, error) {
		switch opts.Label {
		case "boom":
			// Fail fast, tripping the fail policy.
			return "", fmt.Errorf("kaboom")
		case "slow":
			return "", nil // unreachable; the child builder honors ctx below
		}
		return "ok", nil
	})
	// Rebuild the spawner so the slow sibling blocks on ctx and reports cancel.
	sp = agent.NewSpawner(
		agent.WithTree(tree),
		agent.WithNode(tree.Node(tree.RootID())),
		agent.WithSpawnDepth(0),
		agent.WithChildBuilder(func(ctx context.Context, opts agent.SpawnOpts, _ *agent.AgentNode, _ *agent.AgentTree) (string, error) {
			switch opts.Label {
			case "boom":
				// Give the slow sibling time to be in flight before failing.
				time.Sleep(20 * time.Millisecond)
				return "", fmt.Errorf("kaboom")
			case "slow":
				select {
				case <-ctx.Done():
					atomic.StoreInt32(&siblingCancelled, 1)
					return "", ctx.Err()
				case <-time.After(5 * time.Second):
					return "too-slow", nil
				}
			}
			return "ok", nil
		}),
	)

	p := &Pipeline{Name: "p", Steps: []Step{
		{Name: "boom", Worker: "w", Prompt: "x", OnError: ErrorPolicy{Kind: ErrorFail}},
		{Name: "slow", Worker: "w", Prompt: "y", OnError: ErrorPolicy{Kind: ErrorFail}},
	}}

	done := make(chan struct{})
	var st Status
	var err error
	go func() {
		st, err = Run(context.Background(), p, sp, bb)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return promptly after fail — in-flight sibling was not cancelled")
	}
	if err == nil || st.State != StateFailed {
		t.Fatalf("want failed pipeline, got st=%+v err=%v", st, err)
	}
	if atomic.LoadInt32(&siblingCancelled) != 1 {
		t.Fatal("slow in-flight sibling did not observe ctx cancellation after the fail")
	}
}

// TestEngineOnErrorSkip: a failing step with skip continues; its outputs are
// absent and the pipeline completes.
func TestEngineOnErrorSkip(t *testing.T) {
	tree := agent.NewAgentTree("root", "m")
	bb := agent.NewBlackboard(tree)
	sp := fakeSpawner(t, tree, func(opts agent.SpawnOpts) (string, error) {
		if opts.Label == "a" {
			return "", fmt.Errorf("fail a")
		}
		return "ok", nil
	})
	p := &Pipeline{Name: "p", Steps: []Step{
		{Name: "a", Worker: "w", Prompt: "x", Outputs: []string{"oa"}, OnError: ErrorPolicy{Kind: ErrorSkip}},
		{Name: "b", Worker: "w", Prompt: "y", Outputs: []string{"ob"}, DependsOn: []string{"a"}},
	}}
	st, err := Run(context.Background(), p, sp, bb)
	if err != nil {
		t.Fatalf("skip should not fail run: %v", err)
	}
	if st.State != StateCompleted {
		t.Fatalf("want completed, got %+v", st)
	}
	if _, ok := bb.Get("oa"); ok {
		t.Fatal("skipped step must not write outputs")
	}
	if _, ok := bb.Get("ob"); !ok {
		t.Fatal("downstream step should still run after skip")
	}
}

// TestEngineOnErrorRetry: a step failing then succeeding within retry budget
// completes; the spawn is attempted the expected number of times.
func TestEngineOnErrorRetry(t *testing.T) {
	tree := agent.NewAgentTree("root", "m")
	bb := agent.NewBlackboard(tree)
	var attempts int32
	sp := fakeSpawner(t, tree, func(opts agent.SpawnOpts) (string, error) {
		n := atomic.AddInt32(&attempts, 1)
		if n < 3 {
			return "", fmt.Errorf("flaky %d", n)
		}
		return "eventually", nil
	})
	p := &Pipeline{Name: "p", Steps: []Step{
		{Name: "s", Worker: "w", Prompt: "x", Outputs: []string{"o"}, OnError: ErrorPolicy{Kind: ErrorRetry, Retries: 3}},
	}}
	st, err := Run(context.Background(), p, sp, bb)
	if err != nil {
		t.Fatalf("retry-then-success should not fail: %v", err)
	}
	if st.State != StateCompleted {
		t.Fatalf("want completed, got %+v", st)
	}
	if atomic.LoadInt32(&attempts) != 3 {
		t.Fatalf("want 3 attempts, got %d", attempts)
	}
	if e, ok := bb.Get("o"); !ok || e.Value != "eventually" {
		t.Fatalf("output not written after retry: %+v", e)
	}
}

// TestEngineRetryExhausted: retries run out => pipeline fails naming the step.
func TestEngineRetryExhausted(t *testing.T) {
	tree := agent.NewAgentTree("root", "m")
	bb := agent.NewBlackboard(tree)
	var attempts int32
	sp := fakeSpawner(t, tree, func(opts agent.SpawnOpts) (string, error) {
		atomic.AddInt32(&attempts, 1)
		return "", fmt.Errorf("always")
	})
	p := &Pipeline{Name: "p", Steps: []Step{
		{Name: "s", Worker: "w", Prompt: "x", OnError: ErrorPolicy{Kind: ErrorRetry, Retries: 2}},
	}}
	st, err := Run(context.Background(), p, sp, bb)
	if err == nil {
		t.Fatal("exhausted retries should fail")
	}
	if st.FailedStep != "s" {
		t.Fatalf("want failed step s, got %+v", st)
	}
	// initial + 2 retries = 3 attempts.
	if atomic.LoadInt32(&attempts) != 3 {
		t.Fatalf("want 3 attempts (1+2 retries), got %d", attempts)
	}
}

// TestEngineExpectsStructured: a step with an expects schema whose child returns
// conforming JSON lands the STRUCTURED value on the output key.
func TestEngineExpectsStructured(t *testing.T) {
	tree := agent.NewAgentTree("root", "m")
	bb := agent.NewBlackboard(tree)
	sp := fakeSpawner(t, tree, func(opts agent.SpawnOpts) (string, error) {
		return `{"answer":42}`, nil
	})
	p := &Pipeline{Name: "p", Steps: []Step{
		{Name: "s", Worker: "w", Prompt: "x", Outputs: []string{"o"},
			Expects: []byte(`{"type":"object","properties":{"answer":{"type":"integer"}},"required":["answer"]}`)},
	}}
	if _, err := Run(context.Background(), p, sp, bb); err != nil {
		t.Fatalf("run: %v", err)
	}
	e, ok := bb.Get("o")
	if !ok {
		t.Fatal("output not written")
	}
	m, ok := e.Value.(map[string]any)
	if !ok {
		t.Fatalf("expected structured map, got %T: %#v", e.Value, e.Value)
	}
	if m["answer"] != float64(42) {
		t.Fatalf("structured value wrong: %#v", m)
	}
}

// TestEngineExpectsMismatchDegrades: a mismatching result degrades to raw text on
// the output key WITHOUT failing the step.
func TestEngineExpectsMismatchDegrades(t *testing.T) {
	tree := agent.NewAgentTree("root", "m")
	bb := agent.NewBlackboard(tree)
	sp := fakeSpawner(t, tree, func(opts agent.SpawnOpts) (string, error) {
		return `{"answer":"not-an-int"}`, nil
	})
	p := &Pipeline{Name: "p", Steps: []Step{
		{Name: "s", Worker: "w", Prompt: "x", Outputs: []string{"o"},
			Expects: []byte(`{"type":"object","properties":{"answer":{"type":"integer"}},"required":["answer"]}`)},
	}}
	st, err := Run(context.Background(), p, sp, bb)
	if err != nil {
		t.Fatalf("mismatch must not fail run: %v", err)
	}
	if st.State != StateCompleted {
		t.Fatalf("want completed, got %+v", st)
	}
	e, ok := bb.Get("o")
	if !ok {
		t.Fatal("output not written on degrade")
	}
	s, ok := e.Value.(string)
	if !ok || !strings.Contains(s, "not-an-int") {
		t.Fatalf("degrade should keep raw text, got %T %#v", e.Value, e.Value)
	}
}

// TestEngineDeadlockShapeUnderTightSlots: with the scheduler slot cap equal to
// the total fanout, a fanout step must still complete. This regresses
// slot-cap-yield-while-blocked-on-children: the coordinator must not itself hold
// a scheduler slot while awaiting the step's child spawns.
func TestEngineDeadlockShapeUnderTightSlots(t *testing.T) {
	// Cap slots at 2; two fanout steps of 2 each run in sequence (dep chain), so
	// the coordinator awaiting one step's 2 children must not consume a slot.
	tree := agent.NewAgentTreeWithConcurrency("root", "m", 2)
	bb := agent.NewBlackboard(tree)
	sp := fakeSpawner(t, tree, func(opts agent.SpawnOpts) (string, error) {
		time.Sleep(10 * time.Millisecond)
		return "x", nil
	})
	p := &Pipeline{Name: "p", Steps: []Step{
		{Name: "a", Worker: "w", Prompt: "go", Outputs: []string{"a/*"}, Fanout: 2},
		{Name: "b", Worker: "w", Prompt: "go", Outputs: []string{"b/*"}, Fanout: 2, DependsOn: []string{"a"}},
	}}
	done := make(chan struct{})
	var st Status
	var err error
	go func() {
		st, err = Run(context.Background(), p, sp, bb)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("pipeline deadlocked under tight slot cap")
	}
	if err != nil || st.State != StateCompleted {
		t.Fatalf("want completed, got st=%+v err=%v", st, err)
	}
	if len(bb.Keys("a/*")) != 2 || len(bb.Keys("b/*")) != 2 {
		t.Fatalf("fanout outputs missing: a=%v b=%v", bb.Keys("a/*"), bb.Keys("b/*"))
	}
}
