// Package mcp provides a stdio-transport MCP client (JSON-RPC 2.0).
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"sync/atomic"
)

// defaultLogCap is the default stderr ring buffer capacity per server.
// Override with FUSE_MCP_LOG_LINES env var.
const defaultLogCap = 200

func logCap() int {
	if s := os.Getenv("FUSE_MCP_LOG_LINES"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			return n
		}
	}
	return defaultLogCap
}

// jsonrpcRequest is a JSON-RPC 2.0 request frame.
type jsonrpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      string `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// jsonrpcNotification is a JSON-RPC 2.0 notification frame — deliberately has
// no ID field (a notification MUST omit id; jsonrpcRequest.ID is not omitempty
// and so cannot express one).
type jsonrpcNotification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// jsonrpcResponse is a JSON-RPC 2.0 response frame.
type jsonrpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      string          `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *jsonrpcError   `json:"error,omitempty"`
}

// jsonrpcInbound is a permissive frame that captures BOTH the response fields
// (id/result/error) and a notification's method/params in a single decode. A
// read pump sniffs it: a frame with a method and no id is a server→client
// notification (e.g. "$/progress"), routed to the notification router; anything
// with an id is a response, fanned to the matching pending caller.
type jsonrpcInbound struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      string          `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *jsonrpcError   `json:"error,omitempty"`
}

// isNotification reports whether the inbound frame is an id-less notification
// (a method with no id). Response frames carry an id; requests from the server
// are not part of fuse's client contract.
func (f jsonrpcInbound) isNotification() bool { return f.Method != "" && f.ID == "" }

// response projects the inbound frame's response fields.
func (f jsonrpcInbound) response() jsonrpcResponse {
	return jsonrpcResponse{JSONRPC: f.JSONRPC, ID: f.ID, Result: f.Result, Error: f.Error}
}

type jsonrpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// StdioClient manages a single MCP server process over stdio.
type StdioClient struct {
	name    string
	cmd     *exec.Cmd
	enc     *json.Encoder
	dec     *json.Decoder
	mu      sync.Mutex
	pending map[string]chan jsonrpcResponse
	counter atomic.Uint64
	done    chan struct{}
	once    sync.Once

	// router receives id-less notification frames (e.g. "$/progress"). Nil for
	// clients constructed without a manager, in which case notifications are
	// dropped (pre-#0020 behavior).
	router notificationRouter

	logMu   sync.Mutex
	logRing []string
	logCap  int
}

// LogLines returns the last n lines of stderr (or all available if n <= 0).
func (c *StdioClient) LogLines(n int) []string {
	c.logMu.Lock()
	defer c.logMu.Unlock()
	if n <= 0 || n >= len(c.logRing) {
		out := make([]string, len(c.logRing))
		copy(out, c.logRing)
		return out
	}
	start := len(c.logRing) - n
	out := make([]string, n)
	copy(out, c.logRing[start:])
	return out
}

// appendLog adds a line to the ring buffer, evicting the oldest when full.
func (c *StdioClient) appendLog(line string) {
	c.logMu.Lock()
	defer c.logMu.Unlock()
	if len(c.logRing) >= c.logCap {
		copy(c.logRing, c.logRing[1:])
		c.logRing[len(c.logRing)-1] = line
	} else {
		c.logRing = append(c.logRing, line)
	}
}

// PID returns the process ID of the server process, or 0 if not started.
func (c *StdioClient) PID() int {
	if c.cmd == nil || c.cmd.Process == nil {
		return 0
	}
	return c.cmd.Process.Pid
}

// newStdioClient spawns the server process and starts the read pump.
func newStdioClient(name string, command []string, env []string) (*StdioClient, error) {
	if len(command) == 0 {
		return nil, fmt.Errorf("mcp server %q: empty command", name)
	}
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Env = env

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("mcp %q stdin pipe: %w", name, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("mcp %q stdout pipe: %w", name, err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("mcp %q stderr pipe: %w", name, err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("mcp %q start: %w", name, err)
	}

	c := &StdioClient{
		name:    name,
		cmd:     cmd,
		enc:     json.NewEncoder(stdin),
		dec:     json.NewDecoder(stdout),
		pending: map[string]chan jsonrpcResponse{},
		done:    make(chan struct{}),
		logCap:  logCap(),
	}
	go c.readPump()
	go c.drainStderr(stderrPipe)
	return c, nil
}

// drainStderr reads stderr line by line into the ring buffer.
func (c *StdioClient) drainStderr(r io.Reader) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		c.appendLog(sc.Text())
	}
}

// readPump drains the server's stdout, routing id-less notifications to the
// router and fanning id-keyed responses to waiting callers.
func (c *StdioClient) readPump() {
	defer c.closeAll()
	for {
		var frame jsonrpcInbound
		if err := c.dec.Decode(&frame); err != nil {
			if err != io.EOF {
				// swallow — server exited or closed stdout
			}
			return
		}
		// An id-less notification (e.g. "$/progress") is routed BEFORE the pending
		// lookup — it has no id, so the old code dropped it (the load-bearing bug).
		if frame.isNotification() {
			c.mu.Lock()
			router := c.router
			c.mu.Unlock()
			if router != nil {
				router.dispatchNotification(c.name, frame.Method, frame.Params)
			}
			continue
		}
		resp := frame.response()
		c.mu.Lock()
		ch, ok := c.pending[resp.ID]
		if ok {
			delete(c.pending, resp.ID)
		}
		c.mu.Unlock()
		if ok {
			ch <- resp
		}
	}
}

// closeAll closes all pending channels so callers unblock on server crash.
func (c *StdioClient) closeAll() {
	c.once.Do(func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		for id, ch := range c.pending {
			close(ch)
			delete(c.pending, id)
		}
		close(c.done)
	})
}

// call sends a JSON-RPC request and waits for the matching response.
func (c *StdioClient) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := fmt.Sprintf("%d", c.counter.Add(1))
	ch := make(chan jsonrpcResponse, 1)

	c.mu.Lock()
	select {
	case <-c.done:
		c.mu.Unlock()
		return nil, fmt.Errorf("mcp server %q is closed", c.name)
	default:
	}
	c.pending[id] = ch
	req := jsonrpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params}
	if err := c.enc.Encode(req); err != nil {
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, fmt.Errorf("mcp %q send: %w", c.name, err)
	}
	c.mu.Unlock()

	select {
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, ctx.Err()
	case resp, ok := <-ch:
		if !ok {
			return nil, fmt.Errorf("mcp server %q closed", c.name)
		}
		if resp.Error != nil {
			return nil, fmt.Errorf("mcp %q: %w", c.name, &RPCError{Code: resp.Error.Code, Message: resp.Error.Message})
		}
		return resp.Result, nil
	}
}

// notify sends a JSON-RPC notification (no id, no response awaited).
func (c *StdioClient) notify(_ context.Context, method string, params any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	select {
	case <-c.done:
		return fmt.Errorf("mcp server %q is closed", c.name)
	default:
	}
	if err := c.enc.Encode(jsonrpcNotification{JSONRPC: "2.0", Method: method, Params: params}); err != nil {
		return fmt.Errorf("mcp %q notify: %w", c.name, err)
	}
	return nil
}

// stop terminates the server process.
func (c *StdioClient) stop() {
	c.closeAll()
	_ = c.cmd.Process.Kill()
	_ = c.cmd.Wait()
}
