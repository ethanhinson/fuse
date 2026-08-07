package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethanhinson/fuse/internal/config"
)

// StreamableHTTPClient implements mcpConn over the MCP v2025-03-26 Streamable
// HTTP transport.
//
// Unlike httpClient (the older HTTP/SSE transport), Streamable HTTP is
// request-scoped: there is a single endpoint (baseURL), each JSON-RPC request is
// a POST, and the server answers a POST *either* immediately with
// Content-Type: application/json (one synchronous response) *or* with
// Content-Type: text/event-stream (a chunked SSE stream). There is therefore no
// persistent background read pump and no pending-response map — each call() owns
// its own response.
//
// The MCP init handshake is driven transport-agnostically by handshakeAndDiscover
// (see manager.go); this client does not own an initialize exchange. It captures
// the server's Mcp-Session-Id from the response headers (first seen on the
// initialize response) and echoes it, plus MCP-Protocol-Version, on every
// subsequent request.
type StreamableHTTPClient struct {
	name    string
	baseURL string
	http    *http.Client
	auth    config.MCPAuthConfig // retained for token refresh on a 401
	counter atomic.Uint64

	mu          sync.Mutex // guards bearerToken, sessionID, closed
	bearerToken string
	sessionID   string
	closed      bool
}

const (
	hdrSessionID = "Mcp-Session-Id"
	hdrProtocol  = "MCP-Protocol-Version"
	hdrLastEvent = "Last-Event-Id"

	streamResumeCap = 2 // bounded Last-Event-Id reconnects per call
)

// errStreamClosed signals an SSE response stream ended before the matching
// JSON-RPC response arrived — the trigger for a Last-Event-Id resume attempt.
var errStreamClosed = errors.New("mcp streamable: stream closed before response")

// newStreamableHTTPClient builds a Streamable HTTP client. Construction performs
// no network I/O — the first request is the manager's initialize call.
func newStreamableHTTPClient(name, baseURL, token string, auth config.MCPAuthConfig) (*StreamableHTTPClient, error) {
	return &StreamableHTTPClient{
		name:        name,
		baseURL:     baseURL,
		http:        &http.Client{Timeout: 0}, // per-call ctx governs deadlines
		auth:        auth,
		bearerToken: token,
	}, nil
}

// call sends a JSON-RPC request and returns the result. It branches on the
// response Content-Type: application/json is decoded synchronously; a
// text/event-stream response is drained by a per-call SSE pump until the frame
// whose id matches this request arrives.
func (c *StreamableHTTPClient) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if c.isClosed() {
		return nil, fmt.Errorf("mcp streamable %q is closed", c.name)
	}
	id := fmt.Sprintf("%d", c.counter.Add(1))
	body, err := json.Marshal(jsonrpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params})
	if err != nil {
		return nil, fmt.Errorf("mcp streamable %q marshal: %w", c.name, err)
	}

	resp, err := c.send(ctx, http.MethodPost, body)
	if err != nil {
		return nil, err
	}

	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		result, lastEventID, derr := c.drainStream(resp.Body, id)
		resp.Body.Close()
		if derr == nil {
			return result, nil
		}
		if errors.Is(derr, errStreamClosed) && lastEventID != "" {
			return c.resume(ctx, id, lastEventID, 1)
		}
		return nil, fmt.Errorf("mcp streamable %q: %w", c.name, derr)
	}

	// application/json (or unspecified): a single synchronous response.
	result, derr := decodeJSONResult(c.name, resp.Body, id)
	resp.Body.Close()
	return result, derr
}

// notify sends a JSON-RPC notification (no id, no response awaited).
func (c *StreamableHTTPClient) notify(ctx context.Context, method string, params any) error {
	if c.isClosed() {
		return fmt.Errorf("mcp streamable %q is closed", c.name)
	}
	body, err := json.Marshal(jsonrpcNotification{JSONRPC: "2.0", Method: method, Params: params})
	if err != nil {
		return fmt.Errorf("mcp streamable %q marshal notify: %w", c.name, err)
	}
	resp, err := c.send(ctx, http.MethodPost, body)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, resp.Body) //nolint:errcheck
	resp.Body.Close()
	return nil
}

// stop terminates the session (best-effort DELETE with the session header) and
// marks the client closed. Teardown must not block shutdown, so the DELETE runs
// under a short bounded context and its result is ignored.
func (c *StreamableHTTPClient) stop() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	sess, token := c.sessionID, c.bearerToken
	c.mu.Unlock()

	if sess == "" {
		return // stateless server — nothing to tear down
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.baseURL, nil)
	if err != nil {
		return
	}
	req.Header.Set(hdrSessionID, sess)
	req.Header.Set(hdrProtocol, clientProtocolVersion)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return
	}
	io.Copy(io.Discard, resp.Body) //nolint:errcheck
	resp.Body.Close()
}

// send issues one request with at most one 401-refresh and one 404-reinit retry.
// A fresh *http.Request is built from body on every attempt (the body rewind —
// see the rewind-request-body-on-manual-retry learning), so a retried POST never
// sends an empty body. On success it returns the response with an unread body for
// the caller; any non-2xx after the bounded retries is an error.
func (c *StreamableHTTPClient) send(ctx context.Context, method string, body []byte) (*http.Response, error) {
	resp, err := c.attempt(ctx, method, body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode == http.StatusUnauthorized {
		resp.Body.Close()
		if rerr := c.refreshToken(); rerr != nil {
			return nil, fmt.Errorf("mcp streamable %q: 401 and token refresh failed: %w", c.name, rerr)
		}
		if resp, err = c.attempt(ctx, method, body); err != nil {
			return nil, err
		}
	}

	if resp.StatusCode == http.StatusNotFound && c.sessionSet() {
		resp.Body.Close()
		if rerr := c.reinitialize(ctx); rerr != nil {
			return nil, fmt.Errorf("mcp streamable %q: session expired and reinitialize failed: %w", c.name, rerr)
		}
		if resp, err = c.attempt(ctx, method, body); err != nil {
			return nil, err
		}
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		resp.Body.Close()
		return nil, fmt.Errorf("mcp streamable %q: POST returned HTTP %d", c.name, resp.StatusCode)
	}
	return resp, nil
}

// attempt builds a fresh request (a rewound body) and performs it once, capturing
// any Mcp-Session-Id the response carries. No retry logic — send() owns that.
func (c *StreamableHTTPClient) attempt(ctx context.Context, method string, body []byte) (*http.Response, error) {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL, r)
	if err != nil {
		return nil, fmt.Errorf("mcp streamable %q build request: %w", c.name, err)
	}
	c.setHeaders(req, body != nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mcp streamable %q %s: %w", c.name, method, err)
	}
	c.captureSession(resp)
	return resp, nil
}

// setHeaders applies the common Streamable HTTP request headers under mu.
func (c *StreamableHTTPClient) setHeaders(req *http.Request, hasBody bool) {
	req.Header.Set("Accept", "application/json, text/event-stream")
	if hasBody {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set(hdrProtocol, clientProtocolVersion)

	c.mu.Lock()
	token, sess := c.bearerToken, c.sessionID
	c.mu.Unlock()
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if sess != "" {
		req.Header.Set(hdrSessionID, sess)
	}
}

// captureSession stores the Mcp-Session-Id response header when the server sets
// one (first seen on the initialize response).
func (c *StreamableHTTPClient) captureSession(resp *http.Response) {
	if s := resp.Header.Get(hdrSessionID); s != "" {
		c.mu.Lock()
		c.sessionID = s
		c.mu.Unlock()
	}
}

// refreshToken re-runs the OAuth2 flow after a 401 and swaps in the new token.
func (c *StreamableHTTPClient) refreshToken() error {
	token, err := GetAccessToken(c.name, c.baseURL, c.auth)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.bearerToken = token
	c.mu.Unlock()
	return nil
}

// reinitialize refreshes an expired session: it clears the stored id and POSTs a
// fresh initialize (reusing initializeParams), capturing the new Mcp-Session-Id.
// It uses attempt() directly (one shot, no retry) to avoid recursing through
// send()'s 404 path.
func (c *StreamableHTTPClient) reinitialize(ctx context.Context) error {
	c.mu.Lock()
	c.sessionID = ""
	c.mu.Unlock()

	body, err := json.Marshal(jsonrpcRequest{
		JSONRPC: "2.0",
		ID:      fmt.Sprintf("%d", c.counter.Add(1)),
		Method:  "initialize",
		Params:  initializeParams(),
	})
	if err != nil {
		return fmt.Errorf("marshal initialize: %w", err)
	}
	resp, err := c.attempt(ctx, http.MethodPost, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("reinitialize returned HTTP %d", resp.StatusCode)
	}
	io.Copy(io.Discard, resp.Body) //nolint:errcheck — only the session header matters
	return nil
}

// drainStream parses an SSE response body, returning the result of the frame
// whose id matches. Id-less frames and frames bearing a different id are handed
// to handleServerFrame (the inbound-notification seam) rather than dropped
// silently. The last seen SSE id: is returned for resumability. A stream that
// ends before the matching frame yields errStreamClosed.
func (c *StreamableHTTPClient) drainStream(body io.Reader, id string) (json.RawMessage, string, error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var dataLines []string
	var lastEventID string

	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "id:"):
			lastEventID = strings.TrimSpace(strings.TrimPrefix(line, "id:"))
		case strings.HasPrefix(line, "data:"):
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		case line == "":
			if len(dataLines) == 0 {
				continue
			}
			raw := strings.Join(dataLines, "\n")
			dataLines = dataLines[:0]

			var resp jsonrpcResponse
			if err := json.Unmarshal([]byte(raw), &resp); err != nil {
				continue // not a JSON-RPC frame — skip
			}
			if resp.ID != id {
				// Id-less server notification or a frame for another id — route
				// to the seam (0020/0021 replace this), never correlate here.
				c.handleServerFrame(json.RawMessage(raw))
				continue
			}
			if resp.Error != nil {
				return nil, lastEventID, &RPCError{Code: resp.Error.Code, Message: resp.Error.Message}
			}
			return resp.Result, lastEventID, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, lastEventID, fmt.Errorf("stream read: %w", err)
	}
	return nil, lastEventID, errStreamClosed
}

// resume reconnects a broken response stream via a GET carrying Last-Event-Id,
// bounded by streamResumeCap.
func (c *StreamableHTTPClient) resume(ctx context.Context, id, lastEventID string, depth int) (json.RawMessage, error) {
	if depth > streamResumeCap {
		return nil, fmt.Errorf("mcp streamable %q: stream resume exhausted after %d attempts", c.name, streamResumeCap)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL, nil)
	if err != nil {
		return nil, fmt.Errorf("mcp streamable %q build resume: %w", c.name, err)
	}
	c.setHeaders(req, false)
	req.Header.Set(hdrLastEvent, lastEventID)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mcp streamable %q resume: %w", c.name, err)
	}
	c.captureSession(resp)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("mcp streamable %q: resume returned HTTP %d", c.name, resp.StatusCode)
	}

	result, newLast, derr := c.drainStream(resp.Body, id)
	if derr == nil {
		return result, nil
	}
	if errors.Is(derr, errStreamClosed) && newLast != "" && newLast != lastEventID {
		return c.resume(ctx, id, newLast, depth+1)
	}
	return nil, fmt.Errorf("mcp streamable %q: %w", c.name, derr)
}

// handleServerFrame is the seam for inbound server-initiated frames (id-less
// notifications, or frames for another id) observed on a response stream. Today
// it logs and discards; changes 0020 ($/progress) and 0021 (resource
// subscriptions) replace this one method with real notification routing rather
// than reworking the pump (see the mcp-read-pumps-drop-inbound-notifications
// learning).
func (c *StreamableHTTPClient) handleServerFrame(raw json.RawMessage) {
	log.Printf("[mcp] %s: dropping unrouted server frame (%d bytes) — notification routing lands in changes 0020/0021", c.name, len(raw))
}

// decodeJSONResult decodes a single synchronous JSON-RPC response body.
func decodeJSONResult(name string, body io.Reader, wantID string) (json.RawMessage, error) {
	var resp jsonrpcResponse
	if err := json.NewDecoder(body).Decode(&resp); err != nil {
		return nil, fmt.Errorf("mcp streamable %q decode response: %w", name, err)
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("mcp streamable %q: %w", name, &RPCError{Code: resp.Error.Code, Message: resp.Error.Message})
	}
	return resp.Result, nil
}

func (c *StreamableHTTPClient) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

func (c *StreamableHTTPClient) sessionSet() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sessionID != ""
}
