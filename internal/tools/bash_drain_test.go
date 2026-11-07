package tools

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/tools/sandbox"
)

// slowRunner's Exec, once cancelled, keeps going for a while, as a `docker kill` in
// progress does.
type slowRunner struct {
	started  chan struct{}
	cleanup  time.Duration
	finished atomic.Bool
}

func (r *slowRunner) Exec(ctx context.Context, _ string, _ string) (sandbox.Output, error) {
	close(r.started)
	<-ctx.Done()
	time.Sleep(r.cleanup)
	r.finished.Store(true)
	return sandbox.Output{ExitCode: -1}, nil
}

func (*slowRunner) Release(context.Context) error { return nil }

type slowSubstrate struct{ runner *slowRunner }

func (s slowSubstrate) Acquire(context.Context, sandbox.Principal) (sandbox.Runner, error) {
	return s.runner, nil
}

func (slowSubstrate) Release(context.Context, sandbox.Principal, sandbox.Runner, sandbox.ReleaseCause) error {
	return nil
}

func (slowSubstrate) Close(context.Context) error { return nil }

// The agent loop abandons a cancelled tool call and goes straight to teardown, so
// teardown is all that stands between a pending container kill and process exit.
func TestCloseSandbox_WaitsForCancelledCommand(t *testing.T) {
	r := &slowRunner{started: make(chan struct{}), cleanup: 100 * time.Millisecond}
	b := newBashOverSubstrate(slowSubstrate{r})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Execute(ctx, `{"command":"x"}`)
	<-r.started
	cancel()
	if err := b.CloseSandbox(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !r.finished.Load() {
		t.Fatal("CloseSandbox returned while a cancelled command was still finishing")
	}
}

// A command that never finishes costs teardown a bound, not the process's exit.
func TestCloseSandbox_DrainIsBounded(t *testing.T) {
	r := &slowRunner{started: make(chan struct{}), cleanup: time.Hour}
	b := newBashOverSubstrate(slowSubstrate{r})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Execute(ctx, `{"command":"x"}`)
	<-r.started
	cancel()
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer closeCancel()
	at := time.Now()
	_ = b.CloseSandbox(closeCtx)
	if took := time.Since(at); took > 5*time.Second {
		t.Fatalf("took %s", took)
	}
}
