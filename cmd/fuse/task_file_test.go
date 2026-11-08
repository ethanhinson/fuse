package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestReadTaskFile(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if got, err := readTaskFile(write("ok.txt", "  fix the build\n")); err != nil || got != "fix the build" {
		t.Fatalf("got %q, %v", got, err)
	}
	if _, err := readTaskFile(write("empty.txt", " \n\t")); err == nil {
		t.Fatal("an empty task must be refused")
	}
	if _, err := readTaskFile(write("big.txt", strings.Repeat("x", maxTaskFileBytes+1))); err == nil {
		t.Fatal("an oversized file must be refused")
	}
	if _, err := readTaskFile(filepath.Join(dir, "missing.txt")); err == nil {
		t.Fatal("a missing file must be refused")
	}
}

// taskCapturingGateway answers every request with a final reply and records the
// first user message it saw.
func taskCapturingGateway(t *testing.T) (*httptest.Server, func() string) {
	t.Helper()
	var mu sync.Mutex
	var first string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Messages []struct{ Role, Content string } `json:"messages"`
		}
		_ = json.Unmarshal(body, &req)
		mu.Lock()
		for _, m := range req.Messages {
			if m.Role == "user" && first == "" {
				first = m.Content
			}
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":1}}\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv, func() string { mu.Lock(); defer mu.Unlock(); return first }
}

func oneShotWithArgs(t *testing.T, gatewayURL string, args ...string) (int, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("LLM_GATEWAY_URL", gatewayURL)
	t.Setenv("LLM_GATEWAY_KEY", "k")
	t.Chdir(t.TempDir())
	yaml := fmt.Sprintf("gateway: {url: %q, key: k}\nmodels:\n  default: m\n  m: {id: cloud/m, max_tokens: 100}\nmax_turns: 5\nsandbox: {handler: host}\n", gatewayURL)
	dir := filepath.Join(home, ".fuse")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yml"), []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	code := run(args, &out, &errb)
	return code, errb.String()
}

func TestOneShotTaskFileReachesTheModel(t *testing.T) {
	srv, first := taskCapturingGateway(t)
	task := filepath.Join(t.TempDir(), "task.txt")
	if err := os.WriteFile(task, []byte("kill every qemu process, then say done\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, stderr := oneShotWithArgs(t, srv.URL, "--task-file", task)
	if code != 0 {
		t.Fatalf("exit %d; stderr:\n%s", code, stderr)
	}
	if got := first(); !strings.Contains(got, "kill every qemu process, then say done") {
		t.Fatalf("the model did not get the file's task, got %q", got)
	}
}

func TestOneShotRefusesTaskFileAndArgument(t *testing.T) {
	srv, _ := taskCapturingGateway(t)
	task := filepath.Join(t.TempDir(), "task.txt")
	if err := os.WriteFile(task, []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _ := oneShotWithArgs(t, srv.URL, "--task-file", task, "b"); code != 2 {
		t.Fatalf("exit %d, want 2 for a task given twice", code)
	}
}
