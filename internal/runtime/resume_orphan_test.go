package runtime

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/event/fsstore"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/tools"
)

// recordingStore wraps a real durable store and remembers every Append the runtime
// makes through it, so a test can assert on the EXACT synthetic records the orphan
// repair wrote — not merely on what a later fold happens to produce. Durability is
// real (a temp-dir fsstore beneath), so the re-replay path is exercised as it is in
// production.
type recordingStore struct {
	event.DurableStore

	mu       sync.Mutex
	appended []event.Event
}

func (s *recordingStore) Append(ctx context.Context, key event.StreamKey, e event.Event) error {
	s.mu.Lock()
	s.appended = append(s.appended, e)
	s.mu.Unlock()
	return s.DurableStore.Append(ctx, key, e)
}

// recorded returns a snapshot of the appends captured so far. A live loop's run
// goroutine appends concurrently, so this must be taken under the same lock.
func (s *recordingStore) recorded() []event.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]event.Event(nil), s.appended...)
}

// registry exposes the same durable backend as a loop registry, so one temp-dir store
// serves both seams exactly as production wires them.
func (s *recordingStore) registry(t *testing.T) event.LoopRegistry {
	t.Helper()
	reg, ok := s.DurableStore.(event.LoopRegistry)
	if !ok {
		t.Fatalf("%T is not a loop registry", s.DurableStore)
	}
	return reg
}

// reset drops the recorded appends, so a test can isolate what the code under test
// wrote from what a fixture wrote.
func (s *recordingStore) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.appended = nil
}

// newRecordingStore returns a recording store over a real fsstore in a temp dir.
func newRecordingStore(t *testing.T) *recordingStore {
	t.Helper()
	return &recordingStore{DurableStore: fsstore.NewDurableFSStore(t.TempDir())}
}

// appendEvents writes a hand-crafted event slice onto the stream, in order, letting
// the store allocate Seq exactly as the producer would. Callers pass the bare
// durable store so a recording wrapper's captures stay exactly what the code under
// test wrote.
func appendEvents(t *testing.T, store event.DurableStore, key event.StreamKey, evs []event.Event) {
	t.Helper()
	for _, e := range evs {
		if err := store.Append(context.Background(), key, e); err != nil {
			t.Fatalf("seed append %s: %v", e.Kind, err)
		}
	}
}

// payloadOf unmarshals the payload of the first recorded event of kind into v and
// reports whether such an event was appended at all.
func payloadOf[T any](t *testing.T, appended []event.Event, kind event.Kind) (T, bool) {
	t.Helper()
	var out T
	for _, e := range appended {
		if e.Kind != kind {
			continue
		}
		if err := json.Unmarshal(e.Payload, &out); err != nil {
			t.Fatalf("decode %s payload: %v", kind, err)
		}
		return out, true
	}
	return out, false
}

// kindsRecorded lists the kinds a recording store captured, for failure messages.
func kindsRecorded(appended []event.Event) []event.Kind {
	out := make([]event.Kind, len(appended))
	for i, e := range appended {
		out[i] = e.Kind
	}
	return out
}

// orphanStream is the stream a loop leaves behind when its owner dies with a child
// mid-flight: the root asked for a spawn_agent call, the child started, and then
// nothing — no spawn.done for the child, no tool.result for the root's call.
func orphanStream(rootID, childID string) []event.Event {
	return []event.Event{
		{NodeID: rootID, Kind: event.KindUserInput, Payload: mustPayload(event.UserInputPayload{Content: "delegate this"})},
		{NodeID: rootID, Kind: event.KindModelCallEnd, Payload: mustPayload(event.ModelCallEndPayload{
			Content: "spawning a helper",
			ToolCalls: []event.ToolCallRef{
				{ID: "call-1", Name: "spawn_agent"},
			},
		})},
		{NodeID: rootID, Kind: event.KindToolCall, Payload: mustPayload(event.ToolCallPayload{
			ID: "call-1", Name: "spawn_agent", Args: []byte(`{"task":"read the repo"}`),
		})},
		// The child starts its own turn on the same stream key (its events carry its
		// own NodeID, so the root transcript fold skips them).
		{NodeID: childID, ParentID: rootID, Depth: 1, Kind: event.KindSpawnStart, Payload: mustPayload(event.SpawnStartPayload{
			ChildNodeID: childID, Label: "helper", Task: "read the repo",
		})},
		{NodeID: childID, ParentID: rootID, Depth: 1, Kind: event.KindUserInput, Payload: mustPayload(event.UserInputPayload{Content: "read the repo"})},
		// ...and then the owner dies.
	}
}

// mustPayload marshals a payload or panics (test-only fixture helper).
func mustPayload(v any) []byte {
	b, err := event.MarshalPayload(v)
	if err != nil {
		panic(err)
	}
	return b
}

// TestRepairOrphansMakesTheTranscriptAPIValid is the core D3 test: a stream holding
// an orphaned child and an unanswered root tool call is repaired durably, and the
// re-folded state is a conversation a provider will accept — the assistant's tool
// call is answered, and the child's spawn is terminal.
func TestRepairOrphansMakesTheTranscriptAPIValid(t *testing.T) {
	const rootID, childID = "loop-root", "child-1"
	key := event.StreamKey{Tenant: event.DefaultTenant, Loop: event.LoopID(rootID)}

	store := newRecordingStore(t)
	appendEvents(t, store.DurableStore, key, orphanStream(rootID, childID))

	// Fold the un-repaired stream first: the defect this change fixes, asserted so the
	// test cannot pass vacuously (an already-clean stream would make repair a no-op).
	events, err := store.Replay(context.Background(), key, 0)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	before, err := reconstructLoopState(events, rootID)
	if err != nil {
		t.Fatalf("reconstruct (pre-repair): %v", err)
	}
	if len(before.tree.nodes) != 1 || !before.tree.nodes[0].orphaned {
		t.Fatalf("precondition: want exactly one orphaned child, got %+v", before.tree.nodes)
	}
	if unanswered := unansweredToolCalls(before.messages); len(unanswered) != 1 || unanswered[0] != "call-1" {
		t.Fatalf("precondition: want one unanswered root call call-1, got %v", unanswered)
	}

	state, err := repairOrphans(context.Background(), store, key, rootID, before)
	if err != nil {
		t.Fatalf("repairOrphans: %v", err)
	}

	// --- A synthetic spawn.done marks the orphan terminal ---
	done, ok := payloadOf[event.SpawnDonePayload](t, store.recorded(), event.KindSpawnDone)
	if !ok {
		t.Fatalf("no spawn.done appended, got %v", kindsRecorded(store.recorded()))
	}
	if done.ChildNodeID != childID {
		t.Fatalf("spawn.done child = %q, want %q", done.ChildNodeID, childID)
	}
	if !done.Orphaned {
		t.Fatal("spawn.done must be marked Orphaned:true — a failed child, never a silent retry")
	}
	if done.Err != orphanedSpawnErr {
		t.Fatalf("spawn.done err = %q, want %q", done.Err, orphanedSpawnErr)
	}
	if done.ParentID != rootID || done.Depth != 1 {
		t.Fatalf("spawn.done identity = (parent %q, depth %d), want (%q, 1)", done.ParentID, done.Depth, rootID)
	}

	// --- A synthetic tool.result answers the root's dangling call ---
	res, ok := payloadOf[event.ToolResultPayload](t, store.recorded(), event.KindToolResult)
	if !ok {
		t.Fatalf("no tool.result appended, got %v", kindsRecorded(store.recorded()))
	}
	if res.ID != "call-1" || res.Name != "spawn_agent" {
		t.Fatalf("tool.result = (%q, %q), want (call-1, spawn_agent)", res.ID, res.Name)
	}
	if !res.Synthetic || !res.IsError {
		t.Fatalf("tool.result must be Synthetic:true IsError:true, got %+v", res)
	}

	// --- The returned state is the RE-FOLDED stream, synthetic records included ---
	if unanswered := unansweredToolCalls(state.messages); len(unanswered) != 0 {
		t.Fatalf("repaired transcript still has unanswered tool calls: %v", unanswered)
	}
	last := state.messages[len(state.messages)-1]
	if last.Role != "tool" || last.ToolCallID != "call-1" {
		t.Fatalf("repaired transcript must end in the synthetic tool result, ended in %+v", last)
	}
	if last.Content != syntheticToolResultText {
		t.Fatalf("tool message content = %q, want %q", last.Content, syntheticToolResultText)
	}
	if len(state.tree.nodes) != 1 || state.tree.nodes[0].orphaned || !state.tree.nodes[0].terminal {
		t.Fatalf("repaired tree must have a terminal, non-orphaned child, got %+v", state.tree.nodes)
	}

	// No child turns leaked into the root transcript on the re-fold either.
	for _, m := range state.messages {
		if m.Role == "user" && m.Content == "read the repo" {
			t.Fatal("child user turn leaked into the root transcript")
		}
	}
}

// TestRepairOrphansCleanStreamAppendsNothing proves the repair is a no-op on a
// healthy stream: every spawn already finished and every call already answered, so
// a resume appends no synthetic record and pays no re-replay.
func TestRepairOrphansCleanStreamAppendsNothing(t *testing.T) {
	const rootID, childID = "loop-root", "child-1"
	key := event.StreamKey{Tenant: event.DefaultTenant, Loop: event.LoopID(rootID)}

	clean := append(orphanStream(rootID, childID),
		event.Event{NodeID: childID, ParentID: rootID, Depth: 1, Kind: event.KindSpawnDone, Payload: mustPayload(event.SpawnDonePayload{
			ChildNodeID: childID, ParentID: rootID, Label: "helper", Depth: 1, Result: "done",
		})},
		event.Event{NodeID: rootID, Kind: event.KindToolResult, Payload: mustPayload(event.ToolResultPayload{
			ID: "call-1", Name: "spawn_agent", Result: "done",
		})},
	)

	store := newRecordingStore(t)
	appendEvents(t, store.DurableStore, key, clean)

	events, err := store.Replay(context.Background(), key, 0)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	state, err := reconstructLoopState(events, rootID)
	if err != nil {
		t.Fatalf("reconstruct: %v", err)
	}

	repaired, err := repairOrphans(context.Background(), store, key, rootID, state)
	if err != nil {
		t.Fatalf("repairOrphans: %v", err)
	}
	if len(store.recorded()) != 0 {
		t.Fatalf("clean stream must append nothing, appended %v", kindsRecorded(store.recorded()))
	}
	if len(repaired.messages) != len(state.messages) {
		t.Fatalf("clean stream must not change the transcript: %d → %d messages", len(state.messages), len(repaired.messages))
	}
}

// TestRepairOrphansIsIdempotent proves a resume that died after appending its
// synthetic records does not append them again: the repaired stream folds to a
// state with nothing left to repair.
func TestRepairOrphansIsIdempotent(t *testing.T) {
	const rootID, childID = "loop-root", "child-1"
	key := event.StreamKey{Tenant: event.DefaultTenant, Loop: event.LoopID(rootID)}

	store := newRecordingStore(t)
	appendEvents(t, store.DurableStore, key, orphanStream(rootID, childID))

	events, err := store.Replay(context.Background(), key, 0)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	first, err := reconstructLoopState(events, rootID)
	if err != nil {
		t.Fatalf("reconstruct: %v", err)
	}
	repaired, err := repairOrphans(context.Background(), store, key, rootID, first)
	if err != nil {
		t.Fatalf("first repairOrphans: %v", err)
	}
	if len(store.recorded()) != 2 {
		t.Fatalf("first repair appended %v, want one spawn.done + one tool.result", kindsRecorded(store.recorded()))
	}

	// A second resume folds the already-repaired stream: nothing left to do.
	store.reset()
	if _, err := repairOrphans(context.Background(), store, key, rootID, repaired); err != nil {
		t.Fatalf("second repairOrphans: %v", err)
	}
	if len(store.recorded()) != 0 {
		t.Fatalf("second repair re-appended %v — the repair is not idempotent", kindsRecorded(store.recorded()))
	}
}

// unansweredToolCalls lists the tool-call IDs an assistant message asked for that no
// tool message answers — exactly the shape a provider rejects.
func unansweredToolCalls(msgs []model.Message) []string {
	answered := map[string]bool{}
	for _, m := range msgs {
		if m.Role == "tool" && m.ToolCallID != "" {
			answered[m.ToolCallID] = true
		}
	}
	var out []string
	for _, m := range msgs {
		if m.Role != "assistant" {
			continue
		}
		for _, call := range m.ToolCalls {
			if !answered[call.ID] {
				out = append(out, call.ID)
			}
		}
	}
	return out
}

// TestResumeRepairsOrphansOnTheStream drives the production path: a loop's stream is
// left in the shape a dead owner leaves behind (an orphaned child, an unanswered root
// tool call), and a FRESH runtime resumes it. The resume must repair the stream
// durably BEFORE it seeds anything, and the seed transcript the resumed loop carries
// must be a conversation a provider accepts.
// leafCompleter answers every turn with a plain assistant message, so the resumed
// loop parks after one exchange — nothing here depends on scripted tool traffic.
type leafCompleter struct{}

func (leafCompleter) Complete(context.Context, model.CompletionReq) (model.CompletionResp, error) {
	return model.CompletionResp{Content: "picking up where we left off"}, nil
}

func TestResumeRepairsOrphansOnTheStream(t *testing.T) {
	const rootID, childID = "loop-root", "child-1"
	key := event.StreamKey{Tenant: event.DefaultTenant, Loop: event.LoopID(rootID)}

	store := newRecordingStore(t)
	// Do what the dead owner did: open the stream and record the loop, then die with a
	// child mid-flight (no spawn.done, no tool.result). Registering live=false makes the
	// record look like a finished loop, which is exactly the case a resume rehydrates.
	appendEvents(t, store.DurableStore, key, orphanStream(rootID, childID))
	if err := store.registry(t).Register(context.Background(), event.LoopRecord{Key: key, OwnerNodeID: rootID}); err != nil {
		t.Fatalf("register: %v", err)
	}
	store.reset()

	rt := New(Deps{
		DurableStore:  store,
		Registry:      store.registry(t),
		MaxConcurrent: 1,
		IdleTTL:       time.Hour, // keep the resumed loop alive so its seed can be inspected
		BuildAgent: func(s event.EventStore, tree *agent.AgentTree, modelID string, r *tools.Registry) (*agent.Agent, agent.ChildBuilder, string, error) {
			return agent.New(leafCompleter{}, execAll{r}, nopRenderer{}, modelID, "", 1, 0), nil, modelID, nil
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h, err := rt.Resume(ctx, event.DefaultTenant, rootID)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if h.ID() != rootID {
		t.Fatalf("resumed id = %q, want %q", h.ID(), rootID)
	}

	// The repair landed on the durable stream, not just in memory: a fresh replay
	// (which is what any later replica folds) carries both synthetic records.
	events, err := store.DurableStore.Replay(ctx, key, 0)
	if err != nil {
		t.Fatalf("replay after resume: %v", err)
	}
	state, err := reconstructLoopState(events, rootID)
	if err != nil {
		t.Fatalf("reconstruct after resume: %v", err)
	}
	if unanswered := unansweredToolCalls(state.messages); len(unanswered) != 0 {
		t.Fatalf("stream after resume still has unanswered tool calls: %v", unanswered)
	}
	if len(state.tree.nodes) != 1 || !state.tree.nodes[0].terminal || state.tree.nodes[0].orphaned {
		t.Fatalf("stream after resume must carry a terminal (non-orphaned) child, got %+v", state.tree.nodes)
	}

	// And the records really are the synthetic ones, with the flags a consumer reads.
	done, ok := payloadOf[event.SpawnDonePayload](t, store.recorded(), event.KindSpawnDone)
	if !ok {
		t.Fatalf("resume appended no spawn.done, recorded %v", kindsRecorded(store.recorded()))
	}
	if !done.Orphaned || done.ChildNodeID != childID {
		t.Fatalf("resume spawn.done = %+v, want Orphaned:true for %q", done, childID)
	}
	res, ok := payloadOf[event.ToolResultPayload](t, store.recorded(), event.KindToolResult)
	if !ok {
		t.Fatalf("resume appended no tool.result, recorded %v", kindsRecorded(store.recorded()))
	}
	if !res.Synthetic || !res.IsError || res.ID != "call-1" {
		t.Fatalf("resume tool.result = %+v, want Synthetic:true IsError:true for call-1", res)
	}

	// Nothing was retried or spawned: the repair answers the dead work, it does not
	// re-do it.
	for _, e := range store.recorded() {
		if e.Kind == event.KindSpawnStart {
			t.Fatalf("resume re-spawned: %v", kindsRecorded(store.recorded()))
		}
	}
}
