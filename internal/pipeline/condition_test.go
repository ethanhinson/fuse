package pipeline

import (
	"testing"

	"github.com/ethanhinson/fuse/internal/agent"
)

func bbWith(kv map[string]any) *agent.Blackboard {
	bb := agent.NewBlackboard(nil)
	for k, v := range kv {
		bb.Put(k, v, "w", "writer")
	}
	return bb
}

func TestEvalExists(t *testing.T) {
	bb := bbWith(map[string]any{"k": "v"})
	if !evalCondition(Condition{Key: "k", Op: "exists"}, bb) {
		t.Error("exists on present key should be true")
	}
	if evalCondition(Condition{Key: "missing", Op: "exists"}, bb) {
		t.Error("exists on missing key should be false")
	}
}

func TestEvalEqNe(t *testing.T) {
	bb := bbWith(map[string]any{"n": float64(5), "s": "hi"})
	if !evalCondition(Condition{Key: "n", Op: "eq", Value: float64(5)}, bb) {
		t.Error("eq numeric equal should be true")
	}
	if evalCondition(Condition{Key: "n", Op: "eq", Value: float64(6)}, bb) {
		t.Error("eq numeric unequal should be false")
	}
	if !evalCondition(Condition{Key: "s", Op: "ne", Value: "bye"}, bb) {
		t.Error("ne unequal strings should be true")
	}
	if evalCondition(Condition{Key: "s", Op: "ne", Value: "hi"}, bb) {
		t.Error("ne equal strings should be false")
	}
	// eq on missing key => false, never panics.
	if evalCondition(Condition{Key: "missing", Op: "eq", Value: 1}, bb) {
		t.Error("eq on missing key should be false")
	}
}

func TestEvalEqCrossNumericTypes(t *testing.T) {
	// JSON numbers decode to float64; an authored int Value should still compare
	// equal after numeric coercion.
	bb := bbWith(map[string]any{"n": float64(5)})
	if !evalCondition(Condition{Key: "n", Op: "eq", Value: 5}, bb) {
		t.Error("eq should coerce int Value to match float64 stored value")
	}
}

func TestEvalGtLt(t *testing.T) {
	bb := bbWith(map[string]any{"n": float64(10)})
	if !evalCondition(Condition{Key: "n", Op: "gt", Value: 5}, bb) {
		t.Error("10 gt 5 should be true")
	}
	if evalCondition(Condition{Key: "n", Op: "gt", Value: 20}, bb) {
		t.Error("10 gt 20 should be false")
	}
	if !evalCondition(Condition{Key: "n", Op: "lt", Value: 20}, bb) {
		t.Error("10 lt 20 should be true")
	}
	// non-numeric stored side => false.
	sbb := bbWith(map[string]any{"s": "abc"})
	if evalCondition(Condition{Key: "s", Op: "gt", Value: 1}, sbb) {
		t.Error("gt on non-numeric value should be false")
	}
	// non-numeric compare side => false.
	if evalCondition(Condition{Key: "n", Op: "gt", Value: "notnum"}, bb) {
		t.Error("gt with non-numeric Value should be false")
	}
}

func TestEvalContains(t *testing.T) {
	bb := bbWith(map[string]any{
		"s":   "hello world",
		"arr": []any{"a", "b", float64(3)},
	})
	if !evalCondition(Condition{Key: "s", Op: "contains", Value: "world"}, bb) {
		t.Error("string substring contains should be true")
	}
	if evalCondition(Condition{Key: "s", Op: "contains", Value: "xyz"}, bb) {
		t.Error("absent substring should be false")
	}
	if !evalCondition(Condition{Key: "arr", Op: "contains", Value: "b"}, bb) {
		t.Error("array membership should be true")
	}
	if evalCondition(Condition{Key: "arr", Op: "contains", Value: "z"}, bb) {
		t.Error("array non-membership should be false")
	}
	if !evalCondition(Condition{Key: "arr", Op: "contains", Value: 3}, bb) {
		t.Error("array numeric membership should coerce and match")
	}
}

func TestEvalMatches(t *testing.T) {
	bb := bbWith(map[string]any{"s": "error: boom"})
	if !evalCondition(Condition{Key: "s", Op: "matches", Value: "^error:"}, bb) {
		t.Error("regexp match should be true")
	}
	if evalCondition(Condition{Key: "s", Op: "matches", Value: "^ok"}, bb) {
		t.Error("non-match should be false")
	}
	// invalid regexp => false, never panics.
	if evalCondition(Condition{Key: "s", Op: "matches", Value: "(["}, bb) {
		t.Error("invalid regexp should be false")
	}
}

func TestEvalUnknownOpFalse(t *testing.T) {
	bb := bbWith(map[string]any{"k": "v"})
	if evalCondition(Condition{Key: "k", Op: "wat", Value: 1}, bb) {
		t.Error("unknown op should be false, never panic")
	}
}

func TestRouteFirstMatchWins(t *testing.T) {
	bb := bbWith(map[string]any{"a": true, "b": true})
	s := Step{
		Conditions: []Condition{
			{Key: "missing", Op: "exists", Goto: "x"},
			{Key: "a", Op: "exists", Goto: "y"},
			{Key: "b", Op: "exists", Goto: "z"},
		},
		Default: "d",
	}
	next, ok := route(s, bb)
	if !ok || next != "y" {
		t.Fatalf("first matching condition should win: got (%q,%v)", next, ok)
	}
}

func TestRouteFallsToDefault(t *testing.T) {
	bb := bbWith(map[string]any{})
	s := Step{
		Conditions: []Condition{{Key: "missing", Op: "exists", Goto: "x"}},
		Default:    "d",
	}
	next, ok := route(s, bb)
	if !ok || next != "d" {
		t.Fatalf("should fall to default: got (%q,%v)", next, ok)
	}
}

func TestRouteNoMatchNoDefault(t *testing.T) {
	bb := bbWith(map[string]any{})
	s := Step{Conditions: []Condition{{Key: "missing", Op: "exists", Goto: "x"}}}
	next, ok := route(s, bb)
	if ok || next != "" {
		t.Fatalf("no match + no default should be ('',false): got (%q,%v)", next, ok)
	}
}
