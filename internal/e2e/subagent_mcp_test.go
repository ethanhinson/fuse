package e2e

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func spawnCall(id, label, task string) call {
	return call{id, "spawn_agent", mustArgs(map[string]string{"label": label, "task": task})}
}

func hasToolResult(r *request) bool { return len(r.toolResults()) > 0 }

// Spawns one per turn: the budget check sees every earlier child.
func TestSpawn_BudgetEnforced_AcrossTurns(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	var refusals int
	srv := newRouter(t, func(r *request) step {
		task := r.firstUser()
		if task != "e2e task" {
			seen[task] = true
			return text("child done")
		}
		n := len(r.toolResults())
		if n > 0 && strings.Contains(r.toolResults()[n-1], "budget exhausted") {
			refusals++
		}
		if n >= 4 {
			return text("root done")
		}
		return tools(spawnCall(fmt.Sprintf("s%d", n), fmt.Sprintf("l%d", n), fmt.Sprintf("child-%d", n)))
	})
	h := newHarness(t, srv)
	h.extra = "agents:\n  max_spawns: 2\n"
	wantCode(t, h.run(), 0)
	srv.mu.Lock()
	children, refused := len(seen), refusals
	srv.mu.Unlock()
	if children != 2 {
		t.Fatalf("%d children ran, budget is 2", children)
	}
	if refused == 0 {
		t.Fatal("no spawn was refused for budget")
	}
}

// Parallel spawns from one response race the check: used is counted from tree
// nodes, added only after the check, so every spawn can see room.
func TestSpawn_BudgetEnforced_ParallelBatch(t *testing.T) {
	defect(t, "max_spawns is check-then-act: parallel spawn_agent calls in one response overshoot the budget (internal/agent/spawn.go:317; used comes from childCount, nodes added later at spawn.go:409)")
	t.Parallel()
	// A race: any single attempt may happen to land under budget, so one overshoot
	// in any attempt is the failure.
	for attempt := 0; attempt < 25; attempt++ {
		seen := map[string]bool{}
		srv := newRouter(t, func(r *request) step {
			task := r.firstUser()
			if task != "e2e task" {
				seen[task] = true
				return text("child done")
			}
			if hasToolResult(r) {
				return text("root done")
			}
			var calls []call
			for i := 0; i < 12; i++ {
				calls = append(calls, spawnCall(fmt.Sprintf("s%d", i), fmt.Sprintf("l%d", i), fmt.Sprintf("child-%d", i)))
			}
			return tools(calls...)
		})
		h := newHarness(t, srv)
		h.extra = "agents:\n  max_spawns: 2\n"
		wantCode(t, h.run(), 0)
		srv.mu.Lock()
		children := len(seen)
		srv.mu.Unlock()
		if children > 2 {
			t.Fatalf("attempt %d: %d children ran, budget is 2", attempt, children)
		}
	}
}

// A model that spawns from every agent has to stop at the depth or budget cap.
func TestSpawn_RecursionBounded(t *testing.T) {
	t.Parallel()
	srv := newRouter(t, func(r *request) step {
		if hasToolResult(r) {
			return text("done")
		}
		return tools(spawnCall("s1", fmt.Sprintf("l%d", r.N), fmt.Sprintf("task-%d", r.N)))
	})
	h := newHarness(t, srv)
	h.extra = "agents:\n  max_spawns: 8\n"
	wantCode(t, h.run(), 0)
	if srv.count() > 30 {
		t.Fatalf("requests = %d", srv.count())
	}
}

func TestSpawn_ChildFailureReportedToParent(t *testing.T) {
	t.Parallel()
	srv := newRouter(t, func(r *request) step {
		if r.firstUser() == "child-1" {
			return status(401, `{"error":"child rejected"}`)
		}
		if hasToolResult(r) {
			return text("root done")
		}
		return tools(spawnCall("s1", "a", "child-1"))
	})
	h := newHarness(t, srv)
	wantCode(t, h.run(), 0)
	out, ok := h.lastRequest().toolResult("s1")
	if !ok {
		t.Fatal("no spawn result reached the parent")
	}
	wantContains(t, out, "child rejected", "spawn result")
}

// spawn_agent is exempt from the tool timeout, so a child silent at the model is
// bounded only by gateway.request_timeout.
func TestSpawn_ChildHang_BoundedByRequestTimeout(t *testing.T) {
	t.Parallel()
	srv := newServer(t)
	srv.route = func(r *request) step {
		if r.firstUser() == "child-1" {
			return srv.hang()
		}
		if hasToolResult(r) {
			return text("root done")
		}
		return tools(spawnCall("s1", "a", "child-1"))
	}
	h := newHarness(t, srv)
	h.gatewayExtra = "request_timeout: 500ms"
	wantCode(t, h.run(), 0)
	out, ok := h.lastRequest().toolResult("s1")
	if !ok {
		t.Fatal("no spawn result reached the parent")
	}
	wantContains(t, out, "deadline exceeded", "spawn result")
}

func TestSpawn_ChildHungBash_BoundedByToolTimeout(t *testing.T) {
	t.Parallel()
	marker := sleepMarker()
	srv := newRouter(t, func(r *request) step {
		if r.firstUser() == "child-1" {
			if hasToolResult(r) {
				return text("child gave up")
			}
			return tool("bash", map[string]string{"command": "sleep " + marker + "; echo x"})
		}
		if hasToolResult(r) {
			return text("root done")
		}
		return tools(spawnCall("s1", "a", "child-1"))
	})
	h := newHarness(t, srv)
	h.hostSandbox()
	h.extra = "agents:\n  tool_timeout_seconds: 1\n"
	wantCode(t, h.run("--approve-all"), 0)
	out, _ := h.lastRequest().toolResult("s1")
	wantContains(t, out, "child gave up", "spawn result")
}

// --- MCP ------------------------------------------------------------------

const mcpTool = "mcp:h/echo"

// mcpConfig renders one stdio server running this test binary in mode.
func (h *harness) mcpConfig(mode string) (pidFile string) {
	h.t.Helper()
	pidFile = filepath.Join(h.t.TempDir(), "pid")
	h.extra += fmt.Sprintf(`mcp_servers:
  - name: h
    transport: stdio
    command: [%q]
    env:
      %s: %s
      %s: %s
`, selfBin, mcpModeEnv, mode, mcpPidFileEnv, pidFile)
	return pidFile
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func readPid(t *testing.T, pidFile string) int {
	t.Helper()
	pid, err := strconv.Atoi(strings.TrimSpace(readFile(t, pidFile)))
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

func waitDead(pid int, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if !alive(pid) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return !alive(pid)
}

func TestMCP_Normal_ToolCallWorks_NoOrphan(t *testing.T) {
	t.Parallel()
	srv, h := scripted(t)
	pidFile := h.mcpConfig("normal")
	srv.script(tool(mcpTool, `{}`), text("done"))
	wantCode(t, h.run("--approve-all"), 0)
	wantContains(t, h.toolOutput(), "echoed", "tool result")
	if pid := readPid(t, pidFile); !waitDead(pid, 2*time.Second) {
		t.Fatalf("MCP server %d outlived the run", pid)
	}
}

// Even a server that ignores stdin closing is killed on a normal exit.
func TestMCP_IgnoresEOF_KilledOnNormalExit(t *testing.T) {
	t.Parallel()
	srv, h := scripted(t)
	pidFile := h.mcpConfig("ignoreeof")
	srv.script(text("done"))
	wantCode(t, h.run(), 0)
	if pid := readPid(t, pidFile); !waitDead(pid, 2*time.Second) {
		t.Fatalf("MCP server %d outlived the run", pid)
	}
}

func TestMCP_KilledOnModelFailureExit(t *testing.T) {
	t.Parallel()
	srv := newRouter(t, func(*request) step { return status(401, "no") })
	h := newHarness(t, srv)
	pidFile := h.mcpConfig("ignoreeof")
	wantCode(t, h.run(), 1)
	if pid := readPid(t, pidFile); !waitDead(pid, 2*time.Second) {
		t.Fatalf("MCP server %d outlived the failed run", pid)
	}
}

func TestMCP_NoOrphanAfterSignal(t *testing.T) {
	t.Parallel()
	srv := newServer(t)
	srv.route = func(*request) step { return srv.hang() }
	h := newHarness(t, srv)
	pidFile := h.mcpConfig("ignoreeof")
	p := h.start(defaultTimeout, "--model", "m", "e2e task")
	srv.waitForRequests(t, 1, 10*time.Second)
	p.signal(syscall.SIGTERM)
	p.wait()
	if pid := readPid(t, pidFile); !waitDead(pid, 2*time.Second) {
		t.Fatalf("MCP server %d outlived SIGTERM", pid)
	}
}

// A launcher (npx, uvx, a shell script) forks the real server. stop() kills only
// the launcher, so the real server survives a perfectly normal exit.
func TestMCP_WrapperChild_NoOrphanOnNormalExit(t *testing.T) {
	t.Parallel()
	srv, h := scripted(t)
	pidFile := filepath.Join(t.TempDir(), "pid")
	h.extra = fmt.Sprintf(`mcp_servers:
  - name: h
    transport: stdio
    command: ["/bin/sh", "-c", %q]
    env:
      %s: ignoreeof
      %s: %s
`, selfBin+"; exit 0", mcpModeEnv, mcpPidFileEnv, pidFile)
	srv.script(text("done"))
	wantCode(t, h.run(), 0)
	if pid := readPid(t, pidFile); !waitDead(pid, 2*time.Second) {
		t.Fatalf("real server %d outlived the run behind its launcher", pid)
	}
}

func TestMCP_CrashMidCall_ReportedRunContinues(t *testing.T) {
	t.Parallel()
	srv, h := scripted(t)
	h.mcpConfig("crash")
	srv.script(
		tool(mcpTool, `{}`),
		tools(call{"call_2", mcpTool, `{"again":true}`}),
		text("carried on"),
	)
	r := h.run("--approve-all")
	wantCode(t, r, 0)
	first, _ := h.lastRequest().toolResult("call_1")
	wantContains(t, first, "closed", "first result")
	second, _ := h.lastRequest().toolResult("call_2")
	wantContains(t, second, "closed", "second result")
	wantContains(t, r.stdout, "carried on", "stdout")
}

// startTimeout is 3s. A server that never answers initialize is skipped, and its
// tools are simply not offered.
func TestMCP_NeverAnswersInitialize_SkippedBounded(t *testing.T) {
	t.Parallel()
	srv, h := scripted(t)
	pidFile := h.mcpConfig("noinit")
	srv.script(text("done"))
	r := h.run()
	wantCode(t, r, 0)
	if r.dur > 10*time.Second {
		t.Fatalf("took %s", r.dur)
	}
	wantContains(t, r.stderr, "skipping server", "stderr")
	for _, tl := range srv.requests()[0].Tools {
		if strings.HasPrefix(tl.Function.Name, "mcp:") {
			t.Fatalf("offered tool %s from a server that never initialized", tl.Function.Name)
		}
	}
	if pid := readPid(t, pidFile); !waitDead(pid, 2*time.Second) {
		t.Fatalf("unresponsive server %d not cleaned up", pid)
	}
}

func TestMCP_HangingCall_BoundedByToolTimeout(t *testing.T) {
	t.Parallel()
	srv, h := scripted(t)
	h.mcpConfig("hangcall")
	h.extra = "agents:\n  tool_timeout_seconds: 1\n" + h.extra
	srv.script(tool(mcpTool, `{}`), text("done"))
	wantCode(t, h.run("--approve-all"), 0)
	out := h.toolOutput()
	if !strings.Contains(out, "per-call timeout") && !strings.Contains(out, "deadline") {
		t.Fatalf("tool result = %q", out)
	}
}

func TestMCP_OversizedResult_Spilled(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	t.Parallel()
	srv, h := scripted(t)
	h.mcpConfig("oversize")
	srv.script(tool(mcpTool, `{}`), text("done"))
	wantCode(t, h.run("--approve-all"), 0)
	out := h.toolOutput()
	if len(out) > 40<<10 {
		t.Fatalf("result reached the model at %d bytes", len(out))
	}
	wantContains(t, out, "output truncated", "tool result")
}
