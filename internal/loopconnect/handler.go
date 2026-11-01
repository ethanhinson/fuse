// Package loopconnect is the Connect/protobuf transport edge for the networked loop-
// control binding (change 0055, fuse.loop.v1) — successor to the JSON-over-WebSocket
// wire (#48, ADR-0032 superseded). It implements loopv1connect.LoopServiceHandler
// over a runtime.Runtime: StartLoop / Send / GetCapabilities (unary) and Observe
// (server-streaming history-then-live). It is a pure adapter - it imports internal/runtime,
// internal/event, the generated loopwire stubs, the internal/capabilities answer
// types, the internal/tenantlimit per-tenant loop limiter (change 0103), the
// internal/loopsecret in-memory secret values (change 0102) and the
// public harness types (for a
// refused spec's violations, change 0085), and NEVER a renderer, TUI,
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
	"fmt"
	"math"
	"slices"
	"strconv"
	"time"

	"connectrpc.com/connect"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/ethanhinson/fuse/harness"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/loopauth"
	"github.com/ethanhinson/fuse/internal/loopsecret"
	loopv1 "github.com/ethanhinson/fuse/internal/loopwire/v1"
	"github.com/ethanhinson/fuse/internal/loopwire/v1/loopv1connect"
	"github.com/ethanhinson/fuse/internal/observe"
	observeotel "github.com/ethanhinson/fuse/internal/observe/otel"
	"github.com/ethanhinson/fuse/internal/runtime"
	"github.com/ethanhinson/fuse/internal/tenantlimit"
)

// Handler adapts a runtime.Runtime to the generated LoopServiceHandler. Tenant is a
// pass-through, unenforced typed field on every RPC (identity is #0049): the tenant
// string maps to event.TenantID and is threaded to the seam call, never dropped
// (cache-over-tenant-scoped-source-reassert-key-on-hit).
//
// Each RPC requires one token scope when the authenticated principal carries a
// scope set (an empty set is unrestricted): StartLoop needs loop:start, Send needs
// loop:send, Observe needs loop:observe and GetCapabilities needs
// capabilities:read. An explicitly granted loop:admin also satisfies Send and
// Observe and lifts the owner check within the principal's tenant. The scope
// check is the first check each RPC runs, before the tenant check and before the
// runtime is touched.
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

	// caps answers GetCapabilities. Nil means the RPC answers Unimplemented; the
	// composition root injects it via WithCapabilities.
	caps CapabilitySource

	// limits is the per-tenant loop limiter (change 0103) StartLoop admits
	// against and GetCapabilities advertises from, so the advertisement and the
	// enforcement read one source. Nil means unlimited: the fast path, and
	// GetCapabilities reports zeros. The composition root injects it via
	// WithLimits.
	limits *tenantlimit.Limiter
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

// WithLimits injects the per-tenant loop limiter StartLoop enforces and
// GetCapabilities reports. A nil limiter keeps unlimited. Chainable.
func (h *Handler) WithLimits(l *tenantlimit.Limiter) *Handler {
	if l != nil {
		h.limits = l
	}
	return h
}

// requireScope refuses a request whose authenticated principal is not allowed
// scope. alsoSatisfiedBy names scopes that satisfy the requirement when
// EXPLICITLY granted (loop:admin for Send/Observe); an unrestricted principal
// already passes through Allows. No principal (an un-intercepted path) means no
// check, as for the owner rule. The refusal names the canonical scope.
func requireScope(ctx context.Context, scope string, alsoSatisfiedBy ...string) *connect.Error {
	p, ok := PrincipalFrom(ctx)
	if !ok || p.Allows(scope) {
		return nil
	}
	for _, s := range alsoSatisfiedBy {
		if p.Grants(s) {
			return nil
		}
	}
	return connect.NewError(connect.CodePermissionDenied,
		fmt.Errorf("loopconnect: token lacks scope %q", scope))
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

// authorizeLoop enforces per-loop ownership for Send/Observe (and a StartLoop
// orchestrate target). It runs only when a registry AND a principal are present. It
// resolves the (tenant, loop) record — ErrLoopUnknown collapses to CodeNotFound
// (cross-tenant misses land here because the resolve is tenant-scoped), and a record
// owned by a different subject is CodePermissionDenied. An owner-less record
// (Owner == "") imposes no constraint. A principal explicitly granted loop:admin
// skips ONLY the owner comparison: the tenant-scoped resolve still runs, so a loop in
// another tenant stays NotFound. An unrestricted principal is never treated as admin.
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
	if rec.Owner != "" && rec.Owner != p.Subject && !p.Grants(loopauth.ScopeLoopAdmin) {
		return connect.NewError(connect.CodePermissionDenied,
			errors.New("loopconnect: loop is owned by a different subject"))
	}
	return nil
}

// StartLoop constructs and drives one loop from the request, returning its loop_id.
// A StartLoop seam error maps to CodeInternal (loop construction/drive failure is a
// server-side condition here). When authenticated, the principal's tenant is
// authoritative (a mismatching wire tenant is a spoof → CodePermissionDenied) and the
// principal's subject is recorded as the loop's Owner right after creation. A scoped
// principal needs loop:start; loop:admin does not satisfy it (it only lifts the owner
// check on an orchestrate target).
//
// A request carrying loop-scoped secret values (change 0102) also needs
// secrets:inline (an unrestricted principal passes, as for every scope), and the
// secrets must be within loopsecret's wire limits (CodeInvalidArgument
// otherwise). Both checks run right after loop:start, before the tenant, pin,
// orchestrate and tenant-limit checks, so a refused request spends nothing. No
// refusal ever names a secret value. The values go to the runtime in
// LoopConfig.Secrets and nowhere else.
func (h *Handler) StartLoop(ctx context.Context, req *connect.Request[loopv1.StartLoopRequest]) (*connect.Response[loopv1.StartLoopResponse], error) {
	ctx, apiSpan := h.observer.Start(ctx, observe.Descriptor{Kind: observe.OperationAPIRequest, Name: "start_loop"})
	apiOutcome := observe.OutcomeSuccess
	defer func() { apiSpan.End(apiOutcome) }()
	if aerr := requireScope(ctx, loopauth.ScopeLoopStart); aerr != nil {
		apiOutcome = observeotel.OutcomeFromError(ctx, aerr)
		return nil, aerr
	}
	m := req.Msg
	// Loop-scoped secret values need their own scope; an unrestricted principal
	// passes as for every scope. The limits apply before anything else is spent,
	// and no refusal ever names a value.
	secrets := loopSecrets(m.GetSecrets())
	if len(secrets) > 0 {
		if aerr := requireScope(ctx, loopauth.ScopeSecretsInline); aerr != nil {
			apiOutcome = observeotel.OutcomeFromError(ctx, aerr)
			return nil, aerr
		}
		if err := loopsecret.CheckLimits(secrets); err != nil {
			cerr := connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("loopconnect: %w", err))
			apiOutcome = observeotel.OutcomeFromError(ctx, cerr)
			return nil, cerr
		}
	}
	tenant, p, havePrincipal, aerr := h.effectiveTenant(ctx, m.Tenant)
	if aerr != nil {
		apiOutcome = observeotel.OutcomeFromError(ctx, aerr)
		return nil, aerr
	}
	// A token pinned to one harness spec digest may start only that spec. The
	// compare is exact string equality against harness.Digest (no
	// normalization), so a malformed pin fails closed. It runs before the
	// runtime is touched, so a refused request registers nothing.
	if havePrincipal && p.HarnessDigest != "" {
		if m.Harness == nil {
			aerr := connect.NewError(connect.CodePermissionDenied,
				fmt.Errorf("loopconnect: token is pinned to harness %s; the request carries no harness", p.HarnessDigest))
			apiOutcome = observeotel.OutcomeFromError(ctx, aerr)
			return nil, aerr
		}
		if got := harness.Digest(m.Harness); got != p.HarnessDigest {
			aerr := connect.NewError(connect.CodePermissionDenied,
				fmt.Errorf("loopconnect: harness digest %s does not match the token's pin %s", got, p.HarnessDigest))
			apiOutcome = observeotel.OutcomeFromError(ctx, aerr)
			return nil, aerr
		}
	}
	// Orchestrating another loop is acting on it: the caller must be allowed to
	// drive that loop, by the same ownership rule Send and Observe apply.
	if m.OrchestrateLoopId != "" {
		if aerr := h.authorizeLoop(ctx, tenant, m.OrchestrateLoopId, p, havePrincipal); aerr != nil {
			apiOutcome = observeotel.OutcomeFromError(ctx, aerr)
			return nil, aerr
		}
	}
	// Per-tenant limits (change 0103) apply after authentication and the spoof
	// and orchestrate checks, so an unauthenticated or refused caller spends
	// nothing, and before the seam, so a refused start creates no loop. The
	// reservation is held until this handler returns, on every path, so a
	// failed start never leaks a slot.
	release, lerr := h.limits.Admit(ctx, tenant)
	if lerr != nil {
		cerr := limitError(lerr)
		apiOutcome = observeotel.OutcomeFromError(ctx, cerr)
		return nil, cerr
	}
	defer release()
	loopBase := h.baseCtx
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		loopBase = trace.ContextWithSpanContext(loopBase, sc)
	}
	// Launch under the LOOP-LIFETIME context, NOT the per-request ctx (which connect
	// cancels the moment this unary RPC returns — that would kill the loop before its
	// first turn). See Handler.baseCtx.
	// The principal record (loop.principal) is built only from the verified token's
	// identity fields; never the harness digest or anything else token-derived.
	var principalRec *event.LoopPrincipalPayload
	if havePrincipal {
		principalRec = &event.LoopPrincipalPayload{Subject: p.Subject, ClientID: p.ClientID,
			Issuer: p.Issuer, Scopes: p.Scopes.List(), TokenID: p.TokenID}
	}
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
		Orchestrate: m.OrchestrateLoopId,
		Harness:     m.Harness,
		Secrets:     secrets,
		Principal:   principalRec,
	})
	if err != nil {
		apiOutcome = observeotel.OutcomeFromError(ctx, err)
		if cerr := harnessError(err); cerr != nil {
			return nil, cerr
		}
		if errors.Is(err, runtime.ErrLoopNotFound) || errors.Is(err, runtime.ErrLoopFinished) {
			// The orchestrate target is not a loop the caller can steer: a client
			// error, not a server fault.
			return nil, connect.NewError(connect.CodeFailedPrecondition, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	// The loop is registered live now, so the in-flight reservation would count
	// it twice; drop it here (release is idempotent, the defer covers errors).
	release()
	// Record ownership. The seam's StartLoop already Registered the record (with
	// OwnerNodeID + Live) under StreamKey{tenant, handle.ID()}; we Resolve-then-
	// Register so setting Owner preserves OwnerNodeID/Live/CreatedAt (Register is an
	// idempotent upsert that writes every field it is handed). This keeps the seam
	// auth-free: the runtime never learns the authorization subject.
	if h.registry != nil && havePrincipal {
		if aerr := h.recordOwner(h.baseCtx, tenant, handle.ID(), p.Subject); aerr != nil {
			apiOutcome = observeotel.OutcomeFromError(ctx, aerr)
			return nil, aerr
		}
	}
	out := &loopv1.StartLoopResponse{LoopId: handle.ID()}
	if m.Harness != nil {
		out.HarnessDigest = harness.Digest(m.Harness)
	}
	resp := connect.NewResponse(out)
	if id := observeotel.TraceID(ctx); id != "" {
		resp.Header().Set("Fuse-Trace-Id", id)
	}
	return resp, nil
}

// loopSecrets converts the wire secrets to the runtime's in-memory form, nil for
// none. Hosts are copied so the result shares nothing with the request message.
func loopSecrets(in []*loopv1.LoopSecret) []loopsecret.Secret {
	if len(in) == 0 {
		return nil
	}
	out := make([]loopsecret.Secret, len(in))
	for i, s := range in {
		out[i] = loopsecret.Secret{Name: s.GetName(), Value: s.GetValue(), Hosts: slices.Clone(s.GetHosts())}
	}
	return out
}

// limitError maps an Admit failure: a limit refusal is ResourceExhausted
// naming the limit, with Retry-After (whole seconds, rounded up) for the rate
// axis; a caller that gave up while waiting for the tenant's admission lock gets
// its own context code; any other failure means the live loops could not be
// counted and is Unavailable (fail closed: the start does not proceed).
func limitError(err error) *connect.Error {
	var refused *tenantlimit.RefusedError
	if errors.As(err, &refused) {
		cerr := connect.NewError(connect.CodeResourceExhausted, err)
		if refused.RetryAfter > 0 {
			secs := max(int(math.Ceil(refused.RetryAfter.Seconds())), 1)
			cerr.Meta().Set("Retry-After", strconv.Itoa(secs))
		}
		return cerr
	}
	switch {
	case errors.Is(err, context.Canceled):
		return connect.NewError(connect.CodeCanceled, err)
	case errors.Is(err, context.DeadlineExceeded):
		return connect.NewError(connect.CodeDeadlineExceeded, err)
	}
	// A failed registry count is a store outage the client can retry, not a bug.
	return connect.NewError(connect.CodeUnavailable, fmt.Errorf("loopconnect: cannot count live loops: %w", err))
}

// harnessError maps a refused harness spec (change 0085) to its Connect error:
// InvalidArgument carrying every violation as one ValidateHarnessResponse
// detail - the message HarnessService.ValidateHarness returns, so a client
// renders both identically - or Unimplemented when this server's runtime does
// not run specs. Any other error returns nil.
func harnessError(err error) *connect.Error {
	var ve *harness.ValidationError
	if errors.As(err, &ve) {
		cerr := connect.NewError(connect.CodeInvalidArgument, ve)
		if detail, derr := connect.NewErrorDetail(ve.Proto()); derr == nil {
			cerr.AddDetail(detail)
		}
		return cerr
	}
	if errors.Is(err, runtime.ErrHarnessUnsupported) {
		return connect.NewError(connect.CodeUnimplemented, err)
	}
	return nil
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
// error → CodeInternal. A scoped principal needs loop:send, or an explicit
// loop:admin (which also lifts the owner check within the tenant).
func (h *Handler) Send(ctx context.Context, req *connect.Request[loopv1.SendRequest]) (*connect.Response[loopv1.SendResponse], error) {
	ctx, apiSpan := h.observer.Start(ctx, observe.Descriptor{Kind: observe.OperationAPIRequest, Name: "send"})
	apiOutcome := observe.OutcomeSuccess
	defer func() { apiSpan.End(apiOutcome) }()
	if aerr := requireScope(ctx, loopauth.ScopeLoopSend, loopauth.ScopeLoopAdmin); aerr != nil {
		apiOutcome = observeotel.OutcomeFromError(ctx, aerr)
		return nil, aerr
	}
	m := req.Msg
	tenant, p, havePrincipal, aerr := h.effectiveTenant(ctx, m.Tenant)
	if aerr != nil {
		apiOutcome = observeotel.OutcomeFromError(ctx, aerr)
		return nil, aerr
	}
	if aerr := h.authorizeLoop(ctx, tenant, m.LoopId, p, havePrincipal); aerr != nil {
		apiOutcome = observeotel.OutcomeFromError(ctx, aerr)
		return nil, aerr
	}
	if !runtime.ValidSource(m.Source) {
		aerr := connect.NewError(connect.CodeInvalidArgument, errors.New("loopconnect: unknown source "+strconv.Quote(m.Source)+" (want \"human\" or \"orchestrator\")"))
		apiOutcome = observeotel.OutcomeFromError(ctx, aerr)
		return nil, aerr
	}
	opts := runtime.SteerOpts{Interrupt: m.Interrupt, Source: m.Source}
	err := runtime.SendOrSteer(ctx, h.rt, tenant, m.LoopId, m.Input, opts)
	// A finished loop may be RESUMABLE (change 0054, D3): a persistent conversational
	// session whose park was reaped after a disconnect can be revived. Rather than
	// surfacing CodeFailedPrecondition, transparently resume it and retry the Send once,
	// so a client that simply reconnects and sends keeps chatting without a bespoke
	// resume call. Resume is tenant-scoped (ADR-0034); if it cannot revive the loop
	// (genuinely unknown, or live on another instance), the retry's error falls through
	// to the mapping below unchanged.
	//
	// Reviving a loop makes it live again, so the resume is admitted by the
	// per-tenant limits exactly as a StartLoop is (change 0103): a tenant at its
	// cap cannot exceed it by resuming. Only this resume path is admitted; a Send
	// to a loop that is still live never reaches here, so it is never refused and
	// takes no slot. The reservation is held until the handler returns.
	if errors.Is(err, runtime.ErrLoopFinished) {
		release, lerr := h.limits.Admit(ctx, tenant)
		if lerr != nil {
			cerr := limitError(lerr)
			apiOutcome = observeotel.OutcomeFromError(ctx, cerr)
			return nil, cerr
		}
		defer release()
		if _, rerr := h.rt.Resume(ctx, tenant, m.LoopId); rerr == nil {
			// Registered live again: drop the in-flight reservation (idempotent).
			release()
			err = runtime.SendOrSteer(ctx, h.rt, tenant, m.LoopId, m.Input, opts)
		}
	}
	if err != nil {
		apiOutcome = observeotel.OutcomeFromError(ctx, err)
		switch {
		case errors.Is(err, runtime.ErrLoopNotFound):
			return nil, connect.NewError(connect.CodeNotFound, err)
		case errors.Is(err, runtime.ErrLoopFinished):
			return nil, connect.NewError(connect.CodeFailedPrecondition, err)
		default:
			return nil, connect.NewError(connect.CodeInternal, err)
		}
	}
	resp := connect.NewResponse(&loopv1.SendResponse{})
	if id := observeotel.TraceID(ctx); id != "" {
		resp.Header().Set("Fuse-Trace-Id", id)
	}
	return resp, nil
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
