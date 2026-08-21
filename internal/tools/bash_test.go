package tools

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/tools/sandbox"
)

// hostBash is the bash tool on the OFF-SWITCH substrate: a hand-built,
// host-authorized Config, which is the only configuration under which these
// unit tests may run a real command on this machine. It is still scrubbed —
// uncontained never means unscrubbed (see TestBashScrubsAmbientEnvironment).
func hostBash(t *testing.T) Tool {
	t.Helper()
	svc, err := sandbox.NewService(hostAuthorizedConfig())
	if err != nil {
		t.Fatalf("sandbox.NewService: %v", err)
	}
	return NewBash(svc)
}

func TestBashRunsCommand(t *testing.T) {
	b := hostBash(t)
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
	res := hostBash(t).Execute(context.Background(), `{"command":"exit 3"}`)
	if !res.IsError {
		t.Fatal("expected error result on non-zero exit")
	}
}

func TestBashTimeout(t *testing.T) {
	res := hostBash(t).Execute(context.Background(), `{"command":"sleep 5","timeout_seconds":1}`)
	if !res.IsError {
		t.Fatal("expected timeout error")
	}
}

func TestBashRespectsContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res := hostBash(t).Execute(ctx, `{"command":"echo x"}`)
	if !res.IsError {
		t.Fatal("expected error from cancelled context")
	}
	_ = time.Second
}
