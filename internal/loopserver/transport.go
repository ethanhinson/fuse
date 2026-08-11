package loopserver

import (
	"context"
	"encoding/json"

	"github.com/ethanhinson/fuse/internal/event"
)

// conn is the transport-agnostic frame read/write abstraction the dispatch core
// (Server) drives. It decouples the core loop.* dispatch (loop.start / loop.send /
// loop.observe + server-push loop.event notifications) from any concrete wire
// (stdio today, WebSocket next), while preserving the single shared-write-mutex
// discipline: encode serializes every write on one connection so a mid-observe
// id-less loop.event notification can never interleave with another write and
// corrupt the frame stream.
//
// A transport owns exactly one connection's read side, write side, and the mutex
// that serializes writes on it — one mutex per connection.
type conn interface {
	// readRequest reads and decodes one request frame. It returns io.EOF (verbatim)
	// when the connection is closed cleanly so the core's Serve loop ends.
	//
	// On an undecodable frame the transport OWNS the parse-error policy: it emits
	// whatever error frame is appropriate for its wire (the stdio transport's
	// streaming decoder cannot resync, so it emits one null-id codeParseError frame
	// via encode and returns a wrapped error the core surfaces by returning) and
	// returns a non-nil, non-EOF error. The core does not interpret the error beyond
	// EOF-vs-not; it simply returns it.
	readRequest(ctx context.Context) (req, error)

	// encode writes one arbitrary frame (a resp, an eventNote, or the parse-error
	// frame) serialized under this connection's single write mutex.
	encode(v any) error
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
	Tenant string `json:"tenant,omitempty"`
}
type observeParams struct {
	LoopID  string    `json:"loop_id"`
	Tenant  string    `json:"tenant,omitempty"`
	FromSeq event.Seq `json:"from_seq,omitempty"`
}
type observeResult struct {
	Replayed int       `json:"replayed"`
	LastSeq  event.Seq `json:"last_seq"`
}

// eventNote is the id-less loop.event notification frame carrying one event to an
// observing client. It is written immediately (no buffering), so a client reads
// each notification as it is pushed.
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

// parseErrorFrame builds the null-id JSON-RPC parse-error response a transport
// emits before failing fast on an undecodable frame. Kept here so every transport
// produces the byte-identical frame binding #2 has always emitted.
func parseErrorFrame(detail string) resp {
	return resp{
		JSONRPC: "2.0",
		ID:      json.RawMessage("null"),
		Error:   &rpcError{Code: codeParseError, Message: "parse error: " + detail},
	}
}
