// Package mcp provides a stdio-transport MCP client (JSON-RPC 2.0).
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"sync/atomic"
)

// jsonrpcRequest is a JSON-RPC 2.0 request frame.
type jsonrpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      string `json:"id"`
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
	}
	go c.readPump()
	return c, nil
}

// readPump drains the server's stdout and fans responses to waiting callers.
func (c *StdioClient) readPump() {
	defer c.closeAll()
	for {
		var resp jsonrpcResponse
		if err := c.dec.Decode(&resp); err != nil {
			if err != io.EOF {
				// swallow — server exited or closed stdout
			}
			return
		}
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
			return nil, fmt.Errorf("mcp %q: %s", c.name, resp.Error.Message)
		}
		return resp.Result, nil
	}
}

// stop terminates the server process.
func (c *StdioClient) stop() {
	c.closeAll()
	_ = c.cmd.Process.Kill()
	_ = c.cmd.Wait()
}
