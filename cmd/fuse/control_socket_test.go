package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/orchestrator"
	"github.com/ethanhinson/fuse/internal/runtime"
	"github.com/ethanhinson/fuse/internal/tools"
)

// blockingModel blocks its first call until cancelled (a long generation) and records
// every request, so a test can see the steer arrive.
type blockingModel struct {
	mu      sync.Mutex
	reqs    []model.CompletionReq
	started chan struct{}
}

func (b *blockingModel) Complete(ctx context.Context, req model.CompletionReq) (model.CompletionResp, error) {
	b.mu.Lock()
	b.reqs = append(b.reqs, req)
	first := len(b.reqs) == 1
	b.mu.Unlock()
	if first {
		close(b.started)
		<-ctx.Done()
		return model.CompletionResp{}, ctx.Err()
	}
	return model.CompletionResp{Content: "done"}, nil
}

func (b *blockingModel) lastUser() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.reqs) < 2 {
		return ""
	}
	req := b.reqs[len(b.reqs)-1]
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			return req.Messages[i].Content
		}
	}
	return ""
}

// controlFixture runs a loop whose first model call is in flight, with a control socket.
func controlFixture(t *testing.T) (sock, loopID string, bm *blockingModel) {
	t.Helper()
	bm = &blockingModel{started: make(chan struct{})}
	rt := runtime.New(runtime.Deps{
		BaseDir:       t.TempDir(),
		MaxConcurrent: 1,
		BuildAgent: func(store event.EventStore, tree *agent.AgentTree, modelID string, reg *tools.Registry) (*agent.Agent, agent.ChildBuilder, string, error) {
			return agent.New(bm, execRegistry{reg}, discardRenderer{}, modelID, "", 5, 0), nil, modelID, nil
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	h, err := rt.StartLoop(ctx, runtime.LoopConfig{Task: "long task", ModelID: "m"})
	if err != nil {
		t.Fatal(err)
	}
	// unix socket paths are short on macOS; t.TempDir() is too long for them.
	dir, err := os.MkdirTemp("/tmp", "fz")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock = filepath.Join(dir, "c.sock")
	stop, err := serveControlSocket(ctx, sock, rt)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	<-bm.started
	return sock, h.ID(), bm
}

type execRegistry struct{ reg *tools.Registry }

func (e execRegistry) Schemas() []model.ToolSchema { return e.reg.Schemas() }
func (e execRegistry) Execute(ctx context.Context, name, args string) tools.Result {
	return e.reg.Execute(ctx, name, args)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestControlSocketIsPrivate(t *testing.T) {
	sock, _, _ := controlFixture(t)
	fi, err := os.Stat(sock)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("socket mode = %o, want 600", perm)
	}
}

// `fuse steer --interrupt` over the control socket cancels the in-flight model call and
// the next call carries the message.
func TestSteerInterruptsARunOverTheControlSocket(t *testing.T) {
	sock, id, bm := controlFixture(t)
	var out, errb bytes.Buffer
	if code := runSteer([]string{"--socket", sock, "--loop", id, "--interrupt", "switch", "to", "plan", "B"}, &out, &errb); code != 0 {
		t.Fatalf("steer exit %d: %s", code, errb.String())
	}
	waitFor(t, "the steer to reach the model", func() bool { return strings.Contains(bm.lastUser(), "switch to plan B") })
	if !strings.Contains(out.String(), "interrupting") {
		t.Errorf("stdout = %q", out.String())
	}
}

func TestSteerRejectsMissingArguments(t *testing.T) {
	var out, errb bytes.Buffer
	if code := runSteer([]string{"--socket", "/nope"}, &out, &errb); code != 2 {
		t.Fatalf("no message: exit %d", code)
	}
	if code := runSteer([]string{"hello"}, &out, &errb); code != 2 {
		t.Fatalf("no loop/transport: exit %d", code)
	}
	if code := runSteer([]string{"--socket", "/nope", "--url", "http://x", "--loop", "l", "hello"}, &out, &errb); code != 2 {
		t.Fatalf("both transports: exit %d", code)
	}
}

// The orchestrator's wire target reads the loop's events and steers it, labelled, over
// the socket - the same path `fuse orchestrate` takes.
func TestWireTargetReadsAndSteersOverTheSocket(t *testing.T) {
	sock, id, bm := controlFixture(t)
	cf := connFlags{socket: sock, loop: id}
	client, err := cf.client()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	target := newWireTarget(ctx, client, id)

	reg := tools.NewRegistry()
	orchestrator.Register(reg, target)

	// The loop is mid-turn. The FIRST loop_status must already show what it has done:
	// an orchestrator that answers "nothing has happened yet" about a busy agent is
	// worse than none. (No retry loop here - a lazy stream start used to pass a retrying
	// version of this test while failing live.)
	status := reg.Execute(ctx, "loop_status", `{}`)
	if status.IsError || !strings.Contains(status.Output, "long task") {
		t.Fatalf("first status missed the loop's history:\n%s", status.Output)
	}

	if res := reg.Execute(ctx, "steer_agent", `{"message":"use the cache","interrupt":true}`); res.IsError {
		t.Fatalf("steer_agent: %s", res.Output)
	}
	waitFor(t, "the guidance to reach the model", func() bool {
		u := bm.lastUser()
		return strings.Contains(u, "[orchestrator guidance]") && strings.Contains(u, "use the cache")
	})
}
