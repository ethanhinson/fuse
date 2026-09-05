package tools

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/tools/sandbox"
)

// keyedRecorder stands in for the loop's OWN event sink: the runtime hands
// BuildAgent an EventStore already bound to one StreamKey (durableSink), so a
// component that merely Appends lands on exactly that loop's stream. Recording
// the key alongside every event is how the test proves that, rather than
// asserting on a key the emitter never sees.
type keyedRecorder struct {
	key event.StreamKey

	mu     sync.Mutex
	events []event.Event
	keys   []event.StreamKey
}

func (r *keyedRecorder) Append(e event.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
	r.keys = append(r.keys, r.key)
	return nil
}

func (r *keyedRecorder) Subscribe() (<-chan event.Event, func()) {
	ch := make(chan event.Event)
	close(ch)
	return ch, func() {}
}

func (r *keyedRecorder) Replay(event.Seq) ([]event.Event, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]event.Event, len(r.events))
	copy(out, r.events)
	return out, nil
}

func (r *keyedRecorder) snapshot() ([]event.Event, []event.StreamKey) {
	r.mu.Lock()
	defer r.mu.Unlock()
	evs := make([]event.Event, len(r.events))
	copy(evs, r.events)
	ks := make([]event.StreamKey, len(r.keys))
	copy(ks, r.keys)
	return evs, ks
}

// TestBashPoolEmitsSandboxLifecycleEvents is the wiring crux for change 0063
// T8–T11: the four sandbox event kinds, the fuse_sandbox_* metric families, the
// dashboard, and the alert rules are all fed by ONE thing — the pool's hooks
// actually reaching an event stream. Before this wiring existed the projection
// could never observe data, because nothing ever emitted.
//
// It drives a REAL bash execution on the host substrate (the operator off-switch
// config, the only shape under which a unit test may run a command here) so the
// acquire and the hand-back are the pool's own, not a hand-called hook.
func TestBashPoolEmitsSandboxLifecycleEvents(t *testing.T) {
	t.Setenv("FUSE_TEST_SECRET", "s3cret")

	svc, err := sandbox.NewService(hostAuthorizedConfig())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	store := &keyedRecorder{key: event.StreamKey{Tenant: "acme", Loop: "loop-7"}}

	b := NewBash(svc, sandbox.WithPoolHooks(SandboxEventHooks(store, "node-root")))
	if res := b.Execute(context.Background(), `{"command":"echo hi"}`); res.IsError {
		t.Fatalf("bash: %s", res.Output)
	}
	if err := ReleaseSandboxes(context.Background(), registryWith(b)); err != nil {
		t.Fatalf("teardown: %v", err)
	}

	evs, keys := store.snapshot()
	if len(evs) == 0 {
		t.Fatal("no sandbox events reached the loop's event stream — the pool's hooks are not wired to an emitter")
	}

	var acquire, release *event.Event
	for i := range evs {
		switch evs[i].Kind {
		case event.KindSandboxAcquire:
			if acquire == nil {
				acquire = &evs[i]
			}
		case event.KindSandboxRelease:
			if release == nil {
				release = &evs[i]
			}
		}
	}
	if acquire == nil {
		t.Fatalf("no %s event; got kinds %v", event.KindSandboxAcquire, kindsOf(evs))
	}
	if release == nil {
		t.Fatalf("no %s event; got kinds %v", event.KindSandboxRelease, kindsOf(evs))
	}

	// The envelope: every event lands under the loop's OWN StreamKey and carries
	// the loop's node id. Correlation lives here and nowhere else.
	for i, k := range keys {
		if k != (event.StreamKey{Tenant: "acme", Loop: "loop-7"}) {
			t.Fatalf("event %d landed under StreamKey %+v, want the loop's own key", i, k)
		}
		if evs[i].NodeID != "node-root" {
			t.Fatalf("event %d NodeID = %q, want the loop's node id", i, evs[i].NodeID)
		}
	}

	var ap event.SandboxAcquirePayload
	if err := json.Unmarshal(acquire.Payload, &ap); err != nil {
		t.Fatalf("acquire payload: %v", err)
	}
	if ap.Handler != "host" {
		t.Fatalf("acquire handler = %q, want %q", ap.Handler, "host")
	}
	if ap.Reused {
		t.Fatal("first acquire must not report reuse")
	}

	var rp event.SandboxReleasePayload
	if err := json.Unmarshal(release.Payload, &rp); err != nil {
		t.Fatalf("release payload: %v", err)
	}
	if rp.Cause != event.SandboxCauseReleased {
		t.Fatalf("release cause = %q, want %q", rp.Cause, event.SandboxCauseReleased)
	}
	if rp.Handler != "host" {
		t.Fatalf("release handler = %q, want %q", rp.Handler, "host")
	}

	// Payload-free discipline: bounded identity and closed enums only. Never the
	// command, an environment value, output, or a raw error string — events.jsonl
	// is durable and replayed.
	for i, e := range evs {
		raw := string(e.Payload)
		for _, banned := range []string{"echo hi", "s3cret", "FUSE_TEST_SECRET", "hi\n"} {
			if strings.Contains(raw, banned) {
				t.Fatalf("event %d (%s) payload leaked %q: %s", i, e.Kind, banned, raw)
			}
		}
		// The (tenant, loop) envelope must not be duplicated into a payload.
		for _, dup := range []string{"acme", "loop-7", "node-root"} {
			if strings.Contains(raw, dup) {
				t.Fatalf("event %d (%s) duplicated envelope identity %q into its payload: %s", i, e.Kind, dup, raw)
			}
		}
	}
}

// TestSandboxEventHooksNilStoreIsInert: a binding with no per-loop event store
// (one-shot, shell, research-probe, mcp-server) passes nothing, and the pool
// must not panic on a half-built hook set.
func TestSandboxEventHooksNilStoreIsInert(t *testing.T) {
	h := SandboxEventHooks(nil, "node")
	if h.Acquired != nil || h.Released != nil || h.Reaped != nil {
		t.Fatal("hooks over a nil store must be entirely inert")
	}
}

// The gate hooks translate a notable admission into a KindSandboxAdmission event
// with the bounded fields, and a nil store yields inert hooks (change 0077).
func TestSandboxGateHooksTranslateAdmissions(t *testing.T) {
	rec := &keyedRecorder{key: event.StreamKey{Tenant: "acme", Loop: "L1"}}
	h := SandboxGateHooks(rec, "root-node")

	h.Queued(sandbox.AdmissionInfo{Tenant: "acme", Handler: "container", Scope: sandbox.ScopeTenant, Waited: 4200 * time.Millisecond})
	h.Refused(sandbox.AdmissionInfo{Tenant: "acme", Handler: "container", Scope: sandbox.ScopeGlobal})

	evs, _ := rec.snapshot()
	if len(evs) != 2 {
		t.Fatalf("emitted %d events, want 2", len(evs))
	}
	for _, e := range evs {
		if e.Kind != event.KindSandboxAdmission {
			t.Errorf("kind = %q, want sandbox.admission", e.Kind)
		}
		if e.NodeID != "root-node" {
			t.Errorf("node = %q, want root-node", e.NodeID)
		}
	}

	var q event.SandboxAdmissionPayload
	if err := json.Unmarshal(evs[0].Payload, &q); err != nil {
		t.Fatalf("queued payload: %v", err)
	}
	if q.Outcome != "queued" || q.Scope != "tenant" || q.Handler != "container" || q.WaitMS != 4200 {
		t.Errorf("queued payload = %+v", q)
	}

	var r event.SandboxAdmissionPayload
	if err := json.Unmarshal(evs[1].Payload, &r); err != nil {
		t.Fatalf("refused payload: %v", err)
	}
	if r.Outcome != "refused" || r.Scope != "global" {
		t.Errorf("refused payload = %+v", r)
	}
	// A refusal is immediate: no wait recorded.
	if r.WaitMS != 0 {
		t.Errorf("refused wait_ms = %d, want 0 (immediate)", r.WaitMS)
	}
}

func TestSandboxGateHooksNilStoreIsInert(t *testing.T) {
	h := SandboxGateHooks(nil, "node")
	if h.Queued != nil || h.Refused != nil {
		t.Fatal("gate hooks over a nil store must be entirely inert")
	}
}

func kindsOf(evs []event.Event) []event.Kind {
	out := make([]event.Kind, 0, len(evs))
	for _, e := range evs {
		out = append(out, e.Kind)
	}
	return out
}

// TestSandboxHealthHooksTranslate pins the sandbox→event reason mapping and the
// two payload-discipline rules the health emitter carries: ContainerID is always
// empty (this substrate has no container that outlives an Exec, so there is no
// honest id to report), and an unrecognised reason is DROPPED rather than
// emitted with an empty label.
func TestSandboxHealthHooksTranslate(t *testing.T) {
	store := &keyedRecorder{key: event.StreamKey{Tenant: "t", Loop: "l"}}
	hooks := SandboxHealthHooks(store, "node-1")

	for _, r := range []sandbox.HealthReason{
		sandbox.HealthOOM,
		sandbox.HealthRuntimeExit,
		sandbox.HealthPullFailed,
		sandbox.HealthAcquireFailed,
	} {
		hooks.Unhealthy(sandbox.HealthInfo{Handler: "container", Reason: r})
	}
	// An unrecognised reason must add nothing.
	hooks.Unhealthy(sandbox.HealthInfo{Handler: "container", Reason: sandbox.HealthReason("made_up")})

	evs, _ := store.snapshot()
	if len(evs) != 4 {
		t.Fatalf("got %d events, want 4 (the unrecognised reason must be dropped)", len(evs))
	}
	want := []string{"oom", "runtime_exit", "pull_failed", "acquire_failed"}
	for i, e := range evs {
		if e.Kind != event.KindSandboxHealth {
			t.Errorf("event %d kind = %q, want %q", i, e.Kind, event.KindSandboxHealth)
		}
		var p event.SandboxHealthPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("event %d payload: %v", i, err)
		}
		if p.Reason != want[i] {
			t.Errorf("event %d reason = %q, want %q", i, p.Reason, want[i])
		}
		if p.ContainerID != "" {
			t.Errorf("event %d container_id = %q, want empty — this substrate has no durable container to name", i, p.ContainerID)
		}
		if p.Healthy {
			t.Errorf("event %d healthy = true, want false", i)
		}
	}
}

// TestSandboxHealthHooksNilStoreIsInert pins the honest shape for a binding with
// no per-loop event store: entirely inert hooks, not a live emitter into nothing.
func TestSandboxHealthHooksNilStoreIsInert(t *testing.T) {
	if h := SandboxHealthHooks(nil, "node-1"); h.Unhealthy != nil {
		t.Fatal("SandboxHealthHooks(nil) returned a live hook; want inert")
	}
}
