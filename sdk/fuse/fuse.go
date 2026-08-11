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

	"github.com/ethanhinson/fuse/internal/event"
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
)

// backend is the unexported seam the two constructors satisfy; Client delegates every
// public method to it. Keeping it unexported means the public surface exposes only the
// Client, never the local/remote split.
type backend interface {
	startLoop(ctx context.Context, cfg StartLoopConfig) (LoopID, error)
	send(ctx context.Context, id LoopID, input string) error
	observe(ctx context.Context, id LoopID, from Seq) (<-chan Event, func(), error)
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
	return c.backend.send(ctx, id, input)
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
}

func newOptions(opts ...Option) options {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	return o
}
