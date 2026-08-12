package fuse

import (
	"context"
	"net/http"

	"connectrpc.com/connect"

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

// NewRemote builds a Client that dials the loop service at baseURL. creds.Token is sent
// as "Authorization: Bearer <token>" on every request and creds.Tenant is set on every
// request message (a wrong token → ErrUnauthenticated; a tenant that mismatches the
// token's principal → ErrPermissionDenied).
func NewRemote(baseURL string, creds Credentials, opts ...Option) *Client {
	o := newOptions(opts...)
	var httpClient connect.HTTPClient = http.DefaultClient
	if o.httpClient != nil {
		httpClient = o.httpClient
	}
	client := loopv1connect.NewLoopServiceClient(httpClient, baseURL,
		connect.WithInterceptors(bearerInterceptor{token: creds.Token}))
	return &Client{backend: &remoteBackend{client: client, tenant: creds.Tenant}}
}

// bearerInterceptor sets "Authorization: Bearer <token>" on every unary and streaming
// request. An empty token sets no header (which the server's Verifier maps to
// ErrInvalidToken → Unauthenticated, so missing and invalid fail closed alike).
type bearerInterceptor struct{ token string }

func (b bearerInterceptor) set(h http.Header) {
	if b.token != "" {
		h.Set("Authorization", "Bearer "+b.token)
	}
}

func (b bearerInterceptor) setContext(ctx context.Context, h http.Header) {
	b.set(h)
	observeotel.Inject(ctx, h)
}

func (b bearerInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		b.setContext(ctx, req.Header())
		return next(ctx, req)
	}
}

func (b bearerInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		conn := next(ctx, spec)
		b.setContext(ctx, conn.RequestHeader())
		return conn
	}
}

func (b bearerInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next // client-side interceptor: no server leg.
}

func (b *remoteBackend) startLoop(ctx context.Context, cfg StartLoopConfig) (LoopID, error) {
	resp, err := b.client.StartLoop(ctx, connect.NewRequest(&loopv1.StartLoopRequest{
		Task:        cfg.Task,
		Model:       cfg.Model,
		Tenant:      b.tenant,
		Interactive: cfg.Interactive,
	}))
	if err != nil {
		return "", mapConnectError(err)
	}
	return resp.Msg.LoopId, nil
}

func (b *remoteBackend) send(ctx context.Context, id LoopID, input string) error {
	_, err := b.client.Send(ctx, connect.NewRequest(&loopv1.SendRequest{
		LoopId: id,
		Input:  input,
		Tenant: b.tenant,
	}))
	return mapConnectError(err)
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
	default:
		return err
	}
}
