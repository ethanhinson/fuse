package loopconnect

import (
	"context"
	"time"

	"connectrpc.com/connect"

	"github.com/ethanhinson/fuse/internal/event"
	loopv1 "github.com/ethanhinson/fuse/internal/loopwire/v1"
)

// defaultKeepalive is the idle Observe heartbeat interval. It is BELOW a common
// gateway/proxy idle default (60s) so a parked-but-alive loop's stream is not reaped
// mid-park (Task 4). A test injects a short interval via WithKeepalive.
const defaultKeepalive = 20 * time.Second

// WithKeepalive overrides the idle Observe heartbeat interval (test seam; a short
// value makes the keepalive assertions fast). A non-positive value keeps the default.
func (h *Handler) WithKeepalive(d time.Duration) *Handler {
	if d > 0 {
		h.keepalive = d
	}
	return h
}

func (h *Handler) keepaliveInterval() time.Duration {
	if h.keepalive > 0 {
		return h.keepalive
	}
	return defaultKeepalive
}

// Observe streams a loop's history since from_seq then live events, porting the WS-
// era serveObserve discipline VERBATIM in intent (loopserver.serveObserve):
//
//  1. Subscribe to the live tail FIRST (before replay) so no event slips between
//     replay-end and live-start. An event appended in the subscribe→replay window is
//     delivered on BOTH paths; the live loop dedups it against the replay watermark.
//  2. Read durable history since from_seq (Attach), advancing the watermark `last`.
//  3. Replay history as ObserveEvent frames (gap always false on replay).
//  4. Live-tail the channel, dropping ev.Seq <= last (the subscribe/replay overlap),
//     flagging gap = ev.Seq > prev+1 && prev != 0 on the first genuinely-new event
//     past a hole (ADR-0025 drop-newest re-observe hint).
//
// Every post-open send/context error is a CLEAN end (return nil-equivalent teardown),
// never a server fault — the Connect analogue of websocket-read-errors-are-not-
// closeerror. cancel() runs on EVERY return path (leak-free): the subscription and
// the keepalive ticker are released whether the stream ends by ctx cancel, channel
// close, or a client-side send error.
//
// While idle (no live event within the keepalive interval), the stream emits a bare
// keepalive:true frame (no event) so a parked loop's stream survives a gateway idle
// timeout; the client ignores keepalives for dedup (they carry no Seq).
func (h *Handler) Observe(ctx context.Context, req *connect.Request[loopv1.ObserveRequest], stream *connect.ServerStream[loopv1.ObserveEvent]) error {
	m := req.Msg
	tenant := event.TenantID(m.Tenant)

	// 1) Subscribe BEFORE replay. cancel() unsubscribes; it is deferred so EVERY
	//    return path (replay error, live error, ctx cancel, channel close) releases
	//    the subscription — no leaked goroutine/subscription.
	ch, cancel, err := h.rt.Observe(ctx, tenant, m.LoopId)
	if err != nil {
		return connect.NewError(connect.CodeInternal, err)
	}
	defer cancel()

	// 2) Read durable history since from_seq.
	hist, err := h.rt.Attach(ctx, tenant, m.LoopId, event.Seq(m.FromSeq))
	if err != nil {
		return connect.NewError(connect.CodeInternal, err)
	}
	last := event.Seq(m.FromSeq)
	if n := len(hist); n > 0 {
		last = hist[n-1].Seq
	}

	// 3) Replay durable history. A send error mid-replay is a clean client-gone end.
	for _, ev := range hist {
		if err := stream.Send(&loopv1.ObserveEvent{Event: toProtoEvent(ev)}); err != nil {
			return nil // client hung up: clean end, not a fault.
		}
	}

	// 4) Live tail with an idle keepalive. prev starts at the replay watermark so the
	//    first genuinely-new live event's gap is measured against it.
	ticker := time.NewTicker(h.keepaliveInterval())
	defer ticker.Stop()
	prev := last
	for {
		select {
		case <-ctx.Done():
			return nil // ctx cancel (shutdown/client abort): clean teardown.
		case <-ticker.C:
			// Idle heartbeat: a bare keepalive frame (no event) so a parked stream is
			// not reaped. A send error means the client is gone → clean end.
			if err := stream.Send(&loopv1.ObserveEvent{Keepalive: true}); err != nil {
				return nil
			}
		case ev, ok := <-ch:
			if !ok {
				return nil // store closed: clean end.
			}
			// Drop any event already covered by replay (the subscribe/attach overlap);
			// this must NOT advance prev, so the next new event's gap is still measured
			// against the watermark.
			if ev.Seq <= last {
				continue
			}
			gap := ev.Seq > prev+1 && prev != 0
			if err := stream.Send(&loopv1.ObserveEvent{Event: toProtoEvent(ev), Gap: gap}); err != nil {
				return nil // client-side send error: clean reconnect, not a fault.
			}
			prev = ev.Seq
			// Reset the idle timer: we just delivered an event, so the next keepalive is
			// a full interval away.
			ticker.Reset(h.keepaliveInterval())
		}
	}
}
