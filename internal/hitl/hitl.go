// Package hitl provides a Unix-socket HITL (human-in-the-loop) relay between a
// parent Fuse process and a child fuse mcp-server subprocess.
//
// The parent runs a Server bound to its TUI ApprovalFunc; the child connects
// to the socket for each tool call that needs user approval, blocking until
// the user responds in the parent's TUI.
package hitl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"github.com/ethanhinson/fuse/internal/permissions"
)

// Framing deadlines bound the TRANSPORT exchange, never the human decision. A
// peer must send its request/response promptly once connected; the human's
// think-time between them is deliberately unbounded (a person may take minutes
// to answer a y/N/a prompt), so no deadline spans the approve() call.
const (
	// requestDeadline bounds how long a connected peer may take to deliver its
	// framed request/response bytes. Generous enough for a stalled-but-alive
	// link, short enough that a dead peer does not wedge a goroutine forever.
	requestDeadline = 30 * time.Second
	// dialDeadline bounds establishing the client connection to the socket.
	dialDeadline = 5 * time.Second
	// responseWriteDeadline bounds writing the decision back once the human has
	// answered; the bytes are tiny so this only guards a dead peer.
	responseWriteDeadline = 30 * time.Second
)

type wireReq struct {
	ToolName string `json:"tool_name"`
	Args     string `json:"args"`
	Preview  string `json:"preview"`
}

type wireResp struct {
	Approved        bool   `json:"approved"`
	AllowForSession bool   `json:"allow_for_session"`
	ErrMsg          string `json:"err,omitempty"`
}

// Server listens on a Unix socket and relays each inbound approval request to
// approve. Each connection carries exactly one request/response pair.
type Server struct {
	ln   net.Listener
	path string
	// errLog receives non-fatal relay errors (decode/encode failures) that were
	// previously dropped silently. Defaults to os.Stderr; overridable in tests.
	errLog io.Writer
}

// NewServer creates and starts a Server at socketPath. approve is called
// (blocking) for each inbound request; pass tui.NewTeaApprovalFunc in shell
// mode or permissions.AlwaysApprove for non-interactive sessions.
func NewServer(socketPath string, approve permissions.ApprovalFunc) (*Server, error) {
	_ = os.Remove(socketPath) // clean up any leftover socket from a prior crash
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("hitl: listen %s: %w", socketPath, err)
	}
	s := &Server{ln: ln, path: socketPath, errLog: os.Stderr}
	go s.serve(approve)
	return s, nil
}

// Close shuts down the listener and removes the socket file.
func (s *Server) Close() {
	_ = s.ln.Close()
	_ = os.Remove(s.path)
}

func (s *Server) serve(approve permissions.ApprovalFunc) {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return // listener closed — normal shutdown
		}
		go s.relay(conn, approve)
	}
}

func (s *Server) relay(conn net.Conn, approve permissions.ApprovalFunc) {
	defer conn.Close()

	// Bound the request read: a connected-but-silent peer must not wedge this
	// goroutine forever. The deadline covers only the framed request bytes.
	_ = conn.SetReadDeadline(time.Now().Add(requestDeadline))
	var req wireReq
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		// Previously dropped silently, leaving the client blocked on its read
		// until ITS own deadline. Log for diagnosis and let the deferred Close
		// unblock the client with EOF.
		s.logf("hitl: decode request: %v", err)
		return
	}
	// Clear the read deadline for the human wait: the decision below is
	// deliberately unbounded (a person may take arbitrarily long to answer).
	_ = conn.SetReadDeadline(time.Time{})

	// A cancellable context is propagated to approve (was context.Background()),
	// so a future caller that wires shutdown into the Server can abort a pending
	// prompt; today it is cancelled only on relay return, but the seam is real
	// and no longer a hardcoded Background().
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	approved, allowSession, err := approve(ctx, permissions.ApprovalRequest{
		ToolName: req.ToolName,
		Args:     req.Args,
		Preview:  req.Preview,
	})
	resp := wireResp{Approved: approved, AllowForSession: allowSession}
	if err != nil {
		resp.ErrMsg = err.Error()
	}

	_ = conn.SetWriteDeadline(time.Now().Add(responseWriteDeadline))
	if err := json.NewEncoder(conn).Encode(resp); err != nil {
		s.logf("hitl: encode response: %v", err)
	}
}

func (s *Server) logf(format string, args ...any) {
	if s.errLog == nil {
		return
	}
	fmt.Fprintf(s.errLog, format+"\n", args...)
}

// ClientApprovalFunc returns a permissions.ApprovalFunc that dials socketPath,
// sends the request, and returns the parent's decision. Used inside
// fuse mcp-server to relay approvals back to the parent TUI.
// If socketPath is empty the function always approves.
func ClientApprovalFunc(socketPath string) permissions.ApprovalFunc {
	if socketPath == "" {
		return permissions.AlwaysApprove
	}
	return func(ctx context.Context, req permissions.ApprovalRequest) (bool, bool, error) {
		// Bound dialing so a missing/dead server surfaces promptly instead of
		// blocking. Honor the caller's context for cancellation too.
		d := net.Dialer{Timeout: dialDeadline}
		conn, err := d.DialContext(ctx, "unix", socketPath)
		if err != nil {
			return false, false, fmt.Errorf("hitl: dial %s: %w", socketPath, err)
		}
		defer conn.Close()

		// Propagate context cancellation to the blocking socket ops: closing the
		// connection unblocks any in-flight Read/Write with an error.
		stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
		defer stop()

		// Bound the request write; the human decision that follows is unbounded,
		// so no read deadline is set on the response.
		_ = conn.SetWriteDeadline(time.Now().Add(requestDeadline))
		if err := json.NewEncoder(conn).Encode(wireReq{
			ToolName: req.ToolName,
			Args:     req.Args,
			Preview:  req.Preview,
		}); err != nil {
			return false, false, fmt.Errorf("hitl: send: %w", err)
		}
		_ = conn.SetWriteDeadline(time.Time{})

		var resp wireResp
		if err := json.NewDecoder(conn).Decode(&resp); err != nil {
			if ctx.Err() != nil {
				return false, false, fmt.Errorf("hitl: recv cancelled: %w", ctx.Err())
			}
			if errors.Is(err, io.EOF) {
				return false, false, fmt.Errorf("hitl: server closed before responding")
			}
			return false, false, fmt.Errorf("hitl: recv: %w", err)
		}
		if resp.ErrMsg != "" {
			return false, false, fmt.Errorf("hitl: %s", resp.ErrMsg)
		}
		return resp.Approved, resp.AllowForSession, nil
	}
}
