package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/model"
)

// TestOneShotRuntimeParity drives the REAL one-shot binary (run([]string{...}))
// against a scripted LLM_GATEWAY_URL httptest double (learning
// verify-tool-loop-at-gateway-seam) — the fake Completer/renderer never exercises
// cmd/fuse wiring. It asserts the one-shot path runs entirely through
// runtime.StartLoop: exit 0 and the gateway received exactly one request whose
// messages carry the task.
func TestOneShotRuntimeParity(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	var (
		mu       sync.Mutex
		reqCount int
		sawTask  bool
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var sent struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(body, &sent)
		mu.Lock()
		reqCount++
		for _, m := range sent.Messages {
			if m.Role == "user" && m.Content == "do a thing" {
				sawTask = true
			}
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"all done"}}]}`)
	}))
	defer srv.Close()

	t.Setenv("LLM_GATEWAY_URL", srv.URL)
	t.Setenv("LLM_GATEWAY_KEY", "tkn")

	var out, errb bytes.Buffer
	code := run([]string{"--model", "deepseek-flash", "--approve-all", "do a thing"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, errb.String())
	}

	mu.Lock()
	defer mu.Unlock()
	if reqCount != 1 {
		t.Errorf("gateway request count = %d, want 1", reqCount)
	}
	if !sawTask {
		t.Errorf("gateway never received the task in its messages")
	}
}

// TestResearchProbeRuntimeParity drives the REAL research-probe binary through
// runtime.StartLoop against a scripted gateway. The root synthesizes a report in
// one turn (no tool calls); the probe path must run through StartLoop + h.Wait()
// and still print the observable digest (probe.Summarize keeps the tree via
// Deps.Tree). Learning verify-tool-loop-at-gateway-seam.
func TestResearchProbeRuntimeParity(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	const finalReport = "SYNTHESIZED-RESEARCH-REPORT-XYZZY"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		payload := map[string]any{
			"choices": []any{map[string]any{
				"message": map[string]any{"role": "assistant", "content": finalReport},
			}},
		}
		b, _ := json.Marshal(payload)
		w.Write(b)
	}))
	defer srv.Close()

	t.Setenv("LLM_GATEWAY_URL", srv.URL)
	t.Setenv("LLM_GATEWAY_KEY", "tkn")

	var out, errb bytes.Buffer
	code := run([]string{"research-probe", "--model", "deepseek-flash", "--timeout", "30s", "what is fuse"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit = %d, stderr=%s\nstdout=%s", code, errb.String(), out.String())
	}
	s := out.String()
	if !strings.Contains(s, "=== fuse research probe ===") {
		t.Errorf("digest header missing from stdout:\n%s", s)
	}
	if !strings.Contains(s, finalReport) {
		t.Errorf("synthesized final report missing from digest:\n%s", s)
	}
}

// TestBuildShellRuntimeDeps is a wiring assertion (matching the
// blackboard_wiring_test.go / spawn_func_from_test.go style): buildShellRuntimeDeps
// returns a Deps whose BuildAgent produces a non-nil *agent.Agent and whose
// BuildChild is non-nil. A headless bubbletea assertion is impractical; the shell's
// end-to-end behavior stays covered by the existing shell_test.go teatest.
func TestBuildShellRuntimeDeps(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := config.Default()
	reg := model.DefaultRegistry()
	toolReg := defaultToolRegistry(cfg.Research, nil)
	tree := agent.NewAgentTreeWithConcurrency(reg.Default, reg.Default, cfg.Agents.MaxConcurrent)

	deps := buildShellRuntimeDeps(shellDepsInput{
		cfg: cfg, reg: reg, alias: reg.Default, toolReg: toolReg, tree: tree,
		verbose: false, skillBlock: "block", childApprove: nil, rootApprove: nil,
	})
	if deps.BuildChild == nil {
		t.Fatal("BuildChild is nil")
	}
	if deps.BuildAgent == nil {
		t.Fatal("BuildAgent is nil")
	}
	a, _, err := deps.BuildAgent(reg.Default, toolReg)
	if err != nil {
		t.Fatalf("BuildAgent: %v", err)
	}
	if a == nil {
		t.Fatal("BuildAgent returned nil agent")
	}
}

// TestNoDirectEngineDriveAtCmdSites is the patch-every-cloned-child-builder guard:
// no cmd/fuse binding ENTRYPOINT (main.go run(), shell.go runShell(),
// research_probe.go runResearchProbe()) drives the ROOT agent loop directly. Every
// root a.Run now lives inside internal/runtime/inproc.go's StartLoop; each entrypoint
// constructs runtime.New(deps) + StartLoop instead. The child runner a.Run lives only
// inside runtime_binding.go's BuildChild closures (the child the Runtime's Spawner
// invokes) — exempt, as are run.go's SHARED spawn/pipeline adapters (spawnFuncFrom,
// makePipeline*), which are the choke points the deps closures call, not a root drive.
func TestNoDirectEngineDriveAtCmdSites(t *testing.T) {
	// The binding entrypoints that must NOT drive the root engine directly.
	entrypoints := map[string]bool{
		"main.go":           true,
		"shell.go":          true,
		"research_probe.go": true,
	}
	entries, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, f := range entries {
		if !entrypoints[f] {
			continue
		}
		b, rerr := os.ReadFile(f)
		if rerr != nil {
			t.Fatalf("read %s: %v", f, rerr)
		}
		src := string(b)
		// A root-loop drive at these sites is agent.Run on the root agent variable.
		for _, bad := range []string{"a.Run(", "rootAgent.Run(", "ag.Run("} {
			if strings.Contains(src, bad) {
				t.Errorf("%s drives the root loop directly (%q) — it must go through runtime.StartLoop", f, bad)
			}
		}
		checked++
	}
	if checked != len(entrypoints) {
		t.Fatalf("checked %d entrypoints, expected %d — a binding file was renamed/moved", checked, len(entrypoints))
	}
}
