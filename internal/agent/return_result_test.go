package agent

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/tools"
)

// --- Task 1: return_result tool schema synthesis ------------------------------

// TestReturnResultSchema_EqualsExpects verifies the synthesized return_result
// tool's parameters deep-equal the expects schema, its name is "return_result",
// and its description names the tool and "once".
func TestReturnResultSchema_EqualsExpects(t *testing.T) {
	expects := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"novelty": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		},
		"required": []any{"novelty"},
	}
	sch := returnResultSchema(expects)
	if !reflect.DeepEqual(sch, expects) {
		t.Fatalf("return_result parameters must deep-equal the expects schema;\n got %#v\nwant %#v", sch, expects)
	}
	if returnResultToolName != "return_result" {
		t.Fatalf("tool name = %q, want return_result", returnResultToolName)
	}
	desc := returnResultDescription()
	if !strings.Contains(desc, "return_result") {
		t.Errorf("description must name return_result; got %q", desc)
	}
	if !strings.Contains(strings.ToLower(desc), "once") {
		t.Errorf("description must say to call it once; got %q", desc)
	}
}

// TestReturnResultHint_NamesToolNotDirective verifies the prompt hint names
// return_result and does NOT carry the contradictory "final message MUST be a
// single JSON object" directive.
func TestReturnResultHint_NamesToolNotDirective(t *testing.T) {
	schema := map[string]any{"type": "object"}
	hint := returnResultHint(schema)
	if !strings.Contains(hint, "return_result") {
		t.Errorf("hint must name return_result; got %q", hint)
	}
	if strings.Contains(hint, "final message MUST be a single JSON object") {
		t.Errorf("hint must NOT contain the message-channel directive; got %q", hint)
	}
}

// --- Task 2: Agent-side expects state + capture seam --------------------------

// TestRunOffersReturnResultWhenExpectsSet verifies the loop advertises a
// return_result tool (whose parameters equal the schema) when expects is set,
// and does NOT when it is unset (regression).
func TestRunOffersReturnResultWhenExpectsSet(t *testing.T) {
	schema := map[string]any{
		"type":       "object",
		"properties": map[string]any{"n": map[string]any{"type": "integer"}},
	}

	t.Run("offered when set", func(t *testing.T) {
		comp := &capturingToolsCompleter{}
		a := New(comp, &schemaExec{}, nopRenderer{}, "m", "", 10, 100)
		var sink ExpectsSink
		a.SetExpects(schema, &sink)
		if _, err := a.Run(context.Background(), []model.Message{{Role: "user", Content: "hi"}}); err != nil {
			t.Fatal(err)
		}
		if !containsName(comp.gotTools, "return_result") {
			t.Fatalf("return_result should be offered; got %v", toolNames(comp.gotTools))
		}
		for _, ts := range comp.gotTools {
			if ts.Name == "return_result" {
				if !reflect.DeepEqual(ts.Parameters, schema) {
					t.Errorf("return_result parameters must equal the schema; got %#v", ts.Parameters)
				}
			}
		}
		// The registry's own tools must still be offered alongside it.
		if !containsName(comp.gotTools, "bash") {
			t.Errorf("registry tools must remain; got %v", toolNames(comp.gotTools))
		}
	})

	t.Run("absent when unset", func(t *testing.T) {
		comp := &capturingToolsCompleter{}
		a := New(comp, &schemaExec{}, nopRenderer{}, "m", "", 10, 100)
		if _, err := a.Run(context.Background(), []model.Message{{Role: "user", Content: "hi"}}); err != nil {
			t.Fatal(err)
		}
		if containsName(comp.gotTools, "return_result") {
			t.Fatalf("return_result must not leak when expects unset; got %v", toolNames(comp.gotTools))
		}
	})
}

// --- Task 4: terminal handling: valid return_result ends the run -------------

// TestReturnResultTerminal_CapturesAndEnds verifies a conforming return_result
// call ends the run, captures the value into the sink, and requires no further
// assistant message.
func TestReturnResultTerminal_CapturesAndEnds(t *testing.T) {
	schema := map[string]any{
		"type":       "object",
		"properties": map[string]any{"name": map[string]any{"type": "string"}},
		"required":   []any{"name"},
	}
	comp := &scriptedCompleter{responses: []model.CompletionResp{
		{ToolCalls: []model.ToolCall{{ID: "1", Name: "return_result", Arguments: `{"name":"Ada"}`}}},
		// A second response exists but must NOT be consumed — the run ends on the
		// valid return_result call above.
		{Content: "should not be reached"},
	}}
	exec := &fakeExec{}
	a := New(comp, exec, nopRenderer{}, "m", "", 10, 100)
	var sink ExpectsSink
	a.SetExpects(schema, &sink)

	if _, err := a.Run(context.Background(), []model.Message{{Role: "user", Content: "go"}}); err != nil {
		t.Fatalf("run should succeed: %v", err)
	}
	if comp.i != 1 {
		t.Fatalf("run must end on the valid return_result (consumed %d responses, want 1)", comp.i)
	}
	if !sink.Captured() {
		t.Fatal("sink must capture the structured value")
	}
	m, ok := sink.Value().(map[string]any)
	if !ok || m["name"] != "Ada" {
		t.Fatalf("captured value wrong: %#v", sink.Value())
	}
	// return_result must not be dispatched to the underlying registry.
	for _, c := range exec.calls {
		if c == "return_result" {
			t.Errorf("return_result must be handled by the loop, not the registry")
		}
	}
}

// --- Task 5: self-repair loop: invalid then valid ---------------------------

// TestReturnResultRepairThenSuccess verifies a non-conforming return_result
// yields the validation error as the tool result, the child retries, and a
// following conforming call succeeds. Bounded; never hard-fails.
func TestReturnResultRepairThenSuccess(t *testing.T) {
	schema := map[string]any{
		"type":       "object",
		"properties": map[string]any{"n": map[string]any{"type": "integer"}},
		"required":   []any{"n"},
	}
	comp := &scriptedCompleter{responses: []model.CompletionResp{
		{ToolCalls: []model.ToolCall{{ID: "1", Name: "return_result", Arguments: `{"n":"not-an-int"}`}}},
		{ToolCalls: []model.ToolCall{{ID: "2", Name: "return_result", Arguments: `{"n":42}`}}},
	}}
	a := New(comp, &fakeExec{}, nopRenderer{}, "m", "", 10, 100)
	var sink ExpectsSink
	a.SetExpects(schema, &sink)

	hist, err := a.Run(context.Background(), []model.Message{{Role: "user", Content: "go"}})
	if err != nil {
		t.Fatalf("repair-then-success must not error: %v", err)
	}
	if !sink.Captured() {
		t.Fatal("second (valid) return_result must be captured")
	}
	m := sink.Value().(map[string]any)
	if got, _ := m["n"].(float64); got != 42 {
		t.Fatalf("captured n = %v, want 42", m["n"])
	}
	// The first invalid call must have produced a tool result carrying the
	// validation error so the model could self-correct.
	foundErr := false
	for _, msg := range hist {
		if msg.Role == "tool" && msg.Name == "return_result" && strings.Contains(msg.Content, "integer") {
			foundErr = true
		}
	}
	if !foundErr {
		t.Errorf("invalid return_result must return a validation-error tool result; history=%v", hist)
	}
}

// --- Task 6: exhaustion → no structured result ------------------------------

// TestReturnResultExhaustion verifies persistent non-conforming calls hit the
// retry cap, end the run with no captured value, and never hard-fail. Args vary
// each attempt so the doom-loop detector (identical-call) is not what stops it —
// the return_result retry cap is; documented in the loop.
func TestReturnResultExhaustion(t *testing.T) {
	schema := map[string]any{
		"type":       "object",
		"properties": map[string]any{"n": map[string]any{"type": "integer"}},
		"required":   []any{"n"},
	}
	comp := &scriptedCompleter{responses: []model.CompletionResp{
		{ToolCalls: []model.ToolCall{{ID: "1", Name: "return_result", Arguments: `{"n":"a"}`}}},
		{ToolCalls: []model.ToolCall{{ID: "2", Name: "return_result", Arguments: `{"n":"bb"}`}}},
		{ToolCalls: []model.ToolCall{{ID: "3", Name: "return_result", Arguments: `{"n":"ccc"}`}}},
		{ToolCalls: []model.ToolCall{{ID: "4", Name: "return_result", Arguments: `{"n":"dddd"}`}}},
	}}
	a := New(comp, &fakeExec{}, nopRenderer{}, "m", "", 20, 100)
	var sink ExpectsSink
	a.SetExpects(schema, &sink)

	if _, err := a.Run(context.Background(), []model.Message{{Role: "user", Content: "go"}}); err != nil {
		t.Fatalf("exhaustion must complete without a hard error; got %v", err)
	}
	if sink.Captured() {
		t.Fatalf("no value should be captured on exhaustion; got %#v", sink.Value())
	}
}

// --- Task 7: REGRESSION — write_file + return_result coexist -----------------

// recordingToolExec records tool-call args by name and offers a write_file
// schema. It stands in for a real working tool the child uses alongside
// return_result.
type recordingToolExec struct {
	byName map[string]string // last args seen per tool name
}

func (r *recordingToolExec) Schemas() []model.ToolSchema {
	return []model.ToolSchema{
		{Name: "write_file", Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":    map[string]any{"type": "string"},
				"content": map[string]any{"type": "string"},
			},
			"required": []any{"path", "content"},
		}},
	}
}

func (r *recordingToolExec) Execute(ctx context.Context, name, args string) tools.Result {
	if r.byName == nil {
		r.byName = map[string]string{}
	}
	r.byName[name] = args
	return tools.Result{Output: "ok"}
}

// TestRegression_WriteFileAndReturnResultCoexist is the direct guard for the
// production bug: a child with expects that ALSO uses write_file produces a
// well-formed write_file{path,content} (path present, content a real file body,
// NOT the structured object) AND a separate return_result carrying the
// structured object — which is what populates Structured.
func TestRegression_WriteFileAndReturnResultCoexist(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"novelty": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		},
		"required": []any{"novelty"},
	}
	// Turn 1: do work via write_file (well-formed). Turn 2: return the verdict.
	comp := &scriptedCompleter{responses: []model.CompletionResp{
		{ToolCalls: []model.ToolCall{{
			ID:        "1",
			Name:      "write_file",
			Arguments: `{"path":"/tmp/report.md","content":"# Findings\nA real file body."}`,
		}}},
		{ToolCalls: []model.ToolCall{{
			ID:        "2",
			Name:      "return_result",
			Arguments: `{"novelty":["insight-1","insight-2"]}`,
		}}},
	}}
	exec := &recordingToolExec{}
	a := New(comp, exec, nopRenderer{}, "m", "", 10, 100)
	var sink ExpectsSink
	a.SetExpects(schema, &sink)

	if _, err := a.Run(context.Background(), []model.Message{{Role: "user", Content: "grade it"}}); err != nil {
		t.Fatalf("run: %v", err)
	}

	// (a) write_file received a well-formed object.
	wf, ok := exec.byName["write_file"]
	if !ok {
		t.Fatal("write_file was never dispatched")
	}
	var wfArgs map[string]any
	if err := json.Unmarshal([]byte(wf), &wfArgs); err != nil {
		t.Fatalf("write_file args not valid JSON: %v", err)
	}
	path, _ := wfArgs["path"].(string)
	if path == "" {
		t.Errorf("write_file must carry a non-empty path; got %#v", wfArgs)
	}
	content, _ := wfArgs["content"].(string)
	if content == "" {
		t.Errorf("write_file content must be a real file body; got %#v", wfArgs)
	}
	// (c) the structured object must NOT be crammed into write_file.content.
	if strings.Contains(content, "novelty") {
		t.Errorf("structured object leaked into write_file.content: %q", content)
	}
	if _, hasNovelty := wfArgs["novelty"]; hasNovelty {
		t.Errorf("structured object leaked into write_file args: %#v", wfArgs)
	}

	// (b) the structured object arrives via return_result → sink.
	if !sink.Captured() {
		t.Fatal("structured result must be captured from return_result")
	}
	m := sink.Value().(map[string]any)
	arr, ok := m["novelty"].([]any)
	if !ok || len(arr) != 2 {
		t.Fatalf("structured value wrong: %#v", sink.Value())
	}
}
