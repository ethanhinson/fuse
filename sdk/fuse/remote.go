package fuse

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"connectrpc.com/connect"

	"github.com/ethanhinson/fuse/harness"
	harnessv1 "github.com/ethanhinson/fuse/harness/v1"
	"github.com/ethanhinson/fuse/internal/event"
	loopv1 "github.com/ethanhinson/fuse/internal/loopwire/v1"
	"github.com/ethanhinson/fuse/internal/loopwire/v1/loopv1connect"
	observeotel "github.com/ethanhinson/fuse/internal/observe/otel"
)

// remoteBackend dials the fuse.loop.v1 Connect service over HTTP. It presents the
// bearer token on every request (via a client interceptor) and sets the tenant field
// on every request message, then maps connect.Code* back to the SDK sentinel errors so
// a caller matches the same conditions a local caller would.
type remoteBackend struct {
	client loopv1connect.LoopServiceClient
	tenant string
}

// WithHTTPClient overrides the HTTP client the remote backend dials with (e.g. an
// h2c-capable client for a cleartext HTTP/2 server, or a client with custom TLS). The
// default is http.DefaultClient, which negotiates HTTP/1.1 or HTTP/2 over TLS and works
// against the connect server's HTTP/1.1 fallback for unary + server-stream.
func WithHTTPClient(c connectHTTPClient) Option {
	return func(o *options) { o.httpClient = c }
}

// WithTokenProvider supplies the bearer token per request (each unary call and each
// stream open) instead of Credentials.Token, which is then ignored. It lets a client
// present short-lived issuer tokens. A provider error fails that request before
// anything is sent. There is no caching or refresh loop: cache inside the provider if
// needed. A provider that returns "" sends no Authorization header, as an empty Token
// does. It is an Option rather than a Credentials field so Credentials stays
// comparable. The local backend ignores it.
func WithTokenProvider(p func(context.Context) (string, error)) Option {
	return func(o *options) { o.tokenProvider = p }
}

// NewRemote builds a Client that dials the loop service at baseURL. creds.Token is sent
// as "Authorization: Bearer <token>" on every request and creds.Tenant is set on every
// request message (a wrong token → ErrUnauthenticated; a tenant that mismatches the
// token's principal → ErrPermissionDenied). When WithTokenProvider is given it supplies
// the token instead, once per request, and creds.Token is ignored.
func NewRemote(baseURL string, creds Credentials, opts ...Option) *Client {
	o := newOptions(opts...)
	var httpClient connect.HTTPClient = http.DefaultClient
	if o.httpClient != nil {
		httpClient = o.httpClient
	}
	client := loopv1connect.NewLoopServiceClient(httpClient, baseURL,
		connect.WithInterceptors(bearerInterceptor{token: creds.Token, provider: o.tokenProvider}))
	return &Client{backend: &remoteBackend{client: client, tenant: creds.Tenant}}
}

// bearerInterceptor sets "Authorization: Bearer <token>" on every unary and streaming
// request. An empty token sets no header (which the server's Verifier maps to
// ErrInvalidToken → Unauthenticated, so missing and invalid fail closed alike). With a
// provider, the token is fetched per request and a provider error fails that request
// before anything is sent.
type bearerInterceptor struct {
	token    string
	provider func(context.Context) (string, error)
}

// resolve returns the token for one request: the provider's answer when set,
// otherwise the static token.
func (b bearerInterceptor) resolve(ctx context.Context) (string, error) {
	if b.provider == nil {
		return b.token, nil
	}
	tok, err := b.provider(ctx)
	if err != nil {
		return "", fmt.Errorf("fuse: token provider: %w", err)
	}
	return tok, nil
}

func setRequestHeaders(ctx context.Context, h http.Header, token string) {
	if token != "" {
		h.Set("Authorization", "Bearer "+token)
	}
	observeotel.Inject(ctx, h)
}

func (b bearerInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		tok, err := b.resolve(ctx)
		if err != nil {
			return nil, err
		}
		setRequestHeaders(ctx, req.Header(), tok)
		return next(ctx, req)
	}
}

func (b bearerInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		tok, err := b.resolve(ctx)
		if err != nil {
			// The inner conn is never created: in connect-go even closing an
			// unsent conn issues a real (header-less) HTTP request, which could
			// block and would reach the server without credentials.
			return newFailedStreamConn(spec, err)
		}
		conn := next(ctx, spec)
		setRequestHeaders(ctx, conn.RequestHeader(), tok)
		return conn
	}
}

// failedStreamConn is a stream whose bearer token could not be obtained: every
// send, receive and close returns err, so the stream open fails without a request
// ever being made. It stands alone (no inner conn); Spec answers from the call and
// the header maps are empty but writable, as callers merge request headers in.
type failedStreamConn struct {
	spec       connect.Spec
	err        error
	reqHeader  http.Header
	respHeader http.Header
	trailer    http.Header
}

func newFailedStreamConn(spec connect.Spec, err error) failedStreamConn {
	return failedStreamConn{spec: spec, err: err, reqHeader: http.Header{}, respHeader: http.Header{}, trailer: http.Header{}}
}

func (f failedStreamConn) Spec() connect.Spec           { return f.spec }
func (f failedStreamConn) Peer() connect.Peer           { return connect.Peer{} }
func (f failedStreamConn) Send(any) error               { return f.err }
func (f failedStreamConn) RequestHeader() http.Header   { return f.reqHeader }
func (f failedStreamConn) CloseRequest() error          { return f.err }
func (f failedStreamConn) Receive(any) error            { return f.err }
func (f failedStreamConn) ResponseHeader() http.Header  { return f.respHeader }
func (f failedStreamConn) ResponseTrailer() http.Header { return f.trailer }
func (f failedStreamConn) CloseResponse() error         { return f.err }

func (b bearerInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next // client-side interceptor: no server leg.
}

func (b *remoteBackend) startLoop(ctx context.Context, cfg StartLoopConfig) (LoopID, error) {
	resp, err := b.client.StartLoop(ctx, connect.NewRequest(&loopv1.StartLoopRequest{
		Task:              cfg.Task,
		Model:             cfg.Model,
		Tenant:            b.tenant,
		Interactive:       cfg.Interactive,
		OrchestrateLoopId: cfg.Orchestrate,
		Harness:           cfg.Harness,
		Secrets:           toWireSecrets(cfg.Secrets),
	}))
	if err != nil {
		// Unimplemented means "harness specs unsupported" only for a StartLoop that
		// carried one; anywhere else it is a routing problem the caller should see.
		if cfg.Harness != nil && connect.CodeOf(err) == connect.CodeUnimplemented {
			return "", ErrHarnessUnsupported
		}
		return "", mapConnectError(err)
	}
	// A server that predates harness specs drops the field and runs the loop on its
	// own config; only a server that ran the spec acknowledges it with its digest.
	if cfg.Harness != nil && resp.Msg.HarnessDigest == "" {
		return "", fmt.Errorf("%w: loop %s was started without it (the server predates harness specs)", ErrHarnessUnsupported, resp.Msg.LoopId)
	}
	return resp.Msg.LoopId, nil
}

func (b *remoteBackend) send(ctx context.Context, id LoopID, input string, opts SendOptions) error {
	_, err := b.client.Send(ctx, connect.NewRequest(&loopv1.SendRequest{
		LoopId:    id,
		Input:     input,
		Tenant:    b.tenant,
		Interrupt: opts.Interrupt,
		Source:    opts.Source,
	}))
	return mapConnectError(err)
}

// capabilities calls GetCapabilities for this Client's tenant. Unimplemented
// means the server does not advertise; any other failure maps like every call.
func (b *remoteBackend) capabilities(ctx context.Context) (Capabilities, error) {
	resp, err := b.client.GetCapabilities(ctx, connect.NewRequest(&loopv1.GetCapabilitiesRequest{Tenant: b.tenant}))
	if err != nil {
		if connect.CodeOf(err) == connect.CodeUnimplemented {
			return Capabilities{}, ErrCapabilitiesUnsupported
		}
		return Capabilities{}, mapConnectError(err)
	}
	return fromProtoCapabilities(resp.Msg), nil
}

// fromProtoCapabilities maps the wire answer to the public types, copying
// every slice so the result never aliases the response message.
func fromProtoCapabilities(m *loopv1.GetCapabilitiesResponse) Capabilities {
	out := Capabilities{
		SecretRefs:       append([]string(nil), m.GetSecretRefs()...),
		SpecMCPAuthKinds: append([]harnessv1.McpAuthType(nil), m.GetSpecMcpAuthKinds()...),
		SpecPolicy: SpecPolicy{
			MCPHosts:            append([]string(nil), m.GetSpecPolicy().GetMcpHosts()...),
			DenyPrivateNetworks: m.GetSpecPolicy().GetDenyPrivateNetworks(),
			ModelsRegistryOnly:  m.GetSpecPolicy().GetModelsRegistryOnly(),
		},
		Server: ServerInfo{
			Version:         m.GetServer().GetVersion(),
			DigestAlgorithm: m.GetServer().GetDigestAlgorithm(),
		},
		Limits: Limits{
			MaxLiveLoops:    int(m.GetLimits().GetMaxLiveLoops()),
			StartsPerMinute: int(m.GetLimits().GetStartsPerMinute()),
		},
	}
	for _, pm := range m.GetModels() {
		out.Models = append(out.Models, ModelInfo{
			Alias:                  pm.GetAlias(),
			GatewayID:              pm.GetGatewayId(),
			MaxOutputTokens:        int(pm.GetMaxOutputTokens()),
			ContextWindow:          int(pm.GetContextWindow()),
			ContextWindowIsDefault: pm.GetContextWindowIsDefault(),
			Persona:                pm.GetPersona(),
			IsDefault:              pm.GetIsDefault(),
			Label:                  pm.GetLabel(),
			Description:            pm.GetDescription(),
			Tags:                   append([]string(nil), pm.GetTags()...),
		})
	}
	for _, s := range m.GetSkills() {
		out.Skills = append(out.Skills, SkillInfo{Name: s.GetName(), Description: s.GetDescription()})
	}
	for _, c := range m.GetCeilings() {
		out.Ceilings = append(out.Ceilings, Ceiling{
			FieldPath:   c.GetFieldPath(),
			OperatorSet: c.GetOperatorSet(),
			Value:       c.GetValue().GetNumberValue(),
			Direction:   fromProtoDirection(c.GetDirection()),
		})
	}
	return out
}

func fromProtoDirection(d loopv1.CeilingDirection) CeilingDirection {
	switch d {
	case loopv1.CeilingDirection_CEILING_DIRECTION_MAX:
		return CeilingMax
	case loopv1.CeilingDirection_CEILING_DIRECTION_MIN:
		return CeilingMin
	default:
		return 0
	}
}

// observe opens the server-stream Observe(fromSeq) and pumps its frames onto a client
// channel: it SKIPS keepalive frames (they carry no Seq), tracks Seq, and maps each
// proto Event to an sdk Event. The server already performs subscribe-before-replay +
// dedup-at-watermark, so no-loss/no-dup is delivered end-to-end; the client tracks the
// last Seq as a defensive watermark (a duplicate at or below it is dropped) so the
// exactly-once property holds from the client regardless of server framing.
//
// The pump exits when ctx is cancelled, unsubscribe is called, the stream ends, or a
// receive error occurs (a stream end is a clean reconnect signal, not a fault — the
// caller re-opens Observe(lastSeq)). The returned unsubscribe cancels the stream ctx.
func (b *remoteBackend) observe(ctx context.Context, id LoopID, fromSeq Seq) (<-chan Event, func(), error) {
	streamCtx, cancel := context.WithCancel(ctx)
	stream, err := b.client.Observe(streamCtx, connect.NewRequest(&loopv1.ObserveRequest{
		LoopId:  id,
		FromSeq: uint64(fromSeq),
		Tenant:  b.tenant,
	}))
	if err != nil {
		cancel()
		return nil, nil, mapConnectError(err)
	}

	out := make(chan Event, 32)
	go func() {
		defer close(out)
		defer stream.Close()

		last := fromSeq
		for stream.Receive() {
			msg := stream.Msg()
			if msg.Keepalive || msg.Event == nil {
				continue // idle heartbeat: no Seq, ignore for dedup.
			}
			ev := fromProtoEvent(msg.Event)
			if ev.Seq <= last {
				continue // defensive client-side dedup-at-watermark.
			}
			last = ev.Seq
			select {
			case out <- ev:
			case <-streamCtx.Done():
				return
			}
		}
		// stream.Receive() returned false: EOF or error. Either way it is a clean end
		// (the caller reconnects via Observe(lastSeq)); we do not surface it on the chan.
	}()

	return out, func() { cancel() }, nil
}

// fromProtoEvent maps a proto loopv1.Event to a domain event.Event. It mirrors
// loopconnect.fromProtoEvent (unexported there); the mapping is copied here so the SDK
// does not depend on loopconnect internals. A nil ts yields the zero time; payload is
// copied so the caller owns it.
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

// mapConnectError maps a connect error's code to the SDK sentinel, so a remote caller
// matches the same errors.Is targets a local caller would. A non-connect error (or nil)
// passes through unchanged.
func mapConnectError(err error) error {
	if err == nil {
		return nil
	}
	switch connect.CodeOf(err) {
	case connect.CodeUnauthenticated:
		return ErrUnauthenticated
	case connect.CodePermissionDenied:
		return ErrPermissionDenied
	case connect.CodeNotFound:
		return ErrLoopNotFound
	case connect.CodeFailedPrecondition:
		return ErrLoopFinished
	case connect.CodeInvalidArgument:
		// A refused harness spec (change 0085) carries its violations as one
		// ValidateHarnessResponse detail; surface them as the same
		// *harness.ValidationError the local backend returns.
		var cerr *connect.Error
		if errors.As(err, &cerr) {
			for _, d := range cerr.Details() {
				if m, derr := d.Value(); derr == nil {
					if resp, ok := m.(*harnessv1.ValidateHarnessResponse); ok {
						return harness.ValidationErrorFromProto(resp)
					}
				}
			}
		}
		return err
	default:
		return err
	}
}

// toWireSecrets converts loop-scoped secrets to their wire form; nil for none,
// so a request without secrets never carries the field.
func toWireSecrets(ss []LoopSecret) []*loopv1.LoopSecret {
	if len(ss) == 0 {
		return nil
	}
	out := make([]*loopv1.LoopSecret, len(ss))
	for i, s := range ss {
		out[i] = &loopv1.LoopSecret{Name: s.Name, Value: s.Value, Hosts: slices.Clone(s.Hosts)}
	}
	return out
}
