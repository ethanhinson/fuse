package tools

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestBashRunsCommand(t *testing.T) {
	b := NewBash()
	if b.Name() != "bash" {
		t.Fatalf("name = %q", b.Name())
	}
	res := b.Execute(context.Background(), `{"command":"echo hello"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	if !strings.Contains(res.Output, "hello") {
		t.Errorf("output = %q", res.Output)
	}
}

func TestBashNonZeroExitIsError(t *testing.T) {
	res := NewBash().Execute(context.Background(), `{"command":"exit 3"}`)
	if !res.IsError {
		t.Fatal("expected error result on non-zero exit")
	}
}

func TestBashTimeout(t *testing.T) {
	res := NewBash().Execute(context.Background(), `{"command":"sleep 5","timeout_seconds":1}`)
	if !res.IsError {
		t.Fatal("expected timeout error")
	}
}

func TestBashRespectsContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res := NewBash().Execute(ctx, `{"command":"echo x"}`)
	if !res.IsError {
		t.Fatal("expected error from cancelled context")
	}
	_ = time.Second
}
