package tools

import (
	"context"
	"strings"
	"testing"
)

// okSpawn is a SpawnFunc that returns a fixed child result with no error.
func okSpawn(result string) SpawnFunc {
	return func(_ context.Context, _ SpawnRequest) (string, error) {
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
	failing := func(_ context.Context, _ SpawnRequest) (string, error) {
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

// --- change 0036: token-quota warning line ---

// The token-quota warning line mirrors the budget line: a QuotaFunc supplies the
// machine-authored line (or "") at result time, appended to a SUCCESSFUL spawn
// result so the agent concludes with what it has once a hard token quota is hit.
// Absent before exhaustion, absent outside scope, never on an error result.

func TestSpawnAgentTool_NoQuotaWarningBeforeExhaustion(t *testing.T) {
	// QuotaFunc returns "" (quota not exhausted) => no warning line.
	quota := func() string { return "" }
	tool := NewSpawnAgentToolWithBudget(okSpawn("child ok"), nil).WithQuotaWarning(quota)
	res := tool.Execute(context.Background(), `{"label":"c","task":"do"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	if strings.Contains(res.Output, "token quota") {
		t.Errorf("no warning before exhaustion, got:\n%s", res.Output)
	}
	if !strings.Contains(res.Output, "child ok") {
		t.Errorf("child result missing: %q", res.Output)
	}
}

func TestSpawnAgentTool_InjectsQuotaWarningWhenExhausted(t *testing.T) {
	const line = "\n\ntoken quota exhausted: conclude with the results you already have and do not spawn again"
	quota := func() string { return line }
	tool := NewSpawnAgentToolWithBudget(okSpawn("done"), nil).WithQuotaWarning(quota)
	res := tool.Execute(context.Background(), `{"label":"c","task":"do"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	if !strings.Contains(res.Output, "token quota exhausted") {
		t.Errorf("warning line not injected, got:\n%s", res.Output)
	}
	// The child result stays ahead of the warning line.
	if idx := strings.Index(res.Output, "done"); idx < 0 || idx > strings.Index(res.Output, "token quota") {
		t.Errorf("warning must follow the child result, got:\n%s", res.Output)
	}
}

func TestSpawnAgentTool_QuotaWarningRidesAlongsideBudgetLine(t *testing.T) {
	// Both a budget line and a quota warning may be appended in one result; the
	// seam is singular but each optional term contributes its own line.
	budget := func() (used, max int) { return 3, 16 }
	quota := func() string { return "\n\ntoken quota exhausted: conclude now" }
	tool := NewSpawnAgentToolWithBudget(okSpawn("done"), budget).WithQuotaWarning(quota)
	res := tool.Execute(context.Background(), `{"label":"c","task":"do"}`)
	if !strings.Contains(res.Output, "agent budget: 3/16") {
		t.Errorf("budget line missing, got:\n%s", res.Output)
	}
	if !strings.Contains(res.Output, "token quota exhausted") {
		t.Errorf("quota warning missing, got:\n%s", res.Output)
	}
}

func TestSpawnAgentTool_NoQuotaWarningOnSpawnError(t *testing.T) {
	failing := func(_ context.Context, _ SpawnRequest) (string, error) {
		return "", context.Canceled
	}
	quota := func() string { return "\n\ntoken quota exhausted: conclude now" }
	tool := NewSpawnAgentToolWithBudget(failing, nil).WithQuotaWarning(quota)
	res := tool.Execute(context.Background(), `{"label":"c","task":"do"}`)
	if !res.IsError {
		t.Fatal("expected an error result")
	}
	if strings.Contains(res.Output, "token quota") {
		t.Errorf("warning must not ride on an error result, got:\n%s", res.Output)
	}
}

func TestSpawnAgentTool_NilQuotaFuncOmitsWarning(t *testing.T) {
	// No QuotaFunc configured => never a warning line (byte-identical to 0034).
	tool := NewSpawnAgentToolWithBudget(okSpawn("x"), nil)
	res := tool.Execute(context.Background(), `{"label":"c","task":"do"}`)
	if strings.Contains(res.Output, "token quota") {
		t.Errorf("nil quota func => no warning, got:\n%s", res.Output)
	}
}

// --- change 0034: worker param schema ---

func TestSpawnAgentTool_NoWorkerParamByDefault(t *testing.T) {
	tool := NewSpawnAgentTool(okSpawn("x"))
	props := tool.Parameters()["properties"].(map[string]any)
	if _, ok := props["worker"]; ok {
		t.Error("worker param must be absent outside a workflow (no workers configured)")
	}
}

func TestSpawnAgentTool_WithWorkersAddsEnum(t *testing.T) {
	tool := NewSpawnAgentTool(okSpawn("x")).WithWorkers([]string{"facet-researcher", "summarizer"})
	props := tool.Parameters()["properties"].(map[string]any)
	w, ok := props["worker"].(map[string]any)
	if !ok {
		t.Fatal("worker param should be present when workers are configured")
	}
	enum, ok := w["enum"].([]any)
	if !ok || len(enum) != 2 {
		t.Fatalf("worker enum = %v, want 2 entries", w["enum"])
	}
	if enum[0] != "facet-researcher" || enum[1] != "summarizer" {
		t.Errorf("worker enum = %v, want [facet-researcher summarizer]", enum)
	}
}

func TestSpawnAgentTool_WorkerThreadedToSpawnFunc(t *testing.T) {
	var got SpawnRequest
	spawn := func(_ context.Context, req SpawnRequest) (string, error) {
		got = req
		return "ok", nil
	}
	tool := NewSpawnAgentTool(spawn).WithWorkers([]string{"facet-researcher"})
	res := tool.Execute(context.Background(), `{"label":"c","task":"do","worker":"facet-researcher"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	if got.Worker != "facet-researcher" {
		t.Errorf("SpawnRequest.Worker = %q, want facet-researcher", got.Worker)
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
