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

// fakeTruncatingGateway answers every chat request as a reply cut off at the cap
// mid-sentence until okAtCap is reached, recording the caps it was asked for. The
// cut-off replies carry text, so the agent treats them as needing more room.
func fakeTruncatingGateway(t *testing.T, okAtCap int) (*httptest.Server, func() []int) {
	t.Helper()
	var mu sync.Mutex
	var caps []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			MaxTokens int `json:"max_tokens"`
		}
		_ = json.Unmarshal(body, &req)
		mu.Lock()
		caps = append(caps, req.MaxTokens)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		content, reason := "cut off mid", "length"
		if okAtCap > 0 && req.MaxTokens >= okAtCap {
			content, reason = "finished", "stop"
		}
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q},\"finish_reason\":%q}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":%d}}\n\ndata: [DONE]\n\n", content, reason, req.MaxTokens)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []int {
		mu.Lock()
		defer mu.Unlock()
		return append([]int(nil), caps...)
	}
}

func oneShotAgainst(t *testing.T, gatewayURL string) (int, string, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	// These override the config file, and must never reach a real gateway from a test.
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
	code := run([]string{"say hi"}, &out, &errb)
	return code, out.String(), errb.String()
}

// The incident end to end: a run whose every reply is cut off must exit non-zero
// and say why. It used to exit 0 with no work done.
func TestOneShotExitsNonZeroWhenRepliesStayTruncated(t *testing.T) {
	srv, caps := fakeTruncatingGateway(t, 0)
	code, _, stderr := oneShotAgainst(t, srv.URL)
	if code == 0 {
		t.Fatalf("exit 0 with no reply; stderr:\n%s", stderr)
	}
	if !strings.Contains(stderr, "truncated at the output cap") {
		t.Errorf("stderr does not name the cause:\n%s", stderr)
	}
	// Doubling to the 4x ceiling, then two retries there asking for smaller pieces.
	if got := caps(); len(got) != 5 || got[0] != 100 || got[1] != 200 || got[2] != 400 || got[4] != 400 {
		t.Errorf("caps = %v, want 100 doubling to 400, then two more at 400", got)
	}
}

func TestOneShotRecoversWhenARaisedCapIsEnough(t *testing.T) {
	srv, caps := fakeTruncatingGateway(t, 200)
	code, stdout, stderr := oneShotAgainst(t, srv.URL)
	if code != 0 {
		t.Fatalf("exit %d; stderr:\n%s", code, stderr)
	}
	if !strings.Contains(stdout+stderr, "finished") {
		t.Errorf("final reply missing:\nstdout: %s\nstderr: %s", stdout, stderr)
	}
	if got := caps(); len(got) != 2 || got[1] != 200 {
		t.Errorf("caps = %v", got)
	}
}
