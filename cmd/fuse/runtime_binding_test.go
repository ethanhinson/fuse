package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
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
