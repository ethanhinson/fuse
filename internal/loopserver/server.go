// Package loopserver is binding #2: a dedicated stdio JSON-RPC 2.0 server that
// exposes loop control (loop.start / loop.send / loop.observe) over a
// runtime.Runtime. It is a pure adapter — it imports internal/runtime and
// internal/event only, and NEVER a renderer, TUI, MCP tool registry, or CLI type
// (Decision Note D-2). Its "no approval gate" (auto-approve) policy is a binding
// choice wired by the composition root, not a property of the Runtime seam.
//
// The framing mirrors internal/mcp/server.go's proven encoder discipline: every
// write (responses AND id-less notifications) is serialized under a shared encoder
// mutex so a mid-observe loop.event notification can never interleave with a
// response and corrupt the JSON-RPC stream.
package loopserver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"

	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/runtime"
)

// JSON-RPC 2.0 error codes (subset used by loopserver).
const (
	codeParseError     = -32700
	codeInvalidParams  = -32602
	codeMethodNotFound = -32601
	codeInternal       = -32603
)

// Server is a stdio JSON-RPC 2.0 server dedicated to loop control (binding #2). It
// is a pure adapter over runtime.Runtime — no renderer, TUI, approval gate, or MCP
// tool registry. Its policy is "no approval gate" (auto-approve), a binding choice
// documented at the composition root, NOT a property of the Runtime seam.
type Server struct {
	rt  runtime.Runtime
	enc *json.Encoder
	dec *json.Decoder

	// encMu serializes every write on the shared encoder. loop.observe emits id-less
	// loop.event notifications from a pump goroutine on the same encoder as the
	// eventual observe response — without this lock those writes could interleave and
	// corrupt the JSON-RPC stream (mirrors mcp/server.go's $/progress discipline).
	encMu sync.Mutex
}

// NewServer creates a Server that reads JSON-RPC 2.0 frames from r and writes them
// to w, adapting each method to rt.
func NewServer(r io.Reader, w io.Writer, rt runtime.Runtime) *Server {
	return &Server{
		rt:  rt,
		enc: json.NewEncoder(w),
		dec: json.NewDecoder(r),
	}
}

// req is the server-side JSON-RPC 2.0 request frame. ID is RawMessage so it
// roundtrips for both string and integer client IDs (identical shape to mcp).
type req struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// resp is the server-side JSON-RPC 2.0 response frame.
type resp struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// rpcError is the JSON-RPC 2.0 error object.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// method params / results
type startParams struct {
	Task  string `json:"task"`
	Model string `json:"model,omitempty"`
}
type startResult struct {
	LoopID string `json:"loop_id"`
}
type sendParams struct {
	LoopID string `json:"loop_id"`
	Input  string `json:"input"`
}
type observeParams struct {
	LoopID  string    `json:"loop_id"`
	FromSeq event.Seq `json:"from_seq,omitempty"`
}
type observeResult struct {
	Replayed int       `json:"replayed"`
	LastSeq  event.Seq `json:"last_seq"`
}

// eventNote is the id-less loop.event notification frame carrying one event to an
// observing client. json.Encoder writes it immediately (no buffering), so a raw
// json.Decoder client reads each notification as it is pushed.
type eventNote struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  eventNoteParams `json:"params"`
}
type eventNoteParams struct {
	LoopID string      `json:"loop_id"`
	Event  event.Event `json:"event"`
	Gap    bool        `json:"gap,omitempty"`
}

// Serve reads and dispatches JSON-RPC frames until the reader hits EOF or ctx is
// done via a decode error. Notifications (no id) receive no response.
func (s *Server) Serve(ctx context.Context) error {
	for {
		var r req
		if err := s.dec.Decode(&r); err != nil {
			if err == io.EOF {
				return nil
			}
			return fmt.Errorf("loopserver: decode: %w", err)
		}
		if len(r.ID) == 0 { // notification: no response
			continue
		}
		// loop.observe writes its own response (before the replay notifications) and
		// spawns a live-tail pump, so it is not routed through the return-and-encode
		// path; every other method returns a single resp that Serve encodes.
		if r.Method == "loop.observe" {
			if err := s.serveObserve(ctx, r); err != nil {
				return err
			}
			continue
		}
		out := s.dispatch(ctx, r)
		if err := s.encode(out); err != nil {
			return fmt.Errorf("loopserver: encode: %w", err)
		}
	}
}

// encode serializes one frame (response or notification) under the shared encoder
// mutex.
func (s *Server) encode(v any) error {
	s.encMu.Lock()
	defer s.encMu.Unlock()
	return s.enc.Encode(v)
}

func (s *Server) dispatch(ctx context.Context, r req) resp {
	switch r.Method {
	case "loop.start":
		return s.handleStart(ctx, r)
	case "loop.send":
		return s.handleSend(ctx, r)
	default:
		return s.errResp(r.ID, codeMethodNotFound, "method not found: "+r.Method)
	}
}

func (s *Server) handleStart(ctx context.Context, r req) resp {
	var p startParams
	if err := json.Unmarshal(r.Params, &p); err != nil {
		return s.errResp(r.ID, codeInvalidParams, "invalid params: "+err.Error())
	}
	h, err := s.rt.StartLoop(ctx, runtime.LoopConfig{Task: p.Task, ModelID: p.Model})
	if err != nil {
		return s.errResp(r.ID, codeInternal, err.Error())
	}
	return s.okResp(r.ID, startResult{LoopID: h.ID()})
}

func (s *Server) handleSend(ctx context.Context, r req) resp {
	var p sendParams
	if err := json.Unmarshal(r.Params, &p); err != nil {
		return s.errResp(r.ID, codeInvalidParams, "invalid params: "+err.Error())
	}
	if err := s.rt.Send(ctx, p.LoopID, p.Input); err != nil {
		return s.errResp(r.ID, codeInternal, err.Error())
	}
	return s.okResp(r.ID, map[string]any{})
}

// serveObserve subscribes to the loop's live tail BEFORE replaying durable history
// (so no event slips between replay-end and live-start), writes the observe
// response carrying the replay watermark FIRST, replays events since from_seq as
// id-less loop.event notifications, then live-tails the channel on a goroutine
// pushing further loop.event notifications until ctx is done or the channel closes.
// The FIRST post-gap live event is flagged gap:true (ADR-0025 drop-newest
// detection: ev.Seq > prev+1), telling the client to re-observe from its last-seen
// seq. It writes its own frames (response + notifications) under the shared encoder
// mutex rather than returning a resp, because it emits many frames, not one.
func (s *Server) serveObserve(ctx context.Context, r req) error {
	var p observeParams
	if err := json.Unmarshal(r.Params, &p); err != nil {
		return s.encode(s.errResp(r.ID, codeInvalidParams, "invalid params: "+err.Error()))
	}
	// 1) Subscribe BEFORE replay so no event slips between replay end and live start.
	//    The channel is buffered; nothing is drained until the pump starts (after the
	//    response + replay notes), so live events queue rather than race the replay.
	ch, cancel, err := s.rt.Observe(p.LoopID)
	if err != nil {
		return s.encode(s.errResp(r.ID, codeInternal, err.Error()))
	}
	// 2) Read durable history since from_seq.
	hist, err := s.rt.Attach(p.LoopID, p.FromSeq)
	if err != nil {
		cancel()
		return s.encode(s.errResp(r.ID, codeInternal, err.Error()))
	}
	last := p.FromSeq
	if n := len(hist); n > 0 {
		last = hist[n-1].Seq
	}
	// 3) Response acknowledges the subscription and the replay watermark FIRST, so a
	//    raw client reads the resp before the replayed loop.event notifications.
	if err := s.encode(s.okResp(r.ID, observeResult{Replayed: len(hist), LastSeq: last})); err != nil {
		cancel()
		return fmt.Errorf("loopserver: encode: %w", err)
	}
	// 4) Replay durable history, each as an id-less loop.event notification.
	for _, ev := range hist {
		s.pushEvent(p.LoopID, ev, false)
	}
	// 5) Live tail on a goroutine until ctx done or the loop's store closes (channel
	//    closed). Detect a Seq gap (ADR-0025 drop-newest) and flag the first post-gap
	//    event so the client Attach-replays the hole.
	go func() {
		defer cancel()
		prev := last
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-ch:
				if !ok {
					return
				}
				gap := ev.Seq > prev+1 && prev != 0
				s.pushEvent(p.LoopID, ev, gap)
				prev = ev.Seq
			}
		}
	}()
	return nil
}

// pushEvent writes one id-less loop.event notification under the shared encoder
// mutex (via encode), serializing it against responses and other notifications.
func (s *Server) pushEvent(loopID string, ev event.Event, gap bool) {
	_ = s.encode(eventNote{
		JSONRPC: "2.0",
		Method:  "loop.event",
		Params:  eventNoteParams{LoopID: loopID, Event: ev, Gap: gap},
	})
}

func (s *Server) okResp(id json.RawMessage, v any) resp {
	raw, _ := json.Marshal(v)
	return resp{JSONRPC: "2.0", ID: id, Result: raw}
}

func (s *Server) errResp(id json.RawMessage, code int, msg string) resp {
	return resp{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}}
}
