package tools

import (
	"context"
	"strings"
	"testing"
)

// Edge-case regressions for the blackboard tools found by the change-0023 audit:
// value shapes on write, and the exact timeout boundary on wait.

// TestBlackboardWriteValueShapes covers non-object JSON values the model may send
// and the JSON-null case: each must parse and store, distinct from an error.
func TestBlackboardWriteValueShapes(t *testing.T) {
	cases := []struct {
		name    string
		value   string // the JSON-string value field
		want    any
		wantNil bool
	}{
		{"number", `5`, float64(5), false},
		{"bare string", `\"hi\"`, "hi", false},
		{"bool", `true`, true, false},
		{"array", `[1,2]`, nil, false}, // checked structurally below
		{"json null", `null`, nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeStore()
			w := toolByName(NewBlackboardTools(f), "blackboard_write")
			r := w.Execute(context.Background(), `{"key":"k","value":"`+tc.value+`"}`)
			if r.IsError {
				t.Fatalf("value %q should write cleanly, got error: %q", tc.value, r.Output)
			}
			got, ok := f.values["k"]
			if !ok {
				t.Fatalf("value %q: key not written", tc.value)
			}
			if tc.wantNil {
				if got != nil {
					t.Fatalf("json null should store nil, got %#v", got)
				}
				return
			}
			if tc.name == "array" {
				arr, ok := got.([]any)
				if !ok || len(arr) != 2 {
					t.Fatalf("array not parsed: %#v", got)
				}
				return
			}
			if got != tc.want {
				t.Fatalf("value %q => %#v, want %#v", tc.value, got, tc.want)
			}
		})
	}
}

// TestBlackboardWriteEmptyValueIsError covers an empty value string: it is not
// valid JSON, so the tool must report an error rather than store garbage.
func TestBlackboardWriteEmptyValueIsError(t *testing.T) {
	f := newFakeStore()
	w := toolByName(NewBlackboardTools(f), "blackboard_write")
	if r := w.Execute(context.Background(), `{"key":"k","value":""}`); !r.IsError {
		t.Fatal("empty value string should be a tool error (not valid JSON)")
	}
}

// TestBlackboardWaitTimeoutBoundary pins the ceiling boundary: exactly the max is
// allowed; just over it is rejected. (Below-ceiling positive values are covered
// elsewhere.)
func TestBlackboardWaitTimeoutBoundary(t *testing.T) {
	f := newFakeStore()
	f.waitValue = "ok"
	wt := toolByName(NewBlackboardTools(f), "blackboard_wait")

	// Exactly the ceiling is accepted.
	if r := wt.Execute(context.Background(), `{"key":"k","timeout":300}`); r.IsError {
		t.Fatalf("timeout == ceiling (%d) should be accepted, got: %q", maxWaitSeconds, r.Output)
	}
	// Just over the ceiling is rejected.
	if r := wt.Execute(context.Background(), `{"key":"k","timeout":300.001}`); !r.IsError {
		t.Fatal("timeout just over the ceiling should be a tool error")
	}
	if r := wt.Execute(context.Background(), `{"key":"k","timeout":300.001}`); !strings.Contains(r.Output, "ceiling") {
		t.Fatalf("over-ceiling error should mention the ceiling: %q", r.Output)
	}
}

// TestBlackboardWaitTimeoutWrongType (second-pass audit BB-T1): a timeout sent as
// a JSON string ("5" not 5) fails json.Unmarshal into the float64 field and is
// reported as an 'invalid args' tool error — not coerced, not a panic.
func TestBlackboardWaitTimeoutWrongType(t *testing.T) {
	f := newFakeStore()
	wt := toolByName(NewBlackboardTools(f), "blackboard_wait")
	r := wt.Execute(context.Background(), `{"key":"k","timeout":"5"}`)
	if !r.IsError {
		t.Fatal("string timeout should be a tool error")
	}
	if !strings.Contains(r.Output, "invalid args") {
		t.Fatalf("expected 'invalid args', got: %q", r.Output)
	}
}

// TestBlackboardKeyWrongType (second-pass audit BB-T8): a key of the wrong JSON
// type (number, not string) fails json.Unmarshal and yields the 'invalid args'
// error — distinct from the downstream 'key is required' empty-key message.
func TestBlackboardKeyWrongType(t *testing.T) {
	rd := toolByName(NewBlackboardTools(newFakeStore()), "blackboard_read")
	r := rd.Execute(context.Background(), `{"key":123}`)
	if !r.IsError {
		t.Fatal("numeric key should be a tool error")
	}
	if !strings.Contains(r.Output, "invalid args") {
		t.Fatalf("expected 'invalid args' (not 'key is required'), got: %q", r.Output)
	}
}

// TestBlackboardKeysMalformedGlobViaTool confirms the tool surfaces a malformed
// glob as an empty match (delegated to the store's path.Match contract), not an
// error — the tool just marshals whatever the store returns.
func TestBlackboardKeysMalformedGlobViaTool(t *testing.T) {
	f := newFakeStore()
	f.values["a"] = 1
	kt := toolByName(NewBlackboardTools(f), "blackboard_keys")
	r := kt.Execute(context.Background(), `{"pattern":"["}`)
	if r.IsError {
		t.Fatalf("malformed glob should not be a tool error: %q", r.Output)
	}
	if strings.TrimSpace(r.Output) != "[]" {
		t.Fatalf("malformed glob should match nothing, got %q", r.Output)
	}
}
