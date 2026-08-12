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
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/loopauth"
	loopv1 "github.com/ethanhinson/fuse/internal/loopwire/v1"
	"github.com/ethanhinson/fuse/internal/loopwire/v1/loopv1connect"
	"github.com/ethanhinson/fuse/internal/observe"
	observeotel "github.com/ethanhinson/fuse/internal/observe/otel"
	"github.com/ethanhinson/fuse/internal/runtime"
)

// Handler adapts a runtime.Runtime to the generated LoopServiceHandler. Tenant is a
// pass-through, unenforced typed field on every RPC (identity is #0049): the tenant
// string maps to event.TenantID and is threaded to the seam call, never dropped
// (cache-over-tenant-scoped-source-reassert-key-on-hit).
type Handler struct {
	rt       runtime.Runtime
	observer observe.Observer

	// registry is the OPTIONAL durable loop registry the edge reads (Resolve) to
	// authorize per-loop access and writes (Register) to record Owner at StartLoop.
	// It is READ-ONLY for authorization intent (the seam owns liveness). When nil,
	// per-loop ownership authorization is SKIPPED entirely — the pure-transport tests
	// that build NewHandler(rt) with no registry keep passing. Tenant-from-principal
	// derivation and tenant-spoof rejection still apply whenever a principal is
	// present, independent of the registry. The composition root injects it via
	// WithRegistry so the runtime seam never imports auth (ADR-0030): identity and
	// authorization live at this Connect edge, not in internal/runtime.
	registry event.LoopRegistry

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
	return &Handler{rt: rt, baseCtx: context.Background(), observer: observe.NoopObserver{}}
}

func (h *Handler) WithObserver(o observe.Observer) *Handler {
	if o != nil {
		h.observer = o
	}
	return h
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

// WithRegistry injects the durable loop registry the edge reads to authorize per-loop
// access and writes to record Owner at StartLoop. A nil registry keeps the default
// (no registry → per-loop ownership authorization is skipped, preserving the pure-
// transport tests). It is chainable like WithBaseContext/WithKeepalive.
func (h *Handler) WithRegistry(r event.LoopRegistry) *Handler {
	if r != nil {
		h.registry = r
	}
	return h
}

// effectiveTenant resolves the authoritative tenant for a request and rejects a
// tenant spoof. When a Principal is present (the interceptor authenticated the
// request), the principal's tenant is authoritative and the wire tenant, if non-
// empty, MUST equal it — a mismatch is a spoof (CodePermissionDenied). With no
// principal (an un-intercepted/registry-less test path), the wire tenant is used as-
// is, preserving the pure-transport behavior. The bool reports whether a principal
// was present (authorization applies) and returns it for the owner check.
func (h *Handler) effectiveTenant(ctx context.Context, wireTenant string) (event.TenantID, loopauth.Principal, bool, *connect.Error) {
	p, ok := PrincipalFrom(ctx)
	if !ok {
		return event.TenantID(wireTenant), loopauth.Principal{}, false, nil
	}
	if wireTenant != "" && event.TenantID(wireTenant) != p.Tenant {
		return "", p, true, connect.NewError(connect.CodePermissionDenied,
			errors.New("loopconnect: wire tenant does not match authenticated tenant"))
	}
	return p.Tenant, p, true, nil
}

// authorizeLoop enforces per-loop ownership for Send/Observe. It runs only when a
// registry AND a principal are present. It resolves the (tenant, loop) record —
// ErrLoopUnknown collapses to CodeNotFound (cross-tenant misses land here because the
// resolve is tenant-scoped), and a record owned by a different subject is
// CodePermissionDenied. An owner-less record (Owner == "") imposes no constraint.
func (h *Handler) authorizeLoop(ctx context.Context, tenant event.TenantID, loopID string, p loopauth.Principal, havePrincipal bool) *connect.Error {
	if h.registry == nil || !havePrincipal {
		return nil
	}
	rec, err := h.registry.Resolve(ctx, event.StreamKey{Tenant: tenant, Loop: event.LoopID(loopID)})
	if err != nil {
		if errors.Is(err, event.ErrLoopUnknown) {
			return connect.NewError(connect.CodeNotFound, err)
		}
		return connect.NewError(connect.CodeInternal, err)
	}
	if rec.Owner != "" && rec.Owner != p.Subject {
		return connect.NewError(connect.CodePermissionDenied,
			errors.New("loopconnect: loop is owned by a different subject"))
	}
	return nil
}

// StartLoop constructs and drives one loop from the request, returning its loop_id.
// A StartLoop seam error maps to CodeInternal (loop construction/drive failure is a
// server-side condition here). When authenticated, the principal's tenant is
// authoritative (a mismatching wire tenant is a spoof → CodePermissionDenied) and the
// principal's subject is recorded as the loop's Owner right after creation.
func (h *Handler) StartLoop(ctx context.Context, req *connect.Request[loopv1.StartLoopRequest]) (*connect.Response[loopv1.StartLoopResponse], error) {
	ctx, apiSpan := h.observer.Start(ctx, observe.Descriptor{Kind: observe.OperationAPIRequest, Name: "start_loop"})
	apiOutcome := observe.OutcomeSuccess
	defer func() { apiSpan.End(apiOutcome) }()
	m := req.Msg
	tenant, p, havePrincipal, aerr := h.effectiveTenant(ctx, m.Tenant)
	if aerr != nil {
		apiOutcome = observe.OutcomeError
		return nil, aerr
	}
	loopBase := h.baseCtx
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		loopBase = trace.ContextWithSpanContext(loopBase, sc)
	}
	// Launch under the LOOP-LIFETIME context, NOT the per-request ctx (which connect
	// cancels the moment this unary RPC returns — that would kill the loop before its
	// first turn). See Handler.baseCtx.
	handle, err := h.rt.StartLoop(loopBase, runtime.LoopConfig{
		Task:    m.Task,
		ModelID: m.Model,
		Tenant:  tenant,
		// Thread the authenticated initiator's subject (change #59) so the loop-server's
		// Deps.LoopContext hook stamps the REAL principal (tenant + subject) onto the run
		// context — the MCP egress then mints a per-call delegation token with this user
		// in `sub` and fuse in `act`, per tenant, never a seeded local default. When no
		// principal is present (un-intercepted/registry-less test paths) Subject is empty
		// and the loop-server hook falls back to the tenant-only local principal.
		Subject:     p.Subject,
		Interactive: m.Interactive,
	})
	if err != nil {
		apiOutcome = observe.OutcomeError
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	// Record ownership. The seam's StartLoop already Registered the record (with
	// OwnerNodeID + Live) under StreamKey{tenant, handle.ID()}; we Resolve-then-
	// Register so setting Owner preserves OwnerNodeID/Live/CreatedAt (Register is an
	// idempotent upsert that writes every field it is handed). This keeps the seam
	// auth-free: the runtime never learns the authorization subject.
	if h.registry != nil && havePrincipal {
		if aerr := h.recordOwner(h.baseCtx, tenant, handle.ID(), p.Subject); aerr != nil {
			apiOutcome = observe.OutcomeError
			return nil, aerr
		}
	}
	resp := connect.NewResponse(&loopv1.StartLoopResponse{LoopId: handle.ID()})
	if id := observeotel.TraceID(ctx); id != "" {
		resp.Header().Set("Fuse-Trace-Id", id)
	}
	return resp, nil
}

// recordOwner sets Owner = subject on the just-created loop's record via a Resolve-
// then-Register, preserving the OwnerNodeID/Live/CreatedAt the seam recorded. A
// missing record here is a genuine server-side inconsistency (the seam should have
// Registered it) → CodeInternal.
func (h *Handler) recordOwner(ctx context.Context, tenant event.TenantID, loopID, subject string) *connect.Error {
	key := event.StreamKey{Tenant: tenant, Loop: event.LoopID(loopID)}
	rec, err := h.registry.Resolve(ctx, key)
	if err != nil {
		return connect.NewError(connect.CodeInternal, err)
	}
	rec.Owner = subject
	if err := h.registry.Register(ctx, rec); err != nil {
		return connect.NewError(connect.CodeInternal, err)
	}
	return nil
}

// Send injects human input at the loop's next turn boundary. It maps the two client-
// addressable seam sentinels: an unknown loop → CodeNotFound, a finished loop →
// CodeFailedPrecondition (the loop exists but no longer accepts input). Any other
// error → CodeInternal.
func (h *Handler) Send(ctx context.Context, req *connect.Request[loopv1.SendRequest]) (*connect.Response[loopv1.SendResponse], error) {
	m := req.Msg
	tenant, p, havePrincipal, aerr := h.effectiveTenant(ctx, m.Tenant)
	if aerr != nil {
		return nil, aerr
	}
	if aerr := h.authorizeLoop(ctx, tenant, m.LoopId, p, havePrincipal); aerr != nil {
		return nil, aerr
	}
	if err := h.rt.Send(ctx, tenant, m.LoopId, m.Input); err != nil {
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
