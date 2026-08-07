package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// httpClient implements mcpConn over HTTP/SSE transport (MCP 2024-11-05).
//
// Protocol:
//  1. Client GETs {baseURL}/sse with Accept: text/event-stream
//  2. Server sends an "endpoint" SSE event whose data is the POST URL
//  3. Client POSTs JSON-RPC to that messages URL
//  4. Responses arrive as data events on the SSE stream
type httpClient struct {
	name        string
	bearerToken string
	http        *http.Client
	messagesURL string
	sseBody     io.ReadCloser
	mu          sync.Mutex
	pending     map[string]chan jsonrpcResponse
	counter     atomic.Uint64
	done        chan struct{}
	once        sync.Once
}

// newHTTPClient connects to the MCP SSE endpoint and waits for the endpoint event.
func newHTTPClient(name, baseURL, bearerToken string) (*httpClient, error) {
	sseURL := strings.TrimRight(baseURL, "/") + "/sse"

	req, err := http.NewRequest(http.MethodGet, sseURL, nil)
	if err != nil {
		return nil, fmt.Errorf("mcp http %q: build sse request: %w", name, err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")
	if bearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+bearerToken)
	}

	hc := &http.Client{Timeout: 0} // no global timeout; per-call contexts handle timeouts
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mcp http %q: SSE connect: %w", name, err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("mcp http %q: SSE connect: HTTP %d", name, resp.StatusCode)
	}

	// Wait for the endpoint event (10s timeout).
	messagesURL, err := readEndpointEvent(resp.Body, 10*time.Second)
	if err != nil {
		resp.Body.Close()
		return nil, fmt.Errorf("mcp http %q: %w", name, err)
	}

	// If the messages URL is relative, resolve it against baseURL.
	if strings.HasPrefix(messagesURL, "/") {
		u := strings.TrimRight(baseURL, "/")
		// Find the scheme+host portion.
		if idx := strings.Index(u[8:], "/"); idx >= 0 {
			messagesURL = u[:8+idx] + messagesURL
		} else {
			messagesURL = u + messagesURL
		}
	}

	c := &httpClient{
		name:        name,
		bearerToken: bearerToken,
		http:        hc,
		messagesURL: messagesURL,
		sseBody:     resp.Body,
		pending:     make(map[string]chan jsonrpcResponse),
		done:        make(chan struct{}),
	}
	go c.readSSEPump()
	return c, nil
}

// readEndpointEvent reads lines from an SSE stream until it finds an "endpoint" event.
func readEndpointEvent(body io.Reader, timeout time.Duration) (string, error) {
	type result struct {
		url string
		err error
	}
	ch := make(chan result, 1)

	go func() {
		scanner := bufio.NewScanner(body)
		var eventType string
		var dataLines []string
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "event:") {
				eventType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
				continue
			}
			if strings.HasPrefix(line, "data:") {
				data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
				dataLines = append(dataLines, data)
				continue
			}
			if line == "" {
				// Event boundary.
				if eventType == "endpoint" && len(dataLines) > 0 {
					ch <- result{url: strings.Join(dataLines, "\n")}
					return
				}
				// Reset for next event.
				eventType = ""
				dataLines = dataLines[:0]
			}
		}
		if err := scanner.Err(); err != nil {
			ch <- result{err: fmt.Errorf("SSE read: %w", err)}
		} else {
			ch <- result{err: fmt.Errorf("SSE stream closed before endpoint event")}
		}
	}()

	select {
	case r := <-ch:
		return r.url, r.err
	case <-time.After(timeout):
		return "", fmt.Errorf("timeout waiting for SSE endpoint event")
	}
}

// call sends a JSON-RPC request over HTTP and waits for the SSE response.
func (c *httpClient) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := fmt.Sprintf("%d", c.counter.Add(1))
	ch := make(chan jsonrpcResponse, 1)

	c.mu.Lock()
	select {
	case <-c.done:
		c.mu.Unlock()
		return nil, fmt.Errorf("mcp http server %q is closed", c.name)
	default:
	}
	c.pending[id] = ch
	c.mu.Unlock()

	req := jsonrpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params}
	body, err := json.Marshal(req)
	if err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, fmt.Errorf("mcp http %q marshal: %w", c.name, err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.messagesURL, bytes.NewReader(body))
	if err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, fmt.Errorf("mcp http %q build request: %w", c.name, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.bearerToken != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.bearerToken)
	}

	httpResp, err := c.http.Do(httpReq)
	if err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, fmt.Errorf("mcp http %q post: %w", c.name, err)
	}
	// Drain and close the POST response body — the actual JSON-RPC response
	// comes back via the SSE stream, not the POST response body.
	io.Copy(io.Discard, httpResp.Body) //nolint:errcheck
	httpResp.Body.Close()

	if httpResp.StatusCode != http.StatusOK && httpResp.StatusCode != http.StatusAccepted {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, fmt.Errorf("mcp http %q: POST returned HTTP %d", c.name, httpResp.StatusCode)
	}

	// Wait for the response to arrive via the SSE pump.
	select {
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, ctx.Err()
	case resp, ok := <-ch:
		if !ok {
			return nil, fmt.Errorf("mcp http server %q closed", c.name)
		}
		if resp.Error != nil {
			return nil, fmt.Errorf("mcp http %q: %w", c.name, &RPCError{Code: resp.Error.Code, Message: resp.Error.Message})
		}
		return resp.Result, nil
	}
}

// notify sends a JSON-RPC notification over HTTP (no id, no response awaited).
func (c *httpClient) notify(ctx context.Context, method string, params any) error {
	c.mu.Lock()
	select {
	case <-c.done:
		c.mu.Unlock()
		return fmt.Errorf("mcp http server %q is closed", c.name)
	default:
	}
	c.mu.Unlock()

	body, err := json.Marshal(jsonrpcNotification{JSONRPC: "2.0", Method: method, Params: params})
	if err != nil {
		return fmt.Errorf("mcp http %q marshal notify: %w", c.name, err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.messagesURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("mcp http %q build notify: %w", c.name, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.bearerToken != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.bearerToken)
	}
	httpResp, err := c.http.Do(httpReq)
	if err != nil {
		return fmt.Errorf("mcp http %q notify post: %w", c.name, err)
	}
	io.Copy(io.Discard, httpResp.Body) //nolint:errcheck
	httpResp.Body.Close()
	if httpResp.StatusCode != http.StatusOK && httpResp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("mcp http %q: notify POST returned HTTP %d", c.name, httpResp.StatusCode)
	}
	return nil
}

// readSSEPump reads the SSE stream and fans JSON-RPC responses to pending callers.
func (c *httpClient) readSSEPump() {
	defer c.closeAll()
	scanner := bufio.NewScanner(c.sseBody)
	var dataLines []string
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data:") {
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			dataLines = append(dataLines, data)
			continue
		}
		// Skip event:, id:, retry: lines — only care about data.
		if line == "" {
			if len(dataLines) == 0 {
				continue
			}
			raw := strings.Join(dataLines, "\n")
			dataLines = dataLines[:0]

			var resp jsonrpcResponse
			if err := json.Unmarshal([]byte(raw), &resp); err != nil {
				continue // not a JSON-RPC response (e.g. a notification); skip
			}
			if resp.ID == "" {
				continue // notification — no pending channel
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
}

// closeAll unblocks all pending callers when the SSE stream closes.
func (c *httpClient) closeAll() {
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

// stop closes the SSE connection and unblocks all pending calls.
func (c *httpClient) stop() {
	c.sseBody.Close()
	c.closeAll()
}
