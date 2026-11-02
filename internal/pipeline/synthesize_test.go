package pipeline

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
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

var updateSynthGolden = flag.Bool("update-synth-golden", false, "rewrite the no-roles synthesis goldens")

// synthNoRolesContext is the palette the no-roles goldens are captured with.
var synthNoRolesContext = SynthContext{Workers: []string{"searcher", "ranker"}, Tools: []string{"read_file", "web_search"}}

func checkSynthGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *updateSynthGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (run with -update-synth-golden to create it): %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("%s changed:\n got: %q\nwant: %q", name, got, want)
	}
}

// TestSynthesisNoRolesGolden: with no roles, the synthesis system prompt and
// the Expects schema stay byte-identical to before roles.
func TestSynthesisNoRolesGolden(t *testing.T) {
	checkSynthGolden(t, "synth_prompt_noroles.golden", []byte(synthesisSystemPrompt(synthNoRolesContext)))
	b, err := json.MarshalIndent(pipelineJSONSchema(nil), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	checkSynthGolden(t, "synth_schema_noroles.golden", append(b, '\n'))
}

// TestSynthesisPromptListsRoles: with roles, the prompt lists each role as
// "name — description" (description on one line) and the schema description
// and Expects schema gain the role field.
func TestSynthesisPromptListsRoles(t *testing.T) {
	sc := synthNoRolesContext
	sc.Roles = []SynthRole{{Name: "reviewer", Description: "Reviews code\nfor bugs."}, {Name: "writer", Description: "Writes prose."}}
	got := synthesisSystemPrompt(sc)
	for _, want := range []string{
		"Available roles (set a step's role to one of these instead of worker):\n- reviewer — Reviews code for bugs.\n- writer — Writes prose.\n",
		`"role": string (one of the available roles; never together with worker),`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt lacks %q:\n%s", want, got)
		}
	}
	props := pipelineJSONSchema([]string{"reviewer", "writer"})["properties"].(map[string]any)["steps"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)
	if _, ok := props["role"]; !ok {
		t.Fatal("Expects schema lacks the role field with roles defined")
	}
}

// TestSynthesizeReasksOnUnknownRole: a synthesized step naming a role outside
// the palette is rejected and fed back; a corrected reply is accepted.
func TestSynthesizeReasksOnUnknownRole(t *testing.T) {
	bad := `{"name":"p","workflow":"w","steps":[{"name":"a","role":"ghost","prompt":"go"}]}`
	good := `{"name":"p","workflow":"w","steps":[{"name":"a","role":"reviewer","prompt":"go"}]}`
	var calls int32
	var saw string
	sp := scriptedSpawner(t, []string{bad, good}, &saw, &calls)
	sc := SynthContext{Roles: []SynthRole{{Name: "reviewer", Description: "Reviews."}}}
	p, err := Synthesize(context.Background(), "goal", sc, sp, Caps{})
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if p.Steps[0].Role != "reviewer" || atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("got role %q after %d calls, want reviewer after 2", p.Steps[0].Role, calls)
	}
	if !strings.Contains(saw, `unknown role "ghost" (available: reviewer)`) {
		t.Fatalf("re-ask did not feed back the unknown role: %q", saw)
	}
}

// TestSynthesizeNoRolesRefusesRoleStep: with no roles defined, a synthesized
// role step is an unknown role (re-asked, then a loud failure at the cap).
func TestSynthesizeNoRolesRefusesRoleStep(t *testing.T) {
	bad := `{"name":"p","workflow":"w","steps":[{"name":"a","role":"ghost","prompt":"go"}]}`
	sp := scriptedSpawner(t, []string{bad}, nil, nil)
	_, err := Synthesize(context.Background(), "goal", SynthContext{}, sp, Caps{MaxAttempts: 2})
	if err == nil || !strings.Contains(err.Error(), `unknown role "ghost" (available: none configured)`) {
		t.Fatalf("err = %v, want the unknown-role failure", err)
	}
}
