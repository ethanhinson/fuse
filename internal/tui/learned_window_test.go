package tui

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/permissions"
)

// rejectOnceCompleter refuses its first request for length and answers
// normally afterwards, counting every request it sees.
type rejectOnceCompleter struct {
	mu    sync.Mutex
	calls int
}

func (c *rejectOnceCompleter) Complete(_ context.Context, _ model.CompletionReq) (model.CompletionResp, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.calls == 1 {
		return model.CompletionResp{}, errors.New("gateway status 400: This model's maximum context length is 4096 tokens")
	}
	return model.CompletionResp{Content: "ok"}, nil
}

func (c *rejectOnceCompleter) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The first exchange learns a window from a rejection. The second exchange's
// fresh agent starts from it, so it manages context against the window instead
// of sending another request the provider would refuse.
func TestShellReusesLearnedWindowAcrossExchanges(t *testing.T) {
	cmp := &rejectOnceCompleter{}
	build := func(_ string, r agent.Renderer, _ permissions.ApprovalFunc) (*agent.Agent, error) {
		return agent.New(cmp, noopToolExec{}, r, "test/model", "", 25, 0), nil
	}
	m := NewShellModel("alpha", false, "", testRegistry(), nil, build, permissions.NewSessionMode(permissions.ModeSmart), true)
	if got := m.learned.get("alpha"); got != 0 {
		t.Fatalf("fresh learned window = %d, want 0", got)
	}
	h := startHarnessWithModel(t, m)

	h.typeAndSubmit("first")
	waitFor(t, "first exchange to learn a window", func() bool { return m.learned.get("alpha") > 0 })
	if got := cmp.count(); got != 1 {
		t.Fatalf("requests after first exchange = %d, want 1", got)
	}

	h.typeAndSubmit("second")
	// The second exchange must not send a request: the learned window is too
	// small for the history, which ends the run before any call. A fresh
	// unknown window would have sent (and had accepted) a second request.
	time.Sleep(500 * time.Millisecond)
	if got := cmp.count(); got != 1 {
		t.Errorf("requests after second exchange = %d, want 1 (learned window reused)", got)
	}
}

// A learned window belongs to one model.
func TestLearnedWindowIsPerModel(t *testing.T) {
	m := NewShellModel("alpha", false, "", testRegistry(), nil, nil, permissions.NewSessionMode(permissions.ModeSmart), true)
	m.learned.set("alpha", 5000)
	if got := m.learned.get("beta"); got != 0 {
		t.Errorf("window leaked to another alias: %d", got)
	}
	m.switchModel("beta")
	m.switchModel("alpha")
	if got := m.learned.get("alpha"); got != 0 {
		t.Errorf("window survived a model switch: %d", got)
	}
}
