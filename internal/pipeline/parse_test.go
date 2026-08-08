package pipeline

import (
	"encoding/json"
	"strings"
	"testing"
)

const fullYAML = `
name: research
workflow: pool-a
steps:
  - name: gather
    worker: searcher
    prompt: "find {{topic}}"
    inputs: [topic]
    outputs: [hits/*]
    fanout: 3
    on_error: retry(2)
    expects: {"type":"object"}
  - name: rank
    worker: ranker
    prompt: "rank the hits"
    inputs: [hits/*]
    outputs: [ranked]
    depends_on: [gather]
    on_error: skip
    conditions:
      - if: {key: ranked, op: exists, value: null}
        goto: report
    default: stop
  - name: report
    worker: writer
    prompt: "write it up"
    depends_on: [rank]
  - name: stop
    worker: writer
    prompt: "abort"
`

func TestParseFullYAML(t *testing.T) {
	p, err := Parse([]byte(fullYAML))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if p.Name != "research" || p.Workflow != "pool-a" {
		t.Fatalf("header wrong: %+v", p)
	}
	if len(p.Steps) != 4 {
		t.Fatalf("want 4 steps, got %d", len(p.Steps))
	}
	g := p.Steps[0]
	if g.Name != "gather" || g.Worker != "searcher" || g.Prompt != "find {{topic}}" {
		t.Fatalf("gather fields: %+v", g)
	}
	if g.Fanout != 3 {
		t.Fatalf("fanout: %d", g.Fanout)
	}
	if len(g.Inputs) != 1 || g.Inputs[0] != "topic" || len(g.Outputs) != 1 || g.Outputs[0] != "hits/*" {
		t.Fatalf("io: %+v", g)
	}
	if g.OnError.Kind != ErrorRetry || g.OnError.Retries != 2 {
		t.Fatalf("on_error: %+v", g.OnError)
	}
	if len(g.Expects) == 0 || !strings.Contains(string(g.Expects), "object") {
		t.Fatalf("expects not preserved: %s", g.Expects)
	}
	r := p.Steps[1]
	if r.OnError.Kind != ErrorSkip {
		t.Fatalf("rank on_error: %+v", r.OnError)
	}
	if len(r.DependsOn) != 1 || r.DependsOn[0] != "gather" {
		t.Fatalf("depends_on: %+v", r.DependsOn)
	}
	if len(r.Conditions) != 1 {
		t.Fatalf("want 1 condition, got %d", len(r.Conditions))
	}
	c := r.Conditions[0]
	if c.Key != "ranked" || c.Op != "exists" || c.Goto != "report" {
		t.Fatalf("condition: %+v", c)
	}
	if r.Default != "stop" {
		t.Fatalf("default: %q", r.Default)
	}
}

func TestParseJSONParity(t *testing.T) {
	py, err := Parse([]byte(fullYAML))
	if err != nil {
		t.Fatalf("yaml parse: %v", err)
	}
	// Marshal the YAML-parsed pipeline to JSON, then parse it back.
	blob, err := json.Marshal(py)
	if err != nil {
		t.Fatalf("marshal json: %v", err)
	}
	pj, err := Parse(blob)
	if err != nil {
		t.Fatalf("json parse: %v", err)
	}
	if pj.Name != py.Name || len(pj.Steps) != len(py.Steps) {
		t.Fatalf("json parity mismatch: %+v vs %+v", pj, py)
	}
	if pj.Steps[0].OnError != py.Steps[0].OnError {
		t.Fatalf("on_error parity: %+v vs %+v", pj.Steps[0].OnError, py.Steps[0].OnError)
	}
	if pj.Steps[1].Conditions[0].Op != "exists" {
		t.Fatalf("condition parity: %+v", pj.Steps[1].Conditions[0])
	}
}

func TestParseOnErrorForms(t *testing.T) {
	cases := map[string]ErrorPolicy{
		"fail":     {Kind: ErrorFail},
		"skip":     {Kind: ErrorSkip},
		"retry(3)": {Kind: ErrorRetry, Retries: 3},
	}
	for scalar, want := range cases {
		y := "name: p\nsteps:\n  - name: s\n    on_error: " + scalar + "\n"
		p, err := Parse([]byte(y))
		if err != nil {
			t.Fatalf("%q: parse: %v", scalar, err)
		}
		if p.Steps[0].OnError != want {
			t.Errorf("%q => %+v, want %+v", scalar, p.Steps[0].OnError, want)
		}
	}
}

func TestParseOnErrorMalformed(t *testing.T) {
	for _, bad := range []string{"retry(0)", "retry()", "retry(-1)", "retry(abc)", "explode", "retry"} {
		y := "name: p\nsteps:\n  - name: broken\n    on_error: " + bad + "\n"
		_, err := Parse([]byte(y))
		if err == nil {
			t.Errorf("%q: expected parse error", bad)
			continue
		}
		if !strings.Contains(err.Error(), "broken") {
			t.Errorf("%q: error should name step 'broken', got: %v", bad, err)
		}
	}
}

// TestParseColonInPrompt guards the YAML plain-scalar colon trap: a prompt with
// ": " must parse (quoted here, which is the realistic authored form).
func TestParseColonInPrompt(t *testing.T) {
	y := `
name: p
steps:
  - name: s
    prompt: "Step one: do the thing, then: stop"
`
	p, err := Parse([]byte(y))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if p.Steps[0].Prompt != "Step one: do the thing, then: stop" {
		t.Fatalf("prompt mangled: %q", p.Steps[0].Prompt)
	}
}

func TestParseEmptyOnErrorDefaultsFail(t *testing.T) {
	y := "name: p\nsteps:\n  - name: s\n    prompt: hi\n"
	p, err := Parse([]byte(y))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if p.Steps[0].OnError.Kind != ErrorFail {
		t.Fatalf("absent on_error should default to fail, got %+v", p.Steps[0].OnError)
	}
}
