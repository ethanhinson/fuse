package tools

import (
	"context"
	"strings"
	"testing"
)

// fakeHandle is a test SpawnHandle that returns a fixed SpawnResult (change 0044).
// It stands in for the cmd/fuse adapter over agent.AgentHandle, letting the tool's
// model-facing contract be asserted without importing internal/agent.
type fakeHandle struct{ res SpawnResult }

func (h fakeHandle) WaitResult() SpawnResult { return h.res }

// okSpawn is a SpawnFunc that returns a handle whose result is a fixed string with
// no error. The tool awaits it internally (change 0044) and produces the same
// model-facing output as the pre-0044 string-returning seam.
func okSpawn(result string) SpawnFunc {
	return func(_ context.Context, _ SpawnRequest) (SpawnHandle, error) {
		return fakeHandle{res: SpawnResult{Result: result}}, nil
	}
}

// TestSpawnAgentTool_ToolsParamWarnsAboutBlackboard pins the prompting fix in the
// tools param schema: a narrow subset silently strips blackboard access from the
// child, so the description must warn the model about it. Guards against regress.
func TestSpawnAgentTool_ToolsParamWarnsAboutBlackboard(t *testing.T) {
	tool := NewSpawnAgentTool(okSpawn("ok"))
	params := tool.Parameters()
	props, _ := params["properties"].(map[string]any)
	toolsProp, _ := props["tools"].(map[string]any)
	desc, _ := toolsProp["description"].(string)
	for _, want := range []string{"blackboard", "subset"} {
		if !strings.Contains(strings.ToLower(desc), want) {
			t.Errorf("tools param description must warn about %q; got: %q", want, desc)
		}
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
	failing := func(_ context.Context, _ SpawnRequest) (SpawnHandle, error) {
		return nil, context.Canceled
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
	failing := func(_ context.Context, _ SpawnRequest) (SpawnHandle, error) {
		return nil, context.Canceled
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
	spawn := func(_ context.Context, req SpawnRequest) (SpawnHandle, error) {
		got = req
		return fakeHandle{res: SpawnResult{Result: "ok"}}, nil
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
	global := func() (used, max int) { return 60, 64 } // 4 remaining (tighter)
	workflow := func() (used, max int) { return 1, 8 } // 7 remaining
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

// --- change 0042 (D7): freeform spawn_agent is prose-only ---

// captureSpawn records the SpawnRequest it receives so a test can assert what
// the tool threaded across the seam.
func captureSpawn(got *SpawnRequest) SpawnFunc {
	return func(_ context.Context, req SpawnRequest) (SpawnHandle, error) {
		*got = req
		return fakeHandle{res: SpawnResult{Result: "ok"}}, nil
	}
}

// The model-facing tool no longer advertises an `expects` param: freeform
// model-driven spawns are prose-only (change 0042, D7). Structured delegation
// survives on the pipeline/code path via agent.SpawnOpts.Expects, not here.
func TestSpawnAgentTool_ExpectsParamNotAdvertised(t *testing.T) {
	tool := NewSpawnAgentTool(okSpawn("x"))
	props := tool.Parameters()["properties"].(map[string]any)
	if _, ok := props["expects"]; ok {
		t.Fatal("expects param must NOT be advertised in Parameters() — prose-only")
	}
	// And it must never appear in required either.
	req := tool.Parameters()["required"].([]string)
	for _, r := range req {
		if r == "expects" {
			t.Error("expects must not appear in required")
		}
	}
}

// A raw `expects` key in the args is ignored/dropped — it never threads into the
// SpawnRequest, so the freeform child gets no schema (change 0042, D7).
func TestSpawnAgentTool_ExpectsInArgsIsDropped(t *testing.T) {
	var got SpawnRequest
	tool := NewSpawnAgentTool(captureSpawn(&got))
	args := `{"label":"c","task":"do","expects":{"type":"object","properties":{"n":{"type":"integer"}}}}`
	res := tool.Execute(context.Background(), args)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	if got.Expects != nil {
		t.Fatalf("expects key in args must be dropped; got %#v", got.Expects)
	}
}

func TestSpawnAgentTool_AbsentExpectsIsNil(t *testing.T) {
	var got SpawnRequest
	tool := NewSpawnAgentTool(captureSpawn(&got))
	res := tool.Execute(context.Background(), `{"label":"c","task":"do"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	if got.Expects != nil {
		t.Fatalf("absent expects must leave req.Expects nil; got %#v", got.Expects)
	}
}

// --- change 0044: handle-returning SpawnFunc; tool blocks internally ---

// TestSpawnAgentTool_AwaitsHandleForModelContract is the D1 gate: with the new
// handle-returning SpawnFunc, the tool must block on handle.WaitResult() and emit
// the SAME model-facing string as the pre-0044 string-returning seam — prose +
// budget line + quota line, in that order. "Same model-visible output, new
// Go-visible contract."
func TestSpawnAgentTool_AwaitsHandleForModelContract(t *testing.T) {
	budget := func() (used, max int) { return 2, 10 }
	quota := func() string { return "\n\ntoken quota exhausted: conclude now" }
	spawn := func(_ context.Context, _ SpawnRequest) (SpawnHandle, error) {
		return fakeHandle{res: SpawnResult{Result: "child prose"}}, nil
	}
	tool := NewSpawnAgentToolWithBudget(spawn, budget).WithQuotaWarning(quota)
	res := tool.Execute(context.Background(), `{"label":"c","task":"do"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	want := "child prose" +
		"\n\nagent budget: 2/10 used (8 remaining)" +
		"\n\ntoken quota exhausted: conclude now"
	if res.Output != want {
		t.Fatalf("model-facing output changed:\n got %q\nwant %q", res.Output, want)
	}
}

// TestSpawnAgentTool_HandleErrorIsErrorResult: a handle that completes with a run
// error (distinct from a spawn refused before it started) surfaces as an error
// result with no budget/quota line — the same clean-error posture the model needs.
func TestSpawnAgentTool_HandleErrorIsErrorResult(t *testing.T) {
	budget := func() (used, max int) { return 3, 16 }
	spawn := func(_ context.Context, _ SpawnRequest) (SpawnHandle, error) {
		return fakeHandle{res: SpawnResult{Err: context.DeadlineExceeded}}, nil
	}
	tool := NewSpawnAgentToolWithBudget(spawn, budget)
	res := tool.Execute(context.Background(), `{"label":"c","task":"do"}`)
	if !res.IsError {
		t.Fatal("expected an error result when the handle completes with an error")
	}
	if strings.Contains(res.Output, "agent budget") {
		t.Errorf("no budget line on a handle-error result, got:\n%s", res.Output)
	}
	if !strings.Contains(res.Output, "spawn_agent:") {
		t.Errorf("error result should be prefixed spawn_agent:, got:\n%s", res.Output)
	}
}
