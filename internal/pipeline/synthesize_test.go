package pipeline

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ethanhinson/fuse/internal/agent"
)

// scriptedSpawner returns a Spawner whose child builder replays replies in order,
// one per synthesis attempt. The last reply repeats if attempts exceed the script.
func scriptedSpawner(t *testing.T, replies []string, sawPrompt *string, calls *int32) *agent.Spawner {
	t.Helper()
	tree := agent.NewAgentTree("root", "m")
	root := tree.Node(tree.RootID())
	var idx int32
	return agent.NewSpawner(
		agent.WithTree(tree),
		agent.WithNode(root),
		agent.WithSpawnDepth(0),
		agent.WithChildBuilder(func(ctx context.Context, opts agent.SpawnOpts, _ *agent.AgentNode, _ *agent.AgentTree) (string, error) {
			if calls != nil {
				atomic.AddInt32(calls, 1)
			}
			if sawPrompt != nil {
				*sawPrompt = opts.SystemPrompt + "\n---TASK---\n" + opts.Task
			}
			i := int(atomic.AddInt32(&idx, 1)) - 1
			if i >= len(replies) {
				i = len(replies) - 1
			}
			return replies[i], nil
		}),
	)
}

const goodPipelineJSON = `{
  "name": "synth",
  "workflow": "pool-a",
  "steps": [
    {"name":"a","worker":"searcher","prompt":"go"},
    {"name":"b","worker":"ranker","prompt":"rank","depends_on":["a"]}
  ]
}`

func TestSynthesizeGood(t *testing.T) {
	sp := scriptedSpawner(t, []string{goodPipelineJSON}, nil, nil)
	sc := SynthContext{Workers: []string{"searcher", "ranker"}, Tools: []string{"web_search"}}
	p, err := Synthesize(context.Background(), "research otters", sc, sp, Caps{})
	if err != nil {
		t.Fatalf("synthesize: %v", err)
	}
	if p.Name != "synth" || len(p.Steps) != 2 {
		t.Fatalf("bad pipeline: %+v", p)
	}
	// It must be runnable (valid).
	if err := Validate(p, Caps{}); err != nil {
		t.Fatalf("synthesized pipeline invalid: %v", err)
	}
}

func TestSynthesizePromptIncludesContext(t *testing.T) {
	var seen string
	sp := scriptedSpawner(t, []string{goodPipelineJSON}, &seen, nil)
	sc := SynthContext{Workers: []string{"searcher", "ranker"}, Tools: []string{"web_search"}}
	if _, err := Synthesize(context.Background(), "MY-GOAL-TOKEN", sc, sp, Caps{}); err != nil {
		t.Fatalf("synthesize: %v", err)
	}
	for _, want := range []string{"MY-GOAL-TOKEN", "searcher", "ranker", "web_search"} {
		if !strings.Contains(seen, want) {
			t.Errorf("synthesis prompt missing %q; got:\n%s", want, seen)
		}
	}
}

func TestSynthesizeReasksOnInvalidThenSucceeds(t *testing.T) {
	// First reply has a dangling depends_on; second reply is good.
	bad := `{"name":"x","steps":[{"name":"a","depends_on":["ghost"]}]}`
	var seen string
	var calls int32
	sp := scriptedSpawner(t, []string{bad, goodPipelineJSON}, &seen, &calls)
	p, err := Synthesize(context.Background(), "goal", SynthContext{}, sp, Caps{MaxAttempts: 3})
	if err != nil {
		t.Fatalf("should recover on second attempt: %v", err)
	}
	if p.Name != "synth" {
		t.Fatalf("expected recovered pipeline, got %+v", p)
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("want 2 attempts, got %d", calls)
	}
	// The re-ask must feed the validation error back (the second prompt sees it).
	if !strings.Contains(seen, "ghost") {
		t.Errorf("re-ask did not feed back the error; last prompt:\n%s", seen)
	}
}

func TestSynthesizeFailsLoudlyAtCap(t *testing.T) {
	bad := `{"name":"x","steps":[{"name":"a","depends_on":["ghost"]}]}`
	var calls int32
	sp := scriptedSpawner(t, []string{bad}, nil, &calls)
	_, err := Synthesize(context.Background(), "goal", SynthContext{}, sp, Caps{MaxAttempts: 2})
	if err == nil {
		t.Fatal("expected loud failure at attempt cap")
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("want exactly 2 attempts, got %d", calls)
	}
}

func TestSynthesizeRejectsCyclicReply(t *testing.T) {
	cyclic := `{"name":"x","steps":[{"name":"a","depends_on":["b"]},{"name":"b","depends_on":["a"]}]}`
	sp := scriptedSpawner(t, []string{cyclic}, nil, nil)
	if _, err := Synthesize(context.Background(), "goal", SynthContext{}, sp, Caps{MaxAttempts: 1}); err == nil {
		t.Fatal("cyclic synthesized DAG must be rejected")
	}
}

func TestSynthesizeRejectsOverCap(t *testing.T) {
	// A well-formed but over-MaxSteps reply must be rejected, never returned.
	over := `{"name":"x","steps":[{"name":"a"},{"name":"b"},{"name":"c"}]}`
	sp := scriptedSpawner(t, []string{over}, nil, nil)
	if _, err := Synthesize(context.Background(), "goal", SynthContext{}, sp, Caps{MaxSteps: 2, MaxAttempts: 1}); err == nil {
		t.Fatal("over-cap synthesized DAG must be rejected")
	}
}

func TestSynthesizeMalformedReplyReasks(t *testing.T) {
	sp := scriptedSpawner(t, []string{"not json at all", goodPipelineJSON}, nil, nil)
	p, err := Synthesize(context.Background(), "goal", SynthContext{}, sp, Caps{MaxAttempts: 3})
	if err != nil {
		t.Fatalf("should recover after malformed reply: %v", err)
	}
	if p.Name != "synth" {
		t.Fatalf("expected recovered pipeline, got %+v", p)
	}
}
