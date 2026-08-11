package fuse

import (
	"context"
	"errors"

	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/runtime"
)

// localBackend drives an in-process runtime.Runtime directly (the "local" transport).
// Every call threads creds.Tenant to the seam so the Client acts within one isolation
// boundary. The token is unused locally (there is no wire to authenticate); it is part
// of Credentials only for surface parity with the remote backend.
type localBackend struct {
	rt     runtime.Runtime
	tenant event.TenantID
}

// NewLocal builds a Client over a pre-built runtime.Runtime. "Local" means the caller
// already has an in-process runtime (an app embedding fuse, or a test); the SDK gives
// it the parity surface. creds.Tenant is threaded to every seam call; creds.Token is
// ignored (no wire). opts are accepted for surface parity with NewRemote (none apply
// locally today).
func NewLocal(rt runtime.Runtime, creds Credentials, opts ...Option) *Client {
	_ = newOptions(opts...) // no local-specific options today; keep the seam symmetric.
	return &Client{backend: &localBackend{
		rt:     rt,
		tenant: event.TenantID(creds.Tenant),
	}}
}

func (b *localBackend) startLoop(ctx context.Context, cfg StartLoopConfig) (LoopID, error) {
	h, err := b.rt.StartLoop(ctx, runtime.LoopConfig{
		Task:        cfg.Task,
		ModelID:     cfg.Model,
		Tenant:      b.tenant,
		Interactive: cfg.Interactive,
	})
	if err != nil {
		return "", err
	}
	return h.ID(), nil
}

func (b *localBackend) send(ctx context.Context, id LoopID, input string) error {
	return b.rt.Send(ctx, b.tenant, id, input)
}

// observe implements the wire's history-then-live contract over the seam, porting
// internal/loopconnect.(*Handler).Observe's discipline VERBATIM in intent:
//
//  1. Subscribe to the live tail FIRST (before replay) so no event slips between
//     replay-end and live-start. An event appended in the subscribe→replay window is
//     delivered on BOTH paths; the watermark dedup drops the replay duplicate.
//  2. Read durable history since fromSeq (Attach), advancing the watermark `last`.
//  3. Replay history, then live-tail, dropping ev.Seq <= last (the overlap). Dropping
//     a duplicate must NOT advance `prev` (unused here — the local surface carries no
//     gap flag — but the watermark rule is identical).
//
// It returns a buffered client channel plus an idempotent unsubscribe. The pump
// goroutine exits when ctx is cancelled, unsubscribe is called, or the seam channel
// closes — releasing the seam subscription on every path (leak-free).
func (b *localBackend) observe(ctx context.Context, id LoopID, fromSeq Seq) (<-chan Event, func(), error) {
	// 1) Subscribe BEFORE replay.
	live, seamUnsub, err := b.rt.Observe(ctx, b.tenant, id)
	if err != nil {
		return nil, nil, mapSeamError(err)
	}

	// 2) Read durable history since fromSeq.
	hist, err := b.rt.Attach(ctx, b.tenant, id, event.Seq(fromSeq))
	if err != nil {
		seamUnsub()
		return nil, nil, mapSeamError(err)
	}
	last := event.Seq(fromSeq)
	if n := len(hist); n > 0 {
		last = hist[n-1].Seq
	}

	// A cancellable child context so unsubscribe stops the pump even if ctx outlives it.
	pumpCtx, cancel := context.WithCancel(ctx)
	out := make(chan Event, 32)

	go func() {
		defer close(out)
		defer seamUnsub()

		// 3a) Replay durable history. A cancel/consumer-gone ends cleanly.
		for _, ev := range hist {
			select {
			case out <- ev:
			case <-pumpCtx.Done():
				return
			}
		}

		// 3b) Live tail, dropping any event already covered by replay (the overlap).
		for {
			select {
			case <-pumpCtx.Done():
				return
			case ev, ok := <-live:
				if !ok {
					return // seam store closed: clean end.
				}
				if ev.Seq <= last {
					continue // already replayed: drop the duplicate (dedup-at-watermark).
				}
				select {
				case out <- ev:
				case <-pumpCtx.Done():
					return
				}
			}
		}
	}()

	// Idempotent unsubscribe: cancelling the pump ctx tears down the goroutine, which
	// releases the seam subscription and closes out.
	unsub := func() { cancel() }
	return out, unsub, nil
}

// mapSeamError maps a runtime seam sentinel to the SDK sentinel, so a local caller
// matches the same errors.Is targets a remote caller would.
func mapSeamError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, runtime.ErrLoopNotFound):
		return ErrLoopNotFound
	case errors.Is(err, runtime.ErrLoopFinished):
		return ErrLoopFinished
	default:
		return err
	}
}
