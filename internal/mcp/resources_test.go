package mcp

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/ethanhinson/fuse/internal/tools"
)

// recordingConn is an in-package mcpConn double: it records the methods/params it was
// called with and returns a canned result (or error) per method. It satisfies
// mcpConn so a Manager can be driven without a real transport.
type recordingConn struct {
	mu      sync.Mutex
	calls   []recordedCall
	results map[string]json.RawMessage
	errs    map[string]error
	notes   []recordedCall
}

type recordedCall struct {
	method string
	params any
}

func newRecordingConn() *recordingConn {
	return &recordingConn{results: map[string]json.RawMessage{}, errs: map[string]error{}}
}

func (f *recordingConn) call(_ context.Context, method string, params any) (json.RawMessage, error) {
	f.mu.Lock()
	f.calls = append(f.calls, recordedCall{method: method, params: params})
	res := f.results[method]
	err := f.errs[method]
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if res == nil {
		return json.RawMessage(`{}`), nil
	}
	return res, nil
}

func (f *recordingConn) notify(_ context.Context, method string, params any) error {
	f.mu.Lock()
	f.notes = append(f.notes, recordedCall{method: method, params: params})
	f.mu.Unlock()
	return nil
}

func (f *recordingConn) stop() {}

func (f *recordingConn) methodCalls(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c.method == method {
			n++
		}
	}
	return n
}

// managerWithConn builds a Manager with one managed server backed by conn and
// the given advertised capabilities (raw JSON of the capabilities object).
func managerWithConn(t *testing.T, server string, conn mcpConn, capsJSON string) *Manager {
	t.Helper()
	m, _ := NewManager(nil, tools.NewRegistry())
	caps := ServerCapabilities{raw: map[string]json.RawMessage{}}
	if capsJSON != "" {
		var raw map[string]json.RawMessage
		if err := json.Unmarshal([]byte(capsJSON), &raw); err != nil {
			t.Fatalf("bad capsJSON: %v", err)
		}
		caps = ServerCapabilities{raw: raw}
	}
	m.mu.Lock()
	m.servers[server] = &managedServer{conn: conn, caps: caps}
	m.mu.Unlock()
	return m
}

// TestListReadResources: ListResources returns advertised resources; a server
// advertising none returns empty (no error); ReadResource returns content blocks.
func TestListReadResources(t *testing.T) {
	conn := newRecordingConn()
	conn.results["resources/list"] = json.RawMessage(`{"resources":[
		{"uri":"fuse://tools","name":"Tool catalog","mimeType":"application/json"},
		{"uri":"file:///a.txt","name":"a"}
	]}`)
	conn.results["resources/read"] = json.RawMessage(`{"contents":[
		{"uri":"fuse://tools","mimeType":"application/json","text":"{\"tools\":[]}"}
	]}`)

	m := managerWithConn(t, "srv", conn, "")
	defer m.Close()

	res, err := m.ListResources(context.Background(), "srv")
	if err != nil {
		t.Fatalf("ListResources: %v", err)
	}
	if len(res) != 2 {
		t.Fatalf("ListResources returned %d, want 2", len(res))
	}
	if res[0].URI != "fuse://tools" || res[0].Name != "Tool catalog" || res[0].MIMEType != "application/json" {
		t.Errorf("resource[0] = %+v, want fuse://tools/Tool catalog/application/json", res[0])
	}

	rc, err := m.ReadResource(context.Background(), "srv", "fuse://tools")
	if err != nil {
		t.Fatalf("ReadResource: %v", err)
	}
	if len(rc.Contents) != 1 {
		t.Fatalf("ReadResource returned %d content blocks, want 1", len(rc.Contents))
	}
	if rc.Contents[0].URI != "fuse://tools" || rc.Contents[0].Text == "" {
		t.Errorf("content block = %+v, want fuse://tools with text", rc.Contents[0])
	}
	// The read params must carry the requested uri.
	if got := readParamURI(t, conn); got != "fuse://tools" {
		t.Errorf("resources/read uri param = %q, want fuse://tools", got)
	}
}

// TestListResourcesEmptyIsNotError: a server whose resources/list returns no
// resources (or an empty object) yields an empty slice and no error (fail-open).
func TestListResourcesEmptyIsNotError(t *testing.T) {
	conn := newRecordingConn()
	conn.results["resources/list"] = json.RawMessage(`{"resources":[]}`)
	m := managerWithConn(t, "srv", conn, "")
	defer m.Close()

	res, err := m.ListResources(context.Background(), "srv")
	if err != nil {
		t.Fatalf("empty list should not error: %v", err)
	}
	if len(res) != 0 {
		t.Errorf("empty list returned %d resources, want 0", len(res))
	}
}

// readParamURI extracts the "uri" field from the last resources/read call params.
func readParamURI(t *testing.T, conn *recordingConn) string {
	t.Helper()
	conn.mu.Lock()
	defer conn.mu.Unlock()
	for i := len(conn.calls) - 1; i >= 0; i-- {
		if conn.calls[i].method != "resources/read" {
			continue
		}
		b, _ := json.Marshal(conn.calls[i].params)
		var p struct {
			URI string `json:"uri"`
		}
		_ = json.Unmarshal(b, &p)
		return p.URI
	}
	return ""
}
