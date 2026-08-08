package hitl

import (
	"bytes"
	"context"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/permissions"
)

// TestRoundTrip covers the happy path: a client request reaches the server's
// approve func and the decision returns intact.
func TestRoundTrip(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "hitl.sock")
	var gotReq permissions.ApprovalRequest
	srv, err := NewServer(sock, func(_ context.Context, req permissions.ApprovalRequest) (bool, bool, error) {
		gotReq = req
		return true, true, nil
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer srv.Close()

	client := ClientApprovalFunc(sock)
	approved, session, err := client(context.Background(), permissions.ApprovalRequest{
		ToolName: "bash", Args: `{"command":"ls"}`, Preview: "ls",
	})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	if !approved || !session {
		t.Errorf("approved=%v session=%v, want true/true", approved, session)
	}
	if gotReq.ToolName != "bash" {
		t.Errorf("server saw tool %q, want bash", gotReq.ToolName)
	}
}

// TestClientDialTimeout is the regression test for "blocks forever": dialing a
// non-existent socket must fail promptly, not hang.
func TestClientDialTimeout(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "nonexistent.sock")
	client := ClientApprovalFunc(sock)

	done := make(chan error, 1)
	go func() {
		_, _, err := client(context.Background(), permissions.ApprovalRequest{ToolName: "x"})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected dial error against missing socket, got nil")
		}
	case <-time.After(dialDeadline + 5*time.Second):
		t.Fatal("client dial hung past deadline")
	}
}

// TestClientContextCancel proves the client aborts a pending response read when
// its context is cancelled (the human never answers, caller gives up).
func TestClientContextCancel(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "hitl.sock")
	block := make(chan struct{})
	srv, err := NewServer(sock, func(ctx context.Context, _ permissions.ApprovalRequest) (bool, bool, error) {
		<-block // never answer during the test
		return false, false, nil
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer srv.Close()
	defer close(block)

	client := ClientApprovalFunc(sock)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, _, err := client(ctx, permissions.ApprovalRequest{ToolName: "x"})
		done <- err
	}()

	time.Sleep(50 * time.Millisecond) // let the request reach the server
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected cancellation error, got nil")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("client did not abort on context cancel")
	}
}

// TestServerLogsDecodeError is the regression test for silently-dropped decode
// errors: a garbage request must be logged, not swallowed.
func TestServerLogsDecodeError(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "hitl.sock")
	srv, err := NewServer(sock, permissions.AlwaysApprove)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer srv.Close()

	var buf syncBuffer
	srv.errLog = &buf

	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	// Send malformed JSON, then close so the decoder errors.
	_, _ = conn.Write([]byte("this-is-not-json\n"))
	_ = conn.Close()

	deadline := time.After(2 * time.Second)
	for {
		if strings.Contains(buf.String(), "decode request") {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("decode error was not logged; log=%q", buf.String())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// syncBuffer is a goroutine-safe bytes.Buffer for capturing async server logs.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}
func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}
