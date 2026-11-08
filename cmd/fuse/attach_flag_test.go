package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestAttachFlagRejectsModelWithoutAttachments(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "unexpected", 500)
	}))
	defer srv.Close()
	img := filepath.Join(t.TempDir(), "x.png")
	os.WriteFile(img, []byte("x"), 0o644)
	code, stderr := oneShotWithArgs(t, srv.URL, "--model", "deepseek-flash", "--attach", img, "task")
	if code != 2 || !strings.Contains(stderr, "does not accept attachments") {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	if calls.Load() != 0 {
		t.Errorf("gateway called %d times", calls.Load())
	}
}

func TestAttachFlagBadFile(t *testing.T) {
	srv, _ := taskCapturingGateway(t)
	txt := filepath.Join(t.TempDir(), "a.txt")
	os.WriteFile(txt, []byte("hello"), 0o644)
	for _, p := range []string{filepath.Join(t.TempDir(), "missing.png"), txt} {
		// Model "claude" is the built-in Claude entry, which accepts attachments.
		code, stderr := oneShotWithArgs(t, srv.URL, "--model", "claude", "--attach", p, "task")
		if code != 2 || !strings.Contains(stderr, "--attach:") {
			t.Errorf("%s: exit %d, stderr %q", p, code, stderr)
		}
	}
}
