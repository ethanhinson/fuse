package loopserver

// This file is binding #3's WebSocket transport: the WS implementation of the
// transport-agnostic conn abstraction (transport.go) plus the ServeWS entrypoint
// that runs one full loop.* session over a single WebSocket connection. It reuses
// the extracted dispatch core (server.go) verbatim — no replay/observe/dedup logic
// is reimplemented here.
//
// This file also hosts the thin stateless HTTP replay endpoint (Attach catch-up):
// a plain GET exposing runtime.Runtime.Attach(loop_id, from) as durable history,
// distinct from the WS full-session transport — no live tail, no connection state.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"

	"github.com/coder/websocket"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/runtime"
)

// wsTransport is the WebSocket implementation of conn. It reads discrete JSON-RPC
// request frames from one *websocket.Conn and writes response + id-less loop.event
// notification frames back, serialized under a single per-connection write mutex.
//
// coder/websocket's Write is NOT concurrency-safe, so the mutex is mandatory — it is
// the same shared-write-serialization contract stdio's encMu provides: a mid-observe
// loop.event notification (pushed from the observe pump goroutine) must never
// interleave with another write and corrupt the frame stream.
//
// Because WS delivers DISCRETE messages, readRequest handles parse errors
// per-message (there is no streaming decoder to resync): a malformed message emits
// one null-id parse-error frame via the shared parseErrorFrame path the core expects
// and returns a non-EOF error, which the core surfaces by returning (tearing down the
// session).
type wsTransport struct {
	c   *websocket.Conn
	ctx context.Context

	// writeMu serializes every Write on this connection. See the type doc: coder/
	// websocket Write is not concurrency-safe, and the observe pump writes loop.event
	// notifications concurrently with response writes.
	writeMu sync.Mutex
}

// newWSTransport builds a WS transport over c bound to ctx (the session lifetime
// context: Read/Write are cancelled when ctx is done and when the connection drops).
func newWSTransport(ctx context.Context, c *websocket.Conn) *wsTransport {
	return &wsTransport{c: c, ctx: ctx}
}

// encode marshals v and writes it as one text WebSocket message under the shared
// per-connection write mutex.
func (t *wsTransport) encode(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("loopserver: ws marshal: %w", err)
	}
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	return t.c.Write(t.ctx, websocket.MessageText, b)
}

// readRequest reads one discrete WS message and unmarshals it into a req frame. A
// clean client close is reported as io.EOF (via the io.EOF the core's Serve loop
// checks) so the core ends the session without error.
//
// On a malformed message this emits ONE null-id parse-error frame (mirroring the
// stdio transport's fail-fast policy and reusing the shared parseErrorFrame builder
// so the frame is byte-identical), then returns a non-EOF error the core surfaces by
// returning. Unlike stdio there is no streaming decoder that must resync — WS frames
// are message-delimited — but the session is still torn down on a corrupt frame,
// matching binding #2's contract.
func (t *wsTransport) readRequest(ctx context.Context) (req, error) {
	_, data, err := t.c.Read(ctx)
	if err != nil {
		// A clean or abnormal close ends the read loop. Normalize any close (and a
		// cancelled context) to io.EOF so the core's Serve loop returns nil, matching
		// stdio's clean-EOF path. The core compares err == io.EOF exactly, so we must
		// return io.EOF itself. Non-close read errors are surfaced as-is.
		if isWSClosed(err) || ctx.Err() != nil {
			return req{}, io.EOF
		}
		return req{}, fmt.Errorf("loopserver: ws read: %w", err)
	}
	var r req
	if err := json.Unmarshal(data, &r); err != nil {
		_ = t.encode(parseErrorFrame(err.Error()))
		return req{}, fmt.Errorf("loopserver: ws decode: %w", err)
	}
	return r, nil
}

// isWSClosed reports whether err indicates the WebSocket connection has closed
// (either side), so the read loop can end cleanly.
func isWSClosed(err error) bool {
	var ce websocket.CloseError
	if errors.As(err, &ce) {
		return true
	}
	// Dial/Accept teardown and abnormal closes surface as generic errors; treating a
	// net-closed read as EOF keeps ServeWS returning nil on client drop.
	return errors.Is(err, context.Canceled)
}

// ServeWS runs one full loop.* JSON-RPC session over a single WebSocket connection
// against rt. It builds a Server over the WS transport and drives Serve(ctx),
// reusing the extracted dispatch/observe/replay core verbatim. It returns when the
// client disconnects, ctx is cancelled, or a fatal frame error occurs, then closes
// the connection.
func ServeWS(ctx context.Context, c *websocket.Conn, rt runtime.Runtime) error {
	s := &Server{rt: rt, conn: newWSTransport(ctx, c)}
	err := s.Serve(ctx)
	// Best-effort clean close; ignore the error (the peer may already be gone).
	_ = c.Close(websocket.StatusNormalClosure, "")
	return err
}

// --- Thin stateless HTTP replay endpoint (binding #3, D3) ------------------------

// NewReplayHandler returns an http.Handler exposing the durable catch-up path over
// the runtime seam: GET /loops/{id}/events?from=<seq>&tenant=<t> answers with the
// history rt.Attach(ctx, tenant, id, from) returns, as a JSON array of event.Event
// (seq > from), Content-Type application/json.
//
// It is deliberately STATELESS: every request is an independent, idempotent history
// read with no live tail and no accumulated connection state — the WS transport
// (ServeWS) owns live observe and all mutation (loop.start/loop.send). HTTP is
// read-only replay only (D3): there is no start/send route here.
//
// tenant is carried present-but-unenforced via the ?tenant= query param (identity is
// #0049), defaulting to empty when absent — the same way the WS observe params carry
// it. from defaults to 0 and MUST parse as a non-negative uint64 (malformed or
// negative -> 400). An Attach error (e.g. an unknown loop) maps to 404 with a JSON
// error body.
//
// Task 5's loop-serve-net subcommand mounts this handler (alongside the WS accept
// handler) on the process's http.ServeMux.
func NewReplayHandler(rt runtime.Runtime) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /loops/{id}/events", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		tenant := r.URL.Query().Get("tenant") // unenforced (#0049); "" when absent.

		// from defaults to 0; reject a nonparsable or negative value with 400.
		from := uint64(0)
		if raw := r.URL.Query().Get("from"); raw != "" {
			v, err := strconv.ParseUint(raw, 10, 64)
			if err != nil {
				writeJSONError(w, http.StatusBadRequest, "invalid from: "+raw)
				return
			}
			from = v
		}

		hist, err := rt.Attach(r.Context(), event.TenantID(tenant), id, event.Seq(from))
		if err != nil {
			// An Attach failure (unknown loop, etc.) is a client-addressable 404 — the
			// requested loop's durable history could not be resolved.
			writeJSONError(w, http.StatusNotFound, err.Error())
			return
		}

		// Marshal the slice; a nil/empty history must still be a JSON array, never null,
		// so the response shape is stable.
		if hist == nil {
			hist = []event.Event{}
		}
		body, err := json.Marshal(hist)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "marshal history: "+err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	})
	return mux
}

// writeJSONError writes a JSON error body ({"error": msg}) with the given status and
// Content-Type application/json, so every non-2xx response is machine-parseable.
func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	body, _ := json.Marshal(map[string]string{"error": msg})
	_, _ = w.Write(body)
}
