package mcp

import (
	"context"
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
