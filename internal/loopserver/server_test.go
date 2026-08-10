package loopserver

import (
	"context"
	"encoding/json"
	"io"
	"testing"

	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/runtime"
)

// fakeRuntime is a test double satisfying runtime.Runtime. It records the last
// StartLoop/Send arguments and returns scripted Attach/Observe results. It also
// records the tenant threaded onto Observe/Attach (change 0047) so a test can assert
// the tenant reaches the runtime seam.
type fakeRuntime struct {
	startCfg runtime.LoopConfig
	startID  string
	startErr error

	sendLoopID string
	sendInput  string
	sendErr    error

	attachHist []event.Event
	attachErr  error

	observeCh   chan event.Event
	observeErr  error
	observeStop bool

	// observeTenant / attachTenant record the tenant the last Observe/Attach carried.
	observeTenant event.TenantID
	attachTenant  event.TenantID
}

func (f *fakeRuntime) StartLoop(ctx context.Context, cfg runtime.LoopConfig) (runtime.LoopHandle, error) {
	f.startCfg = cfg
	if f.startErr != nil {
		return nil, f.startErr
	}
	return runtimeHandle{id: f.startID}, nil
}

func (f *fakeRuntime) Send(ctx context.Context, loopID, input string) error {
	f.sendLoopID = loopID
	f.sendInput = input
	return f.sendErr
}

func (f *fakeRuntime) Spawn(ctx context.Context, loopID string, opts runtime.SpawnOpts) (runtime.SpawnHandle, error) {
	return nil, nil
}

func (f *fakeRuntime) Observe(ctx context.Context, tenant event.TenantID, loopID string) (<-chan event.Event, func(), error) {
	f.observeTenant = tenant
	if f.observeErr != nil {
		return nil, nil, f.observeErr
	}
	return f.observeCh, func() { f.observeStop = true }, nil
}

func (f *fakeRuntime) Attach(ctx context.Context, tenant event.TenantID, loopID string, from event.Seq) ([]event.Event, error) {
	f.attachTenant = tenant
	return f.attachHist, f.attachErr
}

// runtimeHandle is the concrete handle returned by fakeRuntime; it must match
// runtime.LoopHandle exactly (Wait returns []model.Message).
type runtimeHandle struct{ id string }

func (h runtimeHandle) ID() string                     { return h.id }
func (h runtimeHandle) Wait() ([]model.Message, error) { return nil, nil }

func TestDispatchLoopStartReturnsLoopID(t *testing.T) {
	fr := &fakeRuntime{startID: "loop-abc"}
	s := NewServer(nil, nil, fr)

	r := req{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "loop.start", Params: json.RawMessage(`{"task":"hi","model":"cloud/x"}`)}
	got := s.dispatch(context.Background(), r)

	if got.Error != nil {
		t.Fatalf("unexpected error: %+v", got.Error)
	}
	var res startResult
	if err := json.Unmarshal(got.Result, &res); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if res.LoopID != "loop-abc" {
		t.Fatalf("loop_id = %q, want loop-abc", res.LoopID)
	}
	if fr.startCfg.Task != "hi" || fr.startCfg.ModelID != "cloud/x" {
		t.Fatalf("StartLoop cfg = %+v, want Task=hi ModelID=cloud/x", fr.startCfg)
	}
}

func TestDispatchLoopSendCallsRuntime(t *testing.T) {
	fr := &fakeRuntime{}
	s := NewServer(nil, nil, fr)

	r := req{JSONRPC: "2.0", ID: json.RawMessage(`2`), Method: "loop.send", Params: json.RawMessage(`{"loop_id":"loop-abc","input":"more"}`)}
	got := s.dispatch(context.Background(), r)

	if got.Error != nil {
		t.Fatalf("unexpected error: %+v", got.Error)
	}
	if fr.sendLoopID != "loop-abc" || fr.sendInput != "more" {
		t.Fatalf("Send args = (%q,%q), want (loop-abc,more)", fr.sendLoopID, fr.sendInput)
	}
}

// TestDispatchLoopSendFinishedLoopIsError proves loop.send to a finished (or unknown)
// loop maps the runtime sentinel to a JSON-RPC error (codeInvalidParams), NOT an OK
// response — a client sending to a stale loop_id must be told, not silently accepted.
func TestDispatchLoopSendFinishedLoopIsError(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"finished", runtime.ErrLoopFinished},
		{"unknown", runtime.ErrLoopNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fr := &fakeRuntime{sendErr: tc.err}
			s := NewServer(nil, nil, fr)
			r := req{JSONRPC: "2.0", ID: json.RawMessage(`5`), Method: "loop.send", Params: json.RawMessage(`{"loop_id":"loop-abc","input":"x"}`)}
			got := s.dispatch(context.Background(), r)
			if got.Error == nil {
				t.Fatalf("got OK result %s, want a JSON-RPC error", got.Result)
			}
			if got.Error.Code != codeInvalidParams {
				t.Fatalf("error code = %d, want codeInvalidParams (%d)", got.Error.Code, codeInvalidParams)
			}
		})
	}
}

func TestUnknownMethod(t *testing.T) {
	s := NewServer(nil, nil, &fakeRuntime{})
	r := req{JSONRPC: "2.0", ID: json.RawMessage(`3`), Method: "loop.nope"}
	got := s.dispatch(context.Background(), r)
	if got.Error == nil || got.Error.Code != codeMethodNotFound {
		t.Fatalf("got %+v, want method-not-found", got.Error)
	}
}

func TestLoopStartBadParams(t *testing.T) {
	s := NewServer(nil, nil, &fakeRuntime{})
	r := req{JSONRPC: "2.0", ID: json.RawMessage(`4`), Method: "loop.start", Params: json.RawMessage(`{`)}
	got := s.dispatch(context.Background(), r)
	if got.Error == nil || got.Error.Code != codeInvalidParams {
		t.Fatalf("got %+v, want invalid-params", got.Error)
	}
}

// rawFrame is a loose decode target that captures either a response (Result/Error)
// or a loop.event notification (Method/Params). This is the RAW JSON-RPC test
// client — NOT fuse's mcp client, whose read pump drops id-less notifications
// (learning mcp-read-pumps-drop-inbound-notifications).
type rawFrame struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error"`
}

func TestObserveReplaysThenTails(t *testing.T) {
	live := make(chan event.Event, 1)
	fr := &fakeRuntime{
		attachHist: []event.Event{
			{Seq: 1, Turn: 0, Kind: event.KindTurnStart},
			{Seq: 2, Turn: 0, Kind: event.KindModelCallStart},
		},
		observeCh: live,
	}

	// Server writes to sw; the test reads server output off sr with a raw decoder.
	// The server reads its requests from cr; the test writes requests to cw.
	sr, sw := io.Pipe()
	cr, cw := io.Pipe()
	s := NewServer(cr, sw, fr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = s.Serve(ctx) }()

	// Send the observe request.
	go func() {
		enc := json.NewEncoder(cw)
		_ = enc.Encode(req{JSONRPC: "2.0", ID: json.RawMessage(`10`), Method: "loop.observe", Params: json.RawMessage(`{"loop_id":"loop-abc"}`)})
	}()

	dec := json.NewDecoder(sr)

	// 1) First frame is the observe response with replayed:2,last_seq:2.
	var f0 rawFrame
	if err := dec.Decode(&f0); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(f0.ID) == 0 || f0.Error != nil {
		t.Fatalf("frame 0 not a success response: %+v", f0)
	}
	var res observeResult
	if err := json.Unmarshal(f0.Result, &res); err != nil {
		t.Fatalf("unmarshal observeResult: %v", err)
	}
	if res.Replayed != 2 || res.LastSeq != 2 {
		t.Fatalf("observeResult = %+v, want replayed:2 last_seq:2", res)
	}

	// 2) Two id-less loop.event notifications for the replayed Seq 1,2.
	for _, wantSeq := range []event.Seq{1, 2} {
		np := readNote(t, dec)
		if np.Event.Seq != wantSeq {
			t.Fatalf("replay note seq = %d, want %d", np.Event.Seq, wantSeq)
		}
		if np.Gap {
			t.Fatalf("replay note seq %d unexpectedly flagged gap", wantSeq)
		}
	}

	// 3) Feed a live event (Seq 3) and read its loop.event notification.
	live <- event.Event{Seq: 3, Turn: 0, Kind: event.KindTurnEnd}
	np := readNote(t, dec)
	if np.Event.Seq != 3 {
		t.Fatalf("live note seq = %d, want 3", np.Event.Seq)
	}
	if np.LoopID != "loop-abc" {
		t.Fatalf("live note loop_id = %q, want loop-abc", np.LoopID)
	}
	if np.Gap {
		t.Fatalf("live note seq 3 unexpectedly flagged gap (contiguous)")
	}
}

// TestObserveThreadsTenant proves a tenant-scoped loop.observe threads its tenant
// through to rt.Observe AND rt.Attach (change 0047 tenant isolation). The wire dedup
// path is unchanged; only the tenant now reaches the runtime seam.
func TestObserveThreadsTenant(t *testing.T) {
	live := make(chan event.Event, 1)
	fr := &fakeRuntime{
		attachHist: []event.Event{{Seq: 1, Kind: event.KindTurnStart}},
		observeCh:  live,
	}

	sr, sw := io.Pipe()
	cr, cw := io.Pipe()
	s := NewServer(cr, sw, fr)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = s.Serve(ctx) }()
	go func() {
		_ = json.NewEncoder(cw).Encode(req{JSONRPC: "2.0", ID: json.RawMessage(`30`),
			Method: "loop.observe", Params: json.RawMessage(`{"loop_id":"loop-abc","tenant":"acme"}`)})
	}()

	dec := json.NewDecoder(sr)
	var f0 rawFrame
	if err := dec.Decode(&f0); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	// Read the one replayed note so we know Observe+Attach have both been called.
	_ = readNote(t, dec)

	if fr.observeTenant != event.TenantID("acme") {
		t.Fatalf("Observe tenant = %q, want acme", fr.observeTenant)
	}
	if fr.attachTenant != event.TenantID("acme") {
		t.Fatalf("Attach tenant = %q, want acme", fr.attachTenant)
	}
}

func TestObserveMarksGap(t *testing.T) {
	live := make(chan event.Event, 1)
	fr := &fakeRuntime{
		attachHist: []event.Event{
			{Seq: 1, Kind: event.KindTurnStart},
			{Seq: 2, Kind: event.KindModelCallStart},
		},
		observeCh: live,
	}

	sr, sw := io.Pipe()
	cr, cw := io.Pipe()
	s := NewServer(cr, sw, fr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = s.Serve(ctx) }()
	go func() {
		enc := json.NewEncoder(cw)
		_ = enc.Encode(req{JSONRPC: "2.0", ID: json.RawMessage(`11`), Method: "loop.observe", Params: json.RawMessage(`{"loop_id":"loop-abc"}`)})
	}()

	dec := json.NewDecoder(sr)

	// Drain the response + the two replay notes.
	var f0 rawFrame
	if err := dec.Decode(&f0); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	readNote(t, dec)
	readNote(t, dec)

	// Feed Seq 5 (watermark was 2) — first post-gap live event must be gap:true.
	live <- event.Event{Seq: 5, Kind: event.KindTurnEnd}
	np := readNote(t, dec)
	if np.Event.Seq != 5 {
		t.Fatalf("live note seq = %d, want 5", np.Event.Seq)
	}
	if !np.Gap {
		t.Fatalf("live note seq 5 should be flagged gap (prev watermark 2)")
	}
}

// TestObserveDedupesReplayLiveOverlap proves the reattach "no duplication" contract:
// an event whose Seq is already covered by the replay watermark must NOT be
// re-delivered by the live pump, and a genuine gap AFTER the watermark is still
// flagged. Here Attach replays Seq 1,2,3 (watermark=3) and the live channel ALSO
// delivers Seq 3 (the overlap) then Seq 4 — the client must see 1,2,3,4 exactly once
// (3 not doubled) with no spurious gap on 4. A second observe with an overlap then a
// post-watermark hole (Seq 6 after watermark 3) must flag gap:true on 6.
func TestObserveDedupesReplayLiveOverlap(t *testing.T) {
	live := make(chan event.Event, 2)
	fr := &fakeRuntime{
		attachHist: []event.Event{
			{Seq: 1, Kind: event.KindTurnStart},
			{Seq: 2, Kind: event.KindModelCallStart},
			{Seq: 3, Kind: event.KindModelCallEnd},
		},
		observeCh: live,
	}

	sr, sw := io.Pipe()
	cr, cw := io.Pipe()
	s := NewServer(cr, sw, fr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = s.Serve(ctx) }()
	go func() {
		enc := json.NewEncoder(cw)
		_ = enc.Encode(req{JSONRPC: "2.0", ID: json.RawMessage(`12`), Method: "loop.observe", Params: json.RawMessage(`{"loop_id":"loop-abc"}`)})
	}()

	dec := json.NewDecoder(sr)

	// Response: replayed:3, last_seq:3.
	var f0 rawFrame
	if err := dec.Decode(&f0); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	var res observeResult
	if err := json.Unmarshal(f0.Result, &res); err != nil {
		t.Fatalf("unmarshal observeResult: %v", err)
	}
	if res.Replayed != 3 || res.LastSeq != 3 {
		t.Fatalf("observeResult = %+v, want replayed:3 last_seq:3", res)
	}

	// Replay notes for Seq 1,2,3.
	for _, wantSeq := range []event.Seq{1, 2, 3} {
		np := readNote(t, dec)
		if np.Event.Seq != wantSeq {
			t.Fatalf("replay note seq = %d, want %d", np.Event.Seq, wantSeq)
		}
		if np.Gap {
			t.Fatalf("replay note seq %d unexpectedly flagged gap", wantSeq)
		}
	}

	// Live channel delivers the OVERLAP (Seq 3) then Seq 4. Seq 3 must be skipped;
	// the NEXT note the client reads must be Seq 4, not a duplicated Seq 3.
	live <- event.Event{Seq: 3, Kind: event.KindModelCallEnd}
	live <- event.Event{Seq: 4, Kind: event.KindTurnEnd}
	np := readNote(t, dec)
	if np.Event.Seq != 4 {
		t.Fatalf("live note seq = %d, want 4 (Seq 3 overlap must be deduped, not doubled)", np.Event.Seq)
	}
	if np.Gap {
		t.Fatalf("live note seq 4 unexpectedly flagged gap (contiguous with watermark 3)")
	}
}

// TestObserveMarksGapAfterOverlap proves that after deduping an overlap, a true hole
// past the watermark is still flagged: watermark 3, live delivers overlap Seq 3 then
// Seq 6 — Seq 6 carries gap:true.
func TestObserveMarksGapAfterOverlap(t *testing.T) {
	live := make(chan event.Event, 2)
	fr := &fakeRuntime{
		attachHist: []event.Event{
			{Seq: 1, Kind: event.KindTurnStart},
			{Seq: 2, Kind: event.KindModelCallStart},
			{Seq: 3, Kind: event.KindModelCallEnd},
		},
		observeCh: live,
	}

	sr, sw := io.Pipe()
	cr, cw := io.Pipe()
	s := NewServer(cr, sw, fr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = s.Serve(ctx) }()
	go func() {
		enc := json.NewEncoder(cw)
		_ = enc.Encode(req{JSONRPC: "2.0", ID: json.RawMessage(`13`), Method: "loop.observe", Params: json.RawMessage(`{"loop_id":"loop-abc"}`)})
	}()

	dec := json.NewDecoder(sr)
	var f0 rawFrame
	if err := dec.Decode(&f0); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	readNote(t, dec)
	readNote(t, dec)
	readNote(t, dec)

	// Overlap Seq 3 (deduped) then a hole to Seq 6 — Seq 6 must be gap:true.
	live <- event.Event{Seq: 3, Kind: event.KindModelCallEnd}
	live <- event.Event{Seq: 6, Kind: event.KindTurnEnd}
	np := readNote(t, dec)
	if np.Event.Seq != 6 {
		t.Fatalf("live note seq = %d, want 6", np.Event.Seq)
	}
	if !np.Gap {
		t.Fatalf("live note seq 6 should be flagged gap (watermark 3, hole 4-5)")
	}
}

// TestServeMalformedFrameEmitsParseErrorThenFailsFast proves the decode-error
// contract (FIX 4): a streaming json.Decoder cannot resync mid-stream after a syntax
// error (it returns the same error forever), so Serve emits a null-id parse-error
// response (codeParseError) and then fails fast — the trailing VALID frame after a
// malformed one is NOT processed. This documents the fail-fast choice and exercises
// the codeParseError constant.
func TestServeMalformedFrameEmitsParseErrorThenFailsFast(t *testing.T) {
	fr := &fakeRuntime{startID: "loop-xyz"}

	sr, sw := io.Pipe()
	cr, cw := io.Pipe()
	s := NewServer(cr, sw, fr)

	serveDone := make(chan error, 1)
	go func() { serveDone <- s.Serve(context.Background()) }()

	// A malformed frame followed by a well-formed loop.start. The valid frame must NOT
	// be processed (json.Decoder cannot resync).
	go func() {
		_, _ = io.WriteString(cw, `{bad json`+"\n")
		_, _ = io.WriteString(cw, `{"jsonrpc":"2.0","id":9,"method":"loop.start","params":{"task":"t"}}`+"\n")
		_ = cw.Close()
	}()

	dec := json.NewDecoder(sr)
	// First (and only) frame the client sees is the parse-error response: null id,
	// codeParseError.
	var f rawFrame
	if err := dec.Decode(&f); err != nil {
		t.Fatalf("decode parse-error response: %v", err)
	}
	if f.Error == nil || f.Error.Code != codeParseError {
		t.Fatalf("frame = %+v, want a codeParseError (%d) error", f, codeParseError)
	}
	if string(f.ID) != "null" {
		t.Fatalf("parse-error response id = %s, want null", f.ID)
	}

	// Serve must return (fail-fast) rather than process the trailing valid frame; if it
	// had processed it, the fake would record startCfg.Task = "t".
	if err := <-serveDone; err == nil {
		t.Fatal("Serve returned nil, want a decode error (fail-fast)")
	}
	if fr.startCfg.Task == "t" {
		t.Fatal("Serve processed the trailing valid frame after a malformed one (should fail fast)")
	}
}

// readNote decodes the next frame and asserts it is an id-less loop.event
// notification, returning its params.
func readNote(t *testing.T, dec *json.Decoder) eventNoteParams {
	t.Helper()
	var f rawFrame
	if err := dec.Decode(&f); err != nil {
		t.Fatalf("decode note: %v", err)
	}
	if len(f.ID) != 0 {
		t.Fatalf("expected id-less notification, got id=%s", f.ID)
	}
	if f.Method != "loop.event" {
		t.Fatalf("note method = %q, want loop.event", f.Method)
	}
	var np eventNoteParams
	if err := json.Unmarshal(f.Params, &np); err != nil {
		t.Fatalf("unmarshal eventNoteParams: %v", err)
	}
	return np
}
