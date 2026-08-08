package agent

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// --- Second-pass audit gaps (change 0023) --------------------------------------

// TestEmptyStringKeyIsAddressable: the store places no constraint on keys (the
// tool layer rejects ""), so "" is a legal, addressable key at the store level —
// Put/Get/Delete round-trip and every all-keys Keys("")/Keys("*") includes it.
func TestEmptyStringKeyIsAddressable(t *testing.T) {
	bb := NewBlackboard(nil)
	bb.Put("", 1, "id", "lbl")
	bb.Put("other", 2, "id", "lbl")

	e, ok := bb.Get("")
	if !ok || e.Value != 1 || e.WriterLabel != "lbl" {
		t.Fatalf("empty key round-trip failed: %+v ok=%v", e, ok)
	}
	for _, pat := range []string{"", "*"} {
		found := false
		for _, k := range bb.Keys(pat) {
			if k == "" {
				found = true
			}
		}
		if !found {
			t.Fatalf("Keys(%q) omitted the empty-string key", pat)
		}
	}
	bb.Delete("")
	if _, ok := bb.Get(""); ok {
		t.Fatal("Delete(\"\") did not remove the empty key")
	}
}

// TestKeysUnicodeByteOrderStable pins the documented "sorted, stable" Keys
// contract for multibyte keys: sort.Strings is bytewise (UTF-8 code-unit) order,
// deterministic and stable across calls.
func TestKeysUnicodeByteOrderStable(t *testing.T) {
	bb := NewBlackboard(nil)
	for _, k := range []string{"z", "e", "é", "😀", "a"} {
		bb.Put(k, 1, "i", "l")
	}
	first := bb.Keys("*")
	second := bb.Keys("*")
	if len(first) != 5 {
		t.Fatalf("expected 5 keys, got %v", first)
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("Keys not stable across calls at %d: %q vs %q", i, first[i], second[i])
		}
	}
	// Bytewise: ASCII 'a','e','z' sort before multibyte 'é'(0xC3..) and emoji(0xF0..).
	if first[0] != "a" || first[1] != "e" || first[2] != "z" {
		t.Fatalf("expected ASCII keys first in bytewise order, got %v", first)
	}
}

// TestWrittenAtAdvancesOnOverwrite: every Put re-stamps WrittenAt, so overwriting
// a key advances it (guards a regression that carried the prior timestamp).
func TestWrittenAtAdvancesOnOverwrite(t *testing.T) {
	bb := NewBlackboard(nil)
	bb.Put("k", 1, "i", "l")
	e1, _ := bb.Get("k")
	time.Sleep(2 * time.Millisecond)
	bb.Put("k", 2, "i", "l")
	e2, _ := bb.Get("k")
	if !e2.WrittenAt.After(e1.WrittenAt) {
		t.Fatalf("WrittenAt did not advance on overwrite: %v -> %v", e1.WrittenAt, e2.WrittenAt)
	}
}

// TestEmptyProvenanceStored: empty writerID/label are stored verbatim; the entry
// is present with empty provenance (distinct from a missing key), non-zero time.
func TestEmptyProvenanceStored(t *testing.T) {
	bb := NewBlackboard(nil)
	bb.Put("k", 1, "", "")
	e, ok := bb.Get("k")
	if !ok || e.Value != 1 {
		t.Fatalf("empty-provenance key not present: %+v ok=%v", e, ok)
	}
	if e.WriterID != "" || e.WriterLabel != "" {
		t.Fatalf("provenance not stored verbatim: id=%q label=%q", e.WriterID, e.WriterLabel)
	}
	if e.WrittenAt.IsZero() {
		t.Fatal("WrittenAt should be stamped even with empty provenance")
	}
}

// TestWaitNilValuePresentKeySatisfies: a key present with a nil Value satisfies
// Wait (returns (nil, nil) promptly via the fast path) — presence, not non-nil
// value, is the wait condition. Also covers the late-Put(nil) wakeup.
func TestWaitNilValuePresentKeySatisfies(t *testing.T) {
	// Fast path: key already present with nil value.
	bb := NewBlackboard(nil)
	bb.Put("k", nil, "i", "l")
	start := time.Now()
	v, err := bb.Wait(context.Background(), "k", time.Second, nil)
	if err != nil || v != nil {
		t.Fatalf("fast-path Wait on nil-value key = (%v, %v), want (nil, nil)", v, err)
	}
	if time.Since(start) > 200*time.Millisecond {
		t.Fatal("fast path blocked instead of returning immediately")
	}

	// Late-Put(nil): a waiter on an unset key is woken by a Put of a nil value.
	bb2 := NewBlackboard(nil)
	done := make(chan error, 1)
	go func() {
		val, e := bb2.Wait(context.Background(), "k", 2*time.Second, nil)
		if val != nil {
			done <- context.DeadlineExceeded // sentinel: unexpected value
			return
		}
		done <- e
	}()
	time.Sleep(20 * time.Millisecond)
	bb2.Put("k", nil, "i", "l")
	select {
	case e := <-done:
		if e != nil {
			t.Fatalf("late-Put(nil) wakeup failed: %v", e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Wait not woken by a late Put of a nil value")
	}
}

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
