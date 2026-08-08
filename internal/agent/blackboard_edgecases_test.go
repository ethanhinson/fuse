package agent

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// TestKeysNoMatchIsEmptyNotNull pins the tool-visible contract: Keys returns a
// non-nil empty slice for a no-match, so blackboard_keys marshals it to "[]" and
// never the ambiguous "null" (which a model could misread as an error). The real
// store uses make([]string,0,…); this guards against a regression to a nil return.
func TestKeysNoMatchIsEmptyNotNull(t *testing.T) {
	bb := NewBlackboard(nil)
	bb.Put("alpha", 1, "x", "X")
	for _, pat := range []string{"zzz*", "["} { // valid-no-match and malformed
		got := bb.Keys(pat)
		if got == nil {
			t.Fatalf("Keys(%q) returned nil; must be a non-nil empty slice", pat)
		}
		if b, _ := json.Marshal(got); string(b) != "[]" {
			t.Fatalf("Keys(%q) marshals to %s, want []", pat, string(b))
		}
	}
}

// Edge-case regressions found by the change-0023 coverage audit. Each closes a
// specific untested branch or documents an exact behavioral guarantee.

// TestWaitSignalledButNotSet exercises the rare branch in Wait where a Put wakes
// the waiter (closing its channel) but a Delete removes the entry before the
// woken waiter re-reads it — the "signalled but not set" path (blackboard.go
// ~line 166). It must return the specific error, never panic or hang.
func TestWaitSignalledButNotSet(t *testing.T) {
	bb := NewBlackboard(nil)
	done := make(chan error, 1)
	go func() {
		_, err := bb.Wait(context.Background(), "k", 2*time.Second, nil)
		done <- err
	}()
	time.Sleep(30 * time.Millisecond) // let the waiter register and block

	// Reproduce the race deterministically under the lock: signal the waiter, then
	// remove the entry before it can re-read — exactly the Put-then-Delete window.
	bb.mu.Lock()
	bb.entries["k"] = BlackboardEntry{Value: "v"}
	for _, ch := range bb.waiters["k"] {
		close(ch)
	}
	delete(bb.waiters, "k")
	delete(bb.entries, "k")
	bb.mu.Unlock()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected 'signalled but not set' error, got nil (a value?)")
		}
		if got := err.Error(); got != `blackboard: key "k" signalled but not set` {
			t.Fatalf("unexpected error: %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Wait hung on the signalled-but-not-set race")
	}
}

// TestWaitZeroAndNegativeTimeoutAtStore documents store-level Wait behavior for a
// zero/negative timeout (the tool layer rejects these before they reach the
// store, but the store must still be well-defined): time.After(<=0) fires
// immediately, so Wait returns a timeout error rather than blocking forever.
func TestWaitZeroAndNegativeTimeoutAtStore(t *testing.T) {
	bb := NewBlackboard(nil)
	for _, d := range []time.Duration{0, -1 * time.Second} {
		start := time.Now()
		v, err := bb.Wait(context.Background(), "never", d, nil)
		if err == nil {
			t.Fatalf("timeout=%s: expected timeout error, got value %v", d, v)
		}
		if time.Since(start) > 500*time.Millisecond {
			t.Fatalf("timeout=%s: Wait blocked instead of returning promptly", d)
		}
	}
}

// TestWaitCtxAlreadyCancelled covers Wait entered with an already-cancelled ctx
// on an unset key: it must return promptly with ctx.Err(), not block.
func TestWaitCtxAlreadyCancelled(t *testing.T) {
	bb := NewBlackboard(nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	_, err := bb.Wait(ctx, "never", time.Minute, nil)
	if err != context.Canceled {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Fatal("Wait blocked despite an already-cancelled ctx")
	}
}

// TestKeysMalformedGlobMatchesNothing covers the path.Match ErrBadPattern branch:
// a syntactically invalid glob must match nothing (and not error out), per the
// Keys doc contract.
func TestKeysMalformedGlobMatchesNothing(t *testing.T) {
	bb := NewBlackboard(nil)
	bb.Put("alpha", 1, "x", "X")
	bb.Put("beta", 2, "x", "X")
	if got := bb.Keys("["); len(got) != 0 { // unclosed char class => ErrBadPattern
		t.Fatalf("malformed glob should match nothing, got %v", got)
	}
	// A valid glob still works alongside.
	if got := bb.Keys("al*"); len(got) != 1 || got[0] != "alpha" {
		t.Fatalf("valid glob broke: %v", got)
	}
}

// TestPutNilValueRoundTrips covers a JSON-null value (the tool writes nil when it
// parses "null"): the key is present with a nil Value and correct provenance —
// distinct from a missing key.
func TestPutNilValueRoundTrips(t *testing.T) {
	bb := NewBlackboard(nil)
	bb.Put("k", nil, "id-1", "writer-1")
	e, ok := bb.Get("k")
	if !ok {
		t.Fatal("a nil-value key must still be present (distinct from missing)")
	}
	if e.Value != nil {
		t.Fatalf("Value = %v, want nil", e.Value)
	}
	if e.WriterLabel != "writer-1" {
		t.Fatalf("provenance lost on nil value: %q", e.WriterLabel)
	}
}

// TestSnapshotSharesNestedReference documents the ACTUAL Snapshot guarantee: the
// returned map is an independent top-level copy (safe to insert/delete/replace
// entries), but a nested reference value (map/slice, as produced by JSON object/
// array writes) is SHARED with the store — Snapshot is a shallow copy, not deep.
// This is safe for the only consumer (the read-only TUI). The test pins the
// behavior so a future mutator of a snapshot value is caught here rather than as
// a data race in production.
func TestSnapshotSharesNestedReference(t *testing.T) {
	bb := NewBlackboard(nil)
	nested := map[string]any{"n": 1}
	bb.Put("k", nested, "x", "X")

	snap := bb.Snapshot()

	// Top-level independence holds.
	delete(snap, "k")
	if _, ok := bb.Get("k"); !ok {
		t.Fatal("top-level: deleting from snapshot mutated the store")
	}

	// Nested value is shared (shallow copy): mutating it via the store is visible
	// through a value obtained from a fresh snapshot. This documents the shallow
	// contract; do NOT mutate snapshot values in production code.
	snap2 := bb.Snapshot()
	got := snap2["k"].Value.(map[string]any)
	nested["added"] = 2 // mutate the original via the store's retained reference
	if _, present := got["added"]; !present {
		t.Fatal("expected shared nested reference (shallow Snapshot); got a deep copy — update the Snapshot doc accordingly")
	}
}
