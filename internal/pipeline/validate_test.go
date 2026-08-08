package pipeline

import (
	"strings"
	"testing"
)

func step(name string, deps ...string) Step {
	return Step{Name: name, Worker: "w", DependsOn: deps}
}

func TestValidateOK(t *testing.T) {
	p := &Pipeline{Name: "p", Steps: []Step{
		step("a"),
		step("b", "a"),
		step("c", "a"),
		step("d", "b", "c"), // diamond
	}}
	if err := Validate(p, Caps{}); err != nil {
		t.Fatalf("valid diamond rejected: %v", err)
	}
}

func TestValidateDuplicateNames(t *testing.T) {
	p := &Pipeline{Steps: []Step{step("a"), step("a")}}
	err := Validate(p, Caps{})
	if err == nil || !strings.Contains(err.Error(), "a") {
		t.Fatalf("want duplicate-name error naming 'a', got %v", err)
	}
}

func TestValidateDanglingDependsOn(t *testing.T) {
	p := &Pipeline{Steps: []Step{step("a", "ghost")}}
	err := Validate(p, Caps{})
	if err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("want dangling depends_on error, got %v", err)
	}
}

func TestValidateDanglingGoto(t *testing.T) {
	p := &Pipeline{Steps: []Step{
		{Name: "a", Conditions: []Condition{{Key: "k", Op: "exists", Goto: "nowhere"}}},
	}}
	err := Validate(p, Caps{})
	if err == nil || !strings.Contains(err.Error(), "nowhere") {
		t.Fatalf("want dangling goto error, got %v", err)
	}
}

func TestValidateDanglingDefault(t *testing.T) {
	p := &Pipeline{Steps: []Step{{Name: "a", Default: "gone"}}}
	err := Validate(p, Caps{})
	if err == nil || !strings.Contains(err.Error(), "gone") {
		t.Fatalf("want dangling default error, got %v", err)
	}
}

func TestValidateBadOp(t *testing.T) {
	p := &Pipeline{Steps: []Step{
		{Name: "a", Conditions: []Condition{{Key: "k", Op: "wat", Goto: "a"}}},
	}}
	err := Validate(p, Caps{})
	if err == nil || !strings.Contains(err.Error(), "wat") {
		t.Fatalf("want bad-op error, got %v", err)
	}
}

func TestValidateGoodOps(t *testing.T) {
	for _, op := range []string{"exists", "eq", "ne", "gt", "lt", "contains", "matches"} {
		p := &Pipeline{Steps: []Step{
			{Name: "a", Conditions: []Condition{{Key: "k", Op: op, Goto: "b"}}},
			{Name: "b"},
		}}
		if err := Validate(p, Caps{}); err != nil {
			t.Errorf("op %q rejected: %v", op, err)
		}
	}
}

func TestValidateSelfLoop(t *testing.T) {
	p := &Pipeline{Steps: []Step{step("a", "a")}}
	if err := Validate(p, Caps{}); err == nil {
		t.Fatal("self-loop should be rejected")
	}
}

func TestValidateTwoCycle(t *testing.T) {
	p := &Pipeline{Steps: []Step{step("a", "b"), step("b", "a")}}
	err := Validate(p, Caps{})
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("want 2-cycle error, got %v", err)
	}
}

func TestValidateThreeCycle(t *testing.T) {
	p := &Pipeline{Steps: []Step{step("a", "c"), step("b", "a"), step("c", "b")}}
	if err := Validate(p, Caps{}); err == nil {
		t.Fatal("3-cycle should be rejected")
	}
}

func TestValidateGotoCycle(t *testing.T) {
	// a --goto--> b --goto--> a
	p := &Pipeline{Steps: []Step{
		{Name: "a", Conditions: []Condition{{Key: "k", Op: "exists", Goto: "b"}}},
		{Name: "b", Default: "a"},
	}}
	if err := Validate(p, Caps{}); err == nil {
		t.Fatal("goto cycle should be rejected")
	}
}

func TestValidateCapsMaxSteps(t *testing.T) {
	p := &Pipeline{Steps: []Step{step("a"), step("b"), step("c")}}
	if err := Validate(p, Caps{MaxSteps: 2}); err == nil {
		t.Fatal("over MaxSteps should be rejected")
	}
	if err := Validate(p, Caps{MaxSteps: 3}); err != nil {
		t.Fatalf("at MaxSteps should pass: %v", err)
	}
	// zero => skipped
	if err := Validate(p, Caps{MaxSteps: 0}); err != nil {
		t.Fatalf("MaxSteps 0 must be skipped: %v", err)
	}
}

func TestValidateCapsMaxFanout(t *testing.T) {
	p := &Pipeline{Steps: []Step{{Name: "a", Fanout: 5}}}
	err := Validate(p, Caps{MaxFanout: 3})
	if err == nil || !strings.Contains(err.Error(), "a") {
		t.Fatalf("over MaxFanout should name step 'a', got %v", err)
	}
	if err := Validate(p, Caps{MaxFanout: 5}); err != nil {
		t.Fatalf("at MaxFanout should pass: %v", err)
	}
}

func TestValidateCapsMaxDepth(t *testing.T) {
	// chain a->b->c->d has longest depends_on path length 4 (4 steps in the chain).
	p := &Pipeline{Steps: []Step{step("a"), step("b", "a"), step("c", "b"), step("d", "c")}}
	if err := Validate(p, Caps{MaxDepth: 3}); err == nil {
		t.Fatal("over MaxDepth should be rejected")
	}
	if err := Validate(p, Caps{MaxDepth: 4}); err != nil {
		t.Fatalf("at MaxDepth should pass: %v", err)
	}
}

func TestValidateCapsRequirePool(t *testing.T) {
	p := &Pipeline{Steps: []Step{step("a")}}
	if err := Validate(p, Caps{RequirePool: true}); err == nil {
		t.Fatal("missing pool should be rejected when RequirePool")
	}
	p.Workflow = "pool-a"
	if err := Validate(p, Caps{RequirePool: true}); err != nil {
		t.Fatalf("pool set should pass: %v", err)
	}
}
