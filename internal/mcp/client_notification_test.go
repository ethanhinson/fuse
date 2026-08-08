package mcp

import (
	"encoding/json"
	"io"
	"sync"
	"testing"
	"time"
)

// recordingRouter captures dispatched notifications for assertions.
type recordingRouter struct {
	mu     sync.Mutex
	server []string
	method []string
	params []json.RawMessage
}

func (r *recordingRouter) dispatchNotification(server, method string, params json.RawMessage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.server = append(r.server, server)
	r.method = append(r.method, method)
	r.params = append(r.params, params)
}

func (r *recordingRouter) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.method)
}

// newPipeStdioClient builds a StdioClient whose read pump consumes r, without a
// real subprocess. Writes to w are seen by the pump.
func newPipeStdioClient(t *testing.T, name string, router notificationRouter) (*StdioClient, io.WriteCloser) {
	t.Helper()
	pr, pw := io.Pipe()
	c := &StdioClient{
		name:    name,
		dec:     json.NewDecoder(pr),
		pending: map[string]chan jsonrpcResponse{},
		done:    make(chan struct{}),
		router:  router,
	}
	go c.readPump()
	return c, pw
}

// TestReadPumpRoutesIDlessNotification feeds the pump an id-less $/progress frame
// and asserts it reaches the registered router with the client's server name and
// raw params.
func TestReadPumpRoutesIDlessNotification(t *testing.T) {
	router := &recordingRouter{}
	c, w := newPipeStdioClient(t, "srv-a", router)
	defer c.closeAll()

	frame := `{"jsonrpc":"2.0","method":"$/progress","params":{"progressToken":"tok1","progress":0.5}}` + "\n"
	if _, err := io.WriteString(w, frame); err != nil {
		t.Fatalf("write frame: %v", err)
	}

	waitFor(t, func() bool { return router.count() == 1 })

	router.mu.Lock()
	defer router.mu.Unlock()
	if router.server[0] != "srv-a" {
		t.Errorf("server = %q, want srv-a", router.server[0])
	}
	if router.method[0] != "$/progress" {
		t.Errorf("method = %q, want $/progress", router.method[0])
	}
	var p struct {
		ProgressToken string  `json:"progressToken"`
		Progress      float64 `json:"progress"`
	}
	if err := json.Unmarshal(router.params[0], &p); err != nil {
		t.Fatalf("unmarshal params %q: %v", router.params[0], err)
	}
	if p.ProgressToken != "tok1" || p.Progress != 0.5 {
		t.Errorf("params = %+v, want tok1/0.5", p)
	}
}

// TestReadPumpStillResolvesResponses is the regression guard: an id-keyed
// response must still fan to its pending channel unchanged, and must NOT reach
// the notification router.
func TestReadPumpStillResolvesResponses(t *testing.T) {
	router := &recordingRouter{}
	c, w := newPipeStdioClient(t, "srv-b", router)
	defer c.closeAll()

	ch := make(chan jsonrpcResponse, 1)
	c.mu.Lock()
	c.pending["7"] = ch
	c.mu.Unlock()

	frame := `{"jsonrpc":"2.0","id":"7","result":{"ok":true}}` + "\n"
	if _, err := io.WriteString(w, frame); err != nil {
		t.Fatalf("write frame: %v", err)
	}

	select {
	case resp := <-ch:
		if string(resp.Result) != `{"ok":true}` {
			t.Errorf("result = %s, want {\"ok\":true}", resp.Result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pending response never resolved")
	}
	if router.count() != 0 {
		t.Errorf("id-keyed response must not reach the router, got %d dispatches", router.count())
	}
}

// TestReadPumpMalformedNotificationDoesNotPanic feeds an id-less frame with a
// method but malformed params; the pump must not panic and must keep serving
// subsequent frames.
func TestReadPumpMalformedNotificationDoesNotPanic(t *testing.T) {
	router := &recordingRouter{}
	c, w := newPipeStdioClient(t, "srv-c", router)
	defer c.closeAll()

	// An unknown-method id-less frame is still routed (the router decides what to
	// do with unknown methods) — but a totally malformed line must not kill the
	// pump. Send a valid notification after a garbage line to prove liveness.
	if _, err := io.WriteString(w, `{"jsonrpc":"2.0","method":"weird/thing"}`+"\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := io.WriteString(w, `{"jsonrpc":"2.0","method":"$/progress","params":{"progressToken":"t"}}`+"\n"); err != nil {
		t.Fatalf("write: %v", err)
	}

	waitFor(t, func() bool { return router.count() == 2 })
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition not met within deadline")
}
