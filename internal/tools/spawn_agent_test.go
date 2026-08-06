package tools

import (
	"context"
	"strings"
	"testing"
)

// okSpawn is a SpawnFunc that returns a fixed child result with no error.
func okSpawn(result string) SpawnFunc {
	return func(_ context.Context, _, _, _, _ string, _ []string) (string, error) {
		return result, nil
	}
}

func TestSpawnAgentTool_NoBudgetFuncOmitsLine(t *testing.T) {
	tool := NewSpawnAgentTool(okSpawn("child said hi"))
	res := tool.Execute(context.Background(), `{"label":"c","task":"do"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	if strings.Contains(res.Output, "agent budget") {
		t.Errorf("no budget func => no budget line, got:\n%s", res.Output)
	}
	if !strings.Contains(res.Output, "child said hi") {
		t.Errorf("child result missing: %q", res.Output)
	}
}

func TestSpawnAgentTool_InjectsBudgetLineOnSuccess(t *testing.T) {
	budget := func() (used, max int) { return 7, 16 }
	tool := NewSpawnAgentToolWithBudget(okSpawn("done"), budget)

	res := tool.Execute(context.Background(), `{"label":"c","task":"do"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	// The injected line is computed by the runtime at result time.
	if !strings.Contains(res.Output, "agent budget: 7/16 used (9 remaining)") {
		t.Errorf("budget line not injected, got:\n%s", res.Output)
	}
	// The child's own result must still be present, ahead of the budget line.
	if !strings.Contains(res.Output, "done") {
		t.Errorf("child result dropped: %q", res.Output)
	}
	if idx := strings.Index(res.Output, "done"); idx > strings.Index(res.Output, "agent budget") {
		t.Errorf("budget line should follow the child result, got:\n%s", res.Output)
	}
}

func TestSpawnAgentTool_BudgetLineClampsRemainingAtZero(t *testing.T) {
	// At/over ceiling, remaining never goes negative in the injected text.
	budget := func() (used, max int) { return 16, 16 }
	tool := NewSpawnAgentToolWithBudget(okSpawn("last"), budget)
	res := tool.Execute(context.Background(), `{"label":"c","task":"do"}`)
	if !strings.Contains(res.Output, "agent budget: 16/16 used (0 remaining)") {
		t.Errorf("expected 0 remaining at ceiling, got:\n%s", res.Output)
	}
}

func TestSpawnAgentTool_NoBudgetLineOnSpawnError(t *testing.T) {
	// A failed spawn returns the error verbatim, with no budget line appended —
	// the model needs the clean error to react to.
	failing := func(_ context.Context, _, _, _, _ string, _ []string) (string, error) {
		return "", context.Canceled
	}
	budget := func() (used, max int) { return 3, 16 }
	tool := NewSpawnAgentToolWithBudget(failing, budget)
	res := tool.Execute(context.Background(), `{"label":"c","task":"do"}`)
	if !res.IsError {
		t.Fatal("expected an error result")
	}
	if strings.Contains(res.Output, "agent budget") {
		t.Errorf("budget line must not ride on an error result, got:\n%s", res.Output)
	}
}

func TestSpawnAgentTool_ZeroMaxBudgetOmitsLine(t *testing.T) {
	// max 0 = unset budget: no line, even though a budget func is present.
	budget := func() (used, max int) { return 5, 0 }
	tool := NewSpawnAgentToolWithBudget(okSpawn("x"), budget)
	res := tool.Execute(context.Background(), `{"label":"c","task":"do"}`)
	if strings.Contains(res.Output, "agent budget") {
		t.Errorf("unset budget (max 0) => no line, got:\n%s", res.Output)
	}
}

// --- change 0034: tighter-of-two budget for workflow children ---

func TestTighterBudget_ReportsFewerRemaining(t *testing.T) {
	global := func() (used, max int) { return 10, 64 } // 54 remaining
	workflow := func() (used, max int) { return 6, 8 } // 2 remaining (tighter)

	used, max := TighterBudget(global, workflow)()
	if used != 6 || max != 8 {
		t.Errorf("TighterBudget = (%d,%d), want (6,8) — the workflow-total is tighter", used, max)
	}
}

func TestTighterBudget_GlobalTighter(t *testing.T) {
	global := func() (used, max int) { return 60, 64 }  // 4 remaining (tighter)
	workflow := func() (used, max int) { return 1, 8 }  // 7 remaining
	used, max := TighterBudget(global, workflow)()
	if used != 60 || max != 64 {
		t.Errorf("TighterBudget = (%d,%d), want (60,64) — global is tighter", used, max)
	}
}

func TestTighterBudget_SkipsUnsetOperand(t *testing.T) {
	global := func() (used, max int) { return 10, 64 }
	unset := func() (used, max int) { return 0, 0 } // unset => ignored
	used, max := TighterBudget(global, unset)()
	if used != 10 || max != 64 {
		t.Errorf("TighterBudget = (%d,%d), want (10,64) — unset operand ignored", used, max)
	}
}

func TestTighterBudget_ShowsTighterInLine(t *testing.T) {
	global := func() (used, max int) { return 10, 64 }
	workflow := func() (used, max int) { return 6, 8 }
	tool := NewSpawnAgentToolWithBudget(okSpawn("x"), TighterBudget(global, workflow))
	res := tool.Execute(context.Background(), `{"label":"c","task":"do"}`)
	if !strings.Contains(res.Output, "6/8 used (2 remaining)") {
		t.Errorf("budget line should report the tighter 6/8, got:\n%s", res.Output)
	}
}
