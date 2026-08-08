package mcp

import (
	"context"
	"encoding/json"
	"testing"
)

// TestSubscribeGatedByCapability: a server advertising resources.subscribe sends
// resources/subscribe; a server that does NOT advertise it returns an error on an
// explicit Subscribe (D4 — fail-open: list/read still work, only an explicit
// subscribe attempt errors).
func TestSubscribeGatedByCapability(t *testing.T) {
	// Advertises resources.subscribe.
	conn := newRecordingConn()
	m := managerWithConn(t, "srv", conn, `{"resources":{"subscribe":true}}`)
	defer m.Close()

	if err := m.Subscribe(context.Background(), "srv", "fuse://tools"); err != nil {
		t.Fatalf("Subscribe on capable server: %v", err)
	}
	if conn.methodCalls("resources/subscribe") != 1 {
		t.Errorf("resources/subscribe sent %d times, want 1", conn.methodCalls("resources/subscribe"))
	}

	// Does NOT advertise resources.subscribe.
	conn2 := newRecordingConn()
	m2 := managerWithConn(t, "srv", conn2, `{"resources":{}}`)
	defer m2.Close()

	if err := m2.Subscribe(context.Background(), "srv", "fuse://tools"); err == nil {
		t.Error("Subscribe on incapable server should return an error (explicit attempt)")
	}
	if conn2.methodCalls("resources/subscribe") != 0 {
		t.Errorf("incapable server should not send resources/subscribe, got %d", conn2.methodCalls("resources/subscribe"))
	}
	// Fail-open: list/read still work on the incapable server.
	conn2.results["resources/list"] = []byte(`{"resources":[]}`)
	if _, err := m2.ListResources(context.Background(), "srv"); err != nil {
		t.Errorf("ListResources should still work on incapable server: %v", err)
	}
}

// TestSubscribeRefCount: two Subscribe + one Unsubscribe keeps the URI
// subscribed (no resources/unsubscribe sent yet); the second Unsubscribe releases
// it (one resources/unsubscribe sent).
func TestSubscribeRefCount(t *testing.T) {
	conn := newRecordingConn()
	m := managerWithConn(t, "srv", conn, `{"resources":{"subscribe":true}}`)
	defer m.Close()
	ctx := context.Background()

	if err := m.Subscribe(ctx, "srv", "fuse://tools"); err != nil {
		t.Fatalf("Subscribe 1: %v", err)
	}
	if err := m.Subscribe(ctx, "srv", "fuse://tools"); err != nil {
		t.Fatalf("Subscribe 2: %v", err)
	}
	// A second subscribe of the same URI must NOT re-send the wire subscribe.
	if got := conn.methodCalls("resources/subscribe"); got != 1 {
		t.Errorf("resources/subscribe sent %d times for two Subscribe calls, want 1", got)
	}

	if err := m.Unsubscribe(ctx, "srv", "fuse://tools"); err != nil {
		t.Fatalf("Unsubscribe 1: %v", err)
	}
	if got := conn.methodCalls("resources/unsubscribe"); got != 0 {
		t.Errorf("first Unsubscribe of a ref-count-2 URI must not send unsubscribe, got %d", got)
	}

	if err := m.Unsubscribe(ctx, "srv", "fuse://tools"); err != nil {
		t.Fatalf("Unsubscribe 2: %v", err)
	}
	if got := conn.methodCalls("resources/unsubscribe"); got != 1 {
		t.Errorf("final Unsubscribe must send resources/unsubscribe once, got %d", got)
	}
}

// TestResubscribeOnReconnect: tracked URIs are re-subscribed after a reconnect
// (a new connection replaces the old one). resubscribeAll re-sends subscribe for
// every tracked URI on the fresh connection.
func TestResubscribeOnReconnect(t *testing.T) {
	conn := newRecordingConn()
	m := managerWithConn(t, "srv", conn, `{"resources":{"subscribe":true}}`)
	defer m.Close()
	ctx := context.Background()

	if err := m.Subscribe(ctx, "srv", "fuse://tools"); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if err := m.Subscribe(ctx, "srv", "file:///a"); err != nil {
		t.Fatalf("Subscribe 2: %v", err)
	}

	// Simulate a reconnect: swap in a fresh connection, then resubscribe.
	fresh := newRecordingConn()
	m.mu.Lock()
	m.servers["srv"].conn = fresh
	m.mu.Unlock()

	if err := m.resubscribeAll(ctx, "srv"); err != nil {
		t.Fatalf("resubscribeAll: %v", err)
	}
	if got := fresh.methodCalls("resources/subscribe"); got != 2 {
		t.Errorf("reconnect re-subscribed %d URIs, want 2", got)
	}
}

// TestStopClearsSubscriptionState: Stop must delete the server's subRefs and
// staleURIs entries (S3). Otherwise a Stop/Add cycle leaves a non-zero refcount,
// and the re-added server's first Subscribe sees the URI as already-referenced
// and skips the wire resources/subscribe.
func TestStopClearsSubscriptionState(t *testing.T) {
	conn := newRecordingConn()
	m := managerWithConn(t, "srv", conn, `{"resources":{"subscribe":true}}`)
	defer m.Close()
	ctx := context.Background()

	if err := m.Subscribe(ctx, "srv", "fuse://tools"); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	m.markStale("srv", "fuse://tools")

	// Sanity: state is present before Stop.
	m.subMu.Lock()
	_, hadRef := m.subRefs["srv"]
	m.subMu.Unlock()
	if !hadRef {
		t.Fatal("precondition: subRefs[srv] should exist after Subscribe")
	}
	if !m.IsStale("srv", "fuse://tools") {
		t.Fatal("precondition: URI should be stale after markStale")
	}

	if err := m.Stop("srv"); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	m.subMu.Lock()
	_, stillRef := m.subRefs["srv"]
	m.subMu.Unlock()
	if stillRef {
		t.Error("Stop did not clear subRefs[srv]")
	}
	m.resourceMu.Lock()
	_, stillStale := m.staleURIs["srv"]
	m.resourceMu.Unlock()
	if stillStale {
		t.Error("Stop did not clear staleURIs[srv]")
	}

	// After a Stop/Add cycle, a fresh Subscribe must send the wire subscribe
	// again (refcount was reset, not left dangling at 1).
	conn2 := newRecordingConn()
	m.mu.Lock()
	m.servers["srv"] = &managedServer{conn: conn2, caps: ServerCapabilities{raw: mustCapsRaw(t, `{"resources":{"subscribe":true}}`)}}
	m.mu.Unlock()
	if err := m.Subscribe(ctx, "srv", "fuse://tools"); err != nil {
		t.Fatalf("re-Subscribe after Stop: %v", err)
	}
	if got := conn2.methodCalls("resources/subscribe"); got != 1 {
		t.Errorf("re-Subscribe after Stop sent resources/subscribe %d times, want 1", got)
	}
}

func mustCapsRaw(t *testing.T, capsJSON string) map[string]json.RawMessage {
	t.Helper()
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(capsJSON), &raw); err != nil {
		t.Fatalf("bad capsJSON: %v", err)
	}
	return raw
}
