// Package loopconnect is the Connect/protobuf transport edge for the networked loop-
// control binding (change 0055, fuse.loop.v1) — successor to the JSON-over-WebSocket
// wire (#48, ADR-0032 superseded). It implements loopv1connect.LoopServiceHandler
// over a runtime.Runtime: StartLoop / Send (unary) and Observe (server-streaming
// history-then-live). It is a pure adapter — it imports internal/runtime,
// internal/event, and the generated loopwire stubs only, and NEVER a renderer, TUI,
// MCP tool registry, or CLI type. Its "no approval gate" (auto-approve) policy is the
// composition root's choice, not a property of the Runtime seam.
//
// The package owns the transport-edge mapping between internal/event.Event (kept
// transport-free) and the proto loopv1.Event via toProtoEvent/fromProtoEvent, so no
// proto type ever leaks into internal/event.
package loopconnect

import (
	"context"
	"errors"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/ethanhinson/fuse/internal/event"
	loopv1 "github.com/ethanhinson/fuse/internal/loopwire/v1"
	"github.com/ethanhinson/fuse/internal/loopwire/v1/loopv1connect"
	"github.com/ethanhinson/fuse/internal/runtime"
)

// Handler adapts a runtime.Runtime to the generated LoopServiceHandler. Tenant is a
// pass-through, unenforced typed field on every RPC (identity is #0049): the tenant
// string maps to event.TenantID and is threaded to the seam call, never dropped
// (cache-over-tenant-scoped-source-reassert-key-on-hit).
type Handler struct {
	rt runtime.Runtime

	// baseCtx is the LOOP-LIFETIME context StartLoop launches loops under. It is
	// decoupled from the per-unary-request context on purpose: a unary StartLoop
	// request context is cancelled the instant the RPC returns its loop_id, so tying
	// the loop's Run to it would kill the loop before its first turn completes. baseCtx
	// is the server's lifetime (set by the composition root via WithBaseContext); its
	// cancellation is the correct loop-shutdown signal. Defaults to context.Background.
	baseCtx context.Context

	// keepalive is the idle Observe heartbeat interval (Task 4). Zero uses the
	// package default (defaultKeepalive); a test injects a short interval.
	keepalive time.Duration
}

// compile-time assertion that Handler satisfies the generated service interface.
var _ loopv1connect.LoopServiceHandler = (*Handler)(nil)

// NewHandler builds a Handler over rt. Loops launch under context.Background() unless
// a lifetime context is supplied via WithBaseContext.
func NewHandler(rt runtime.Runtime) *Handler {
	return &Handler{rt: rt, baseCtx: context.Background()}
}

// WithBaseContext sets the loop-lifetime context StartLoop launches loops under (the
// server's serve/shutdown context), so a server shutdown tears running loops down and
// a returning unary request does NOT. A nil ctx keeps the default (Background).
func (h *Handler) WithBaseContext(ctx context.Context) *Handler {
	if ctx != nil {
		h.baseCtx = ctx
	}
	return h
}

// StartLoop constructs and drives one loop from the request, returning its loop_id.
// A StartLoop seam error maps to CodeInternal (loop construction/drive failure is a
// server-side condition here).
func (h *Handler) StartLoop(ctx context.Context, req *connect.Request[loopv1.StartLoopRequest]) (*connect.Response[loopv1.StartLoopResponse], error) {
	m := req.Msg
	// Launch under the LOOP-LIFETIME context, NOT the per-request ctx (which connect
	// cancels the moment this unary RPC returns — that would kill the loop before its
	// first turn). See Handler.baseCtx.
	handle, err := h.rt.StartLoop(h.baseCtx, runtime.LoopConfig{
		Task:        m.Task,
		ModelID:     m.Model,
		Tenant:      event.TenantID(m.Tenant),
		Interactive: m.Interactive,
	})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&loopv1.StartLoopResponse{LoopId: handle.ID()}), nil
}

// Send injects human input at the loop's next turn boundary. It maps the two client-
// addressable seam sentinels: an unknown loop → CodeNotFound, a finished loop →
// CodeFailedPrecondition (the loop exists but no longer accepts input). Any other
// error → CodeInternal.
func (h *Handler) Send(ctx context.Context, req *connect.Request[loopv1.SendRequest]) (*connect.Response[loopv1.SendResponse], error) {
	m := req.Msg
	if err := h.rt.Send(ctx, event.TenantID(m.Tenant), m.LoopId, m.Input); err != nil {
		switch {
		case errors.Is(err, runtime.ErrLoopNotFound):
			return nil, connect.NewError(connect.CodeNotFound, err)
		case errors.Is(err, runtime.ErrLoopFinished):
			return nil, connect.NewError(connect.CodeFailedPrecondition, err)
		default:
			return nil, connect.NewError(connect.CodeInternal, err)
		}
	}
	return connect.NewResponse(&loopv1.SendResponse{}), nil
}

// toProtoEvent maps an internal event.Event to its proto edge representation. It is
// the ONLY place a proto Event is built from a domain event, keeping internal/event
// transport-free.
func toProtoEvent(e event.Event) *loopv1.Event {
	return &loopv1.Event{
		Seq:      uint64(e.Seq),
		Ts:       timestamppb.New(e.TS),
		NodeId:   e.NodeID,
		ParentId: e.ParentID,
		Depth:    int32(e.Depth),
		Turn:     int32(e.Turn),
		Kind:     string(e.Kind),
		Payload:  []byte(e.Payload),
	}
}

// fromProtoEvent maps a proto Event back to a domain event.Event (the reverse edge,
// used by clients/tests). A nil ts yields the zero time.
func fromProtoEvent(pe *loopv1.Event) event.Event {
	e := event.Event{
		Seq:      event.Seq(pe.Seq),
		NodeID:   pe.NodeId,
		ParentID: pe.ParentId,
		Depth:    int(pe.Depth),
		Turn:     int(pe.Turn),
		Kind:     event.Kind(pe.Kind),
	}
	if len(pe.Payload) > 0 {
		e.Payload = append([]byte(nil), pe.Payload...)
	}
	if pe.Ts != nil {
		e.TS = pe.Ts.AsTime()
	}
	return e
}
