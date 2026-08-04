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
	"fmt"
	"net"
	"os"

	"github.com/ethanhinson/fuse/internal/permissions"
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
	s := &Server{ln: ln, path: socketPath}
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
		go relay(conn, approve)
	}
}

func relay(conn net.Conn, approve permissions.ApprovalFunc) {
	defer conn.Close()
	var req wireReq
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		return
	}
	approved, allowSession, err := approve(context.Background(), permissions.ApprovalRequest{
		ToolName: req.ToolName,
		Args:     req.Args,
		Preview:  req.Preview,
	})
	resp := wireResp{Approved: approved, AllowForSession: allowSession}
	if err != nil {
		resp.ErrMsg = err.Error()
	}
	_ = json.NewEncoder(conn).Encode(resp)
}

// ClientApprovalFunc returns a permissions.ApprovalFunc that dials socketPath,
// sends the request, and returns the parent's decision. Used inside
// fuse mcp-server to relay approvals back to the parent TUI.
// If socketPath is empty the function always approves.
func ClientApprovalFunc(socketPath string) permissions.ApprovalFunc {
	if socketPath == "" {
		return permissions.AlwaysApprove
	}
	return func(_ context.Context, req permissions.ApprovalRequest) (bool, bool, error) {
		conn, err := net.Dial("unix", socketPath)
		if err != nil {
			return false, false, fmt.Errorf("hitl: dial %s: %w", socketPath, err)
		}
		defer conn.Close()

		if err := json.NewEncoder(conn).Encode(wireReq{
			ToolName: req.ToolName,
			Args:     req.Args,
			Preview:  req.Preview,
		}); err != nil {
			return false, false, fmt.Errorf("hitl: send: %w", err)
		}

		var resp wireResp
		if err := json.NewDecoder(conn).Decode(&resp); err != nil {
			return false, false, fmt.Errorf("hitl: recv: %w", err)
		}
		if resp.ErrMsg != "" {
			return false, false, fmt.Errorf("hitl: %s", resp.ErrMsg)
		}
		return resp.Approved, resp.AllowForSession, nil
	}
}
