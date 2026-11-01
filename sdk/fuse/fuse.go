// Package fuse is the public client SDK for the fuse loop runtime (change 0050). It
// presents ONE parity surface — StartLoop / Send / Observe — that a caller drives
// identically whether the loop runs in-process (NewLocal, over a runtime.Runtime) or
// across the network (NewRemote, over the fuse.loop.v1 Connect wire). It is a leaf
// importer: the SDK lives at the module root (github.com/ethanhinson/fuse/sdk/fuse)
// so external apps can import it, and it is NEVER imported by internal/*.
//
// The event types on the surface are aliases of internal/event's, so a caller reads
// Event/Seq/Kind without importing internal/. Completion is a first-class predicate
// (IsCompletion), never inferred from stream shape.
package fuse

import (
	"context"
	"errors"
	"net/http"
	"slices"

	harnessv1 "github.com/ethanhinson/fuse/harness/v1"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/loopsecret"
)

// connectHTTPClient is the minimal HTTP surface the remote backend dials with. It
// matches connect.HTTPClient (an *http.Client satisfies it), kept as a local interface
// so the WithHTTPClient option is expressible in the shared options type without Task 1
// depending on the connect package.
type connectHTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

// Event, Seq, and Kind are re-exported from internal/event as aliases so the SDK
// surface is stable and callers never import internal/. Payload stays json.RawMessage
// (raw JSON is the SDK ergonomic per the wire's proto comment).
type (
	Event = event.Event
	Seq   = event.Seq
	Kind  = event.Kind
)

// LoopID identifies one loop (the runtime's tree RootID). It is a string alias so a
// caller passes/stores it without a wrapper type.
type LoopID = string

// StartLoopConfig is the client-facing description of a loop to start. It mirrors the
// wire/seam inputs a client controls; tenant is not here — it is fixed per-Client via
// Credentials, so a Client acts within one isolation boundary.
type StartLoopConfig struct {
	Task        string // the initial user task
	Model       string // resolved gateway model id; "" ⇒ the server resolves the default
	Interactive bool   // persistent conversational mode (parks between turns)
	// Orchestrate, when set to another loop's id (same tenant), starts this loop as that
	// loop's orchestrator: a persistent chat loop that watches and steers the target.
	// Chat with it through Send and Observe like any interactive loop; Model picks the
	// orchestrator's own model. It implies Interactive.
	Orchestrate LoopID
	// Harness, when set, is the harness this loop runs (change 0085): the runtime
	// layers it over its own trusted config, which stays the floor. A spec it
	// refuses fails StartLoop with a *harness.ValidationError (match it with
	// errors.As) listing every violation with its field path; a runtime that does
	// not run specs fails with ErrHarnessUnsupported. Nil: the runtime's config.
	Harness *harnessv1.HarnessSpec
	// Secrets carries values for Harness's secret_ref names (change 0102). They
	// are loop-scoped and held in memory only: the runtime uses them to resolve
	// this loop's harness, never records them, and never logs them or puts them
	// in an error. An operator loop_server.secrets entry for the tenant takes
	// precedence; otherwise an entry resolves its name only for a destination
	// whose hostname is in its Hosts. Remotely this needs the secrets:inline
	// scope. At most 32 entries, each value at most 16 KiB. Ignored when Harness
	// is nil. A loop whose spec depends on loop-scoped secrets cannot be
	// resumed, because resume re-resolves the spec without them.
	Secrets []LoopSecret
}

// LoopSecret is one loop-scoped value for a harness secret_ref: Name is the
// secret_ref name, Value the credential, and Hosts the hostnames it may be sent
// to. It is never recorded, and every rendering of it (fmt, slog, JSON) prints
// the value as [REDACTED].
type LoopSecret = loopsecret.Secret

// WithSecrets returns a copy of c with s appended to its Secrets. The receiver
// is never mutated, and the copy never shares Secrets storage with it.
func (c StartLoopConfig) WithSecrets(s ...LoopSecret) StartLoopConfig {
	c.Secrets = append(slices.Clone(c.Secrets), s...)
	return c
}

// Credentials is the per-Client identity: the bearer token presented on every remote
// request and the tenant isolation boundary threaded on every call (remote wire field
// / local seam argument). The remote backend sends "Authorization: Bearer <Token>"
// and sets the tenant field; the local backend threads Tenant to the seam.
type Credentials struct {
	Token  string
	Tenant string
}

// Sentinel errors the SDK maps transport/seam failures to, so a caller matches a
// stable condition with errors.Is rather than decoding connect.Code / seam sentinels.
var (
	// ErrUnauthenticated is a missing or invalid bearer token (connect Unauthenticated).
	ErrUnauthenticated = errors.New("fuse: unauthenticated")
	// ErrPermissionDenied is a tenant spoof or a cross-owner access (connect PermissionDenied).
	ErrPermissionDenied = errors.New("fuse: permission denied")
	// ErrLoopNotFound is an unknown loop (connect NotFound / runtime.ErrLoopNotFound).
	ErrLoopNotFound = errors.New("fuse: loop not found")
	// ErrLoopFinished is a Send to a finished loop (connect FailedPrecondition /
	// runtime.ErrLoopFinished).
	ErrLoopFinished = errors.New("fuse: loop finished")
	// ErrHarnessUnsupported is a StartLoop carrying a harness spec on a runtime
	// that does not run specs: runtime.ErrHarnessUnsupported locally; remotely a
	// connect Unimplemented, or a server that predates specs and answered without
	// the harness digest (that loop was started on the server's own config). A
	// spec the runtime runs but refuses is a *harness.ValidationError instead.
	ErrHarnessUnsupported = errors.New("fuse: runtime does not run harness specs")
	// ErrCapabilitiesUnsupported is a Capabilities call on a runtime that does
	// not advertise: always from NewLocal (an in-process runtime has no
	// operator to advertise), and remotely on a connect Unimplemented.
	ErrCapabilitiesUnsupported = errors.New("fuse: runtime does not advertise capabilities")
)

// backend is the unexported seam the two constructors satisfy; Client delegates every
// public method to it. Keeping it unexported means the public surface exposes only the
// Client, never the local/remote split.
type backend interface {
	startLoop(ctx context.Context, cfg StartLoopConfig) (LoopID, error)
	send(ctx context.Context, id LoopID, input string, opts SendOptions) error
	observe(ctx context.Context, id LoopID, from Seq) (<-chan Event, func(), error)
	capabilities(ctx context.Context) (Capabilities, error)
}

// Client is the public SDK handle. It is transport-agnostic: construct it with NewLocal
// (in-process runtime) or NewRemote (Connect wire), then drive the identical methods.
type Client struct {
	backend backend
}

// StartLoop starts a loop from cfg and returns its LoopID.
func (c *Client) StartLoop(ctx context.Context, cfg StartLoopConfig) (LoopID, error) {
	return c.backend.startLoop(ctx, cfg)
}

// Send injects human input at the loop's next turn boundary.
func (c *Client) Send(ctx context.Context, id LoopID, input string) error {
	return c.backend.send(ctx, id, input, SendOptions{})
}

// SendOptions qualifies one message to a loop.
type SendOptions struct {
	// Interrupt cancels the loop's in-flight model call or tool batch so the message
	// lands now instead of at the next turn boundary.
	Interrupt bool
	// Source labels who is speaking: "" or SourceHuman (the default), or
	// SourceOrchestrator for guidance from an orchestrator. The agent sees the label.
	Source string
}

// Message sources for SendOptions.Source.
const (
	SourceHuman        = "human"
	SourceOrchestrator = "orchestrator"
)

// SendWith is Send with options: interrupt the loop, and/or label the message's source.
func (c *Client) SendWith(ctx context.Context, id LoopID, input string, opts SendOptions) error {
	return c.backend.send(ctx, id, input, opts)
}

// Interrupt is Send that does not wait: it cancels the loop's in-flight model call or
// tool batch so the message lands now. Use it to steer a loop that is mid-turn. A
// server whose runtime cannot interrupt delivers it at the next boundary like Send.
func (c *Client) Interrupt(ctx context.Context, id LoopID, input string) error {
	return c.backend.send(ctx, id, input, SendOptions{Interrupt: true})
}

// Observe returns a live event tail beginning at fromSeq plus an idempotent
// unsubscribe. Passing fromSeq = the last-seen Seq is the reconnect cursor: history
// since fromSeq is replayed then live events follow, with no loss and no duplication
// across the replay→live handoff (the wire contract, matched by both backends).
func (c *Client) Observe(ctx context.Context, id LoopID, fromSeq Seq) (<-chan Event, func(), error) {
	return c.backend.observe(ctx, id, fromSeq)
}

// IsCompletion reports whether e is the deterministic "this exchange is complete, send
// your next message" signal for a persistent (interactive) loop. It keys on the real
// completion kind, event.KindLoopParked (never inferred from stream shape). A one-shot
// run has no parked event; it ends at its terminal turn.end instead.
func IsCompletion(e Event) bool {
	return e.Kind == event.KindLoopParked
}

// Option configures a Client at construction (reserved for backend-specific tuning,
// e.g. a custom HTTP client on the remote backend). Kept as a functional-option seam so
// the constructors are extensible without a signature churn.
type Option func(*options)

type options struct {
	// httpClient overrides the connect.HTTPClient the remote backend dials with (set
	// via WithHTTPClient). nil ⇒ the backend's default (an h2c-capable http.Client).
	httpClient connectHTTPClient
	// tokenProvider supplies the bearer token per request (set via WithTokenProvider).
	// nil ⇒ the static Credentials.Token.
	tokenProvider func(context.Context) (string, error)
}

func newOptions(opts ...Option) options {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	return o
}
