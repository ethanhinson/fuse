package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/permissions"
)

// A blocked terminal read cannot be cancelled, so without this a signal at the y/N
// prompt would wait for someone to press Enter.
func TestTerminalApproval_CancelEndsABlockedPrompt(t *testing.T) {
	in, w := io.Pipe()
	defer w.Close()
	var out bytes.Buffer
	ask := terminalApproval(in, &out)
	ctx, cancel := context.WithCancel(context.Background())
	cancel2 := time.AfterFunc(20*time.Millisecond, cancel)
	defer cancel2.Stop()
	done := make(chan error, 1)
	go func() {
		ok, _, err := ask(ctx, permissions.ApprovalRequest{Preview: "x"})
		if ok {
			err = errors.New("approved on cancel")
		}
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("prompt ignored cancellation")
	}
}

func TestRunStop_NoStop_FinishesZero(t *testing.T) {
	var stderr bytes.Buffer
	s := startRunStop(time.Hour, &stderr)
	if code := s.finish(); code != 0 {
		t.Fatalf("code = %d", code)
	}
	if s.finish() != 0 {
		t.Fatal("second finish changed the answer")
	}
	if s.ctx.Err() == nil {
		t.Error("finish left the context live")
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr: %q", stderr.String())
	}
}

func TestRunStop_LimitCancelsAndExplains(t *testing.T) {
	var stderr bytes.Buffer
	s := startRunStop(10*time.Millisecond, &stderr)
	select {
	case <-s.ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("limit did not cancel")
	}
	if code := s.finish(); code != exitCodeLimit {
		t.Fatalf("code = %d", code)
	}
	if !strings.Contains(stderr.String(), "max_run_duration") {
		t.Errorf("stderr: %q", stderr.String())
	}
}

func TestRunStop_Signals(t *testing.T) {
	for sig, want := range map[syscall.Signal]int{syscall.SIGTERM: 143, syscall.SIGINT: 130, syscall.SIGHUP: 129} {
		var stderr bytes.Buffer
		s := startRunStop(0, &stderr)
		if err := syscall.Kill(os.Getpid(), sig); err != nil {
			t.Fatal(err)
		}
		select {
		case <-s.ctx.Done():
		case <-time.After(5 * time.Second):
			t.Fatalf("%v did not cancel", sig)
		}
		if code := s.finish(); code != want {
			t.Errorf("%v: code = %d, want %d", sig, code, want)
		}
		if !strings.Contains(stderr.String(), "SIG") {
			t.Errorf("%v: stderr: %q", sig, stderr.String())
		}
	}
}
