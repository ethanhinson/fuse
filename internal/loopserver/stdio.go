package loopserver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

// stdioTransport is the stdio (io.Reader/io.Writer) implementation of conn: a
// streaming json.Decoder for reads and a json.Encoder for writes, with a single
// mutex serializing every write. It is the transport binding #2 has always used;
// NewServer builds one internally so binding #2's signature and behavior are
// unchanged.
type stdioTransport struct {
	enc *json.Encoder
	dec *json.Decoder

	// encMu serializes every write on the shared encoder. loop.observe emits id-less
	// loop.event notifications from a pump goroutine on the same encoder as the
	// eventual observe response — without this lock those writes could interleave and
	// corrupt the JSON-RPC stream (mirrors mcp/server.go's $/progress discipline).
	encMu sync.Mutex
}

// newStdioTransport builds a stdio transport reading frames from r and writing them
// to w. r and w may be nil for tests that only exercise dispatch directly (the
// underlying json.Encoder/json.Decoder bind lazily), matching the pre-refactor
// NewServer(nil, nil, rt) behavior.
func newStdioTransport(r io.Reader, w io.Writer) *stdioTransport {
	return &stdioTransport{
		enc: json.NewEncoder(w),
		dec: json.NewDecoder(r),
	}
}

// encode serializes one frame (response or notification) under the shared encoder
// mutex.
func (t *stdioTransport) encode(v any) error {
	t.encMu.Lock()
	defer t.encMu.Unlock()
	return t.enc.Encode(v)
}

// readRequest decodes one frame from the streaming decoder. On io.EOF it returns
// io.EOF verbatim so the core's Serve loop ends cleanly.
//
// Decode-error policy is FAIL-FAST (verified, not chosen for convenience): a
// streaming json.Decoder cannot resync mid-stream — after a syntax error it returns
// that same error indefinitely, so a trailing valid frame is unreachable. Rather
// than spin, on a non-EOF decode error this emits ONE null-id parse-error response
// (codeParseError) so the client learns its stream is corrupt, then returns the
// wrapped error, which the core surfaces by returning (tearing down the
// connection). A fresh connection re-establishes a clean decoder.
func (t *stdioTransport) readRequest(_ context.Context) (req, error) {
	var r req
	if err := t.dec.Decode(&r); err != nil {
		if err == io.EOF {
			return req{}, io.EOF
		}
		// Best-effort notify the client the stream is unparseable, then fail fast
		// (json.Decoder cannot resync — see the method doc).
		_ = t.encode(parseErrorFrame(err.Error()))
		return req{}, fmt.Errorf("loopserver: decode: %w", err)
	}
	return r, nil
}
