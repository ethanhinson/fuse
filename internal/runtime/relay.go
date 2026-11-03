package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/event"
)

// relay.go lets any replica deliver a message to a loop that another replica runs.
//
// A loop runs in exactly one process: the one that started it. When a Send or Steer
// reaches a different replica (a load balancer does not know which one owns the loop),
// that replica writes the message into the loop's durable stream as a steer.request
// event and returns. The owning replica watches its own loop's stream (relayPump) and
// queues each request on the loop's human bus, exactly as a local message. Because the
// request is stored before the caller is told it was accepted, a crash on either side
// loses nothing, and no replica needs the address of another.
//
// Delivery is recorded in the stream too: the user.input event for the turn that read a
// request lists it in ReqSeqs. A restarted owner (a resume) computes what is still
// undelivered from the stream alone, so a request is neither lost nor read twice.
//
// A Cancel reaches the owner the same way, as a loop.cancel.request: the pump hands it
// to onCancel, which cancels the loop as if the Cancel had been made here, and the
// loop's terminal loop.cancelled answers it. A request already in the stream when the
// pump starts is honoured on its first read. So is a loop.cancelled another replica
// wrote while this one was starting the loop (it sealed the loop while it read not
// live): a sealed loop must not keep running.

// relayedCancel is a cancel the relay pump found in its loop's stream: a
// loop.cancel.request (sealedSeq 0), or a loop.cancelled another replica already wrote
// (sealedSeq is its seq; requestSeq, reason and requestedBy are the ones it records).
type relayedCancel struct {
	requestSeq          event.Seq
	reason, requestedBy string
	sealedSeq           event.Seq
}

// errLiveElsewhere says a loop is running on another replica. Send and Steer answer it by
// relaying; anything else that needs the loop in THIS process treats it as not found.
var errLiveElsewhere = errors.New("runtime: loop is running on another replica")

// defaultRelayPoll is how often an owner re-reads its stream when no wake-up arrives. A
// live subscription wakes it at once; this bounds the delay when the subscription drops
// an event or the store has no cross-process notification.
const defaultRelayPoll = 2 * time.Second

func (r *inProcRuntime) relayPoll() time.Duration {
	if r.deps.RelayPoll > 0 {
		return r.deps.RelayPoll
	}
	return defaultRelayPoll
}

// relayRequest writes a steer.request for a loop that another replica runs.
func (r *inProcRuntime) relayRequest(ctx context.Context, tenant event.TenantID, loopID, input string, opts SteerOpts) error {
	if r.deps.DurableStore == nil {
		return fmt.Errorf("%w: %q", ErrLoopNotFound, loopID)
	}
	source := opts.Source
	if source == "" {
		source = agent.SourceHuman
	}
	payload, err := json.Marshal(event.SteerRequestPayload{Content: input, Source: source, Interrupt: opts.Interrupt})
	if err != nil {
		return err
	}
	key := event.StreamKey{Tenant: event.NormalizeTenant(tenant), Loop: event.LoopID(loopID)}
	return r.deps.DurableStore.Append(ctx, key, event.Event{NodeID: loopID, Kind: event.KindSteerRequest, Payload: payload})
}

// pendingRequests returns the steer.request events in evs that no user.input accounts
// for, in Seq order: what an owner that just started or resumed must still deliver.
func pendingRequests(evs []event.Event) []event.Event {
	delivered := map[event.Seq]bool{}
	for _, e := range evs {
		if e.Kind != event.KindUserInput {
			continue
		}
		var p event.UserInputPayload
		if json.Unmarshal(e.Payload, &p) == nil {
			for _, s := range p.ReqSeqs {
				delivered[s] = true
			}
		}
	}
	var out []event.Event
	for _, e := range evs {
		if e.Kind == event.KindSteerRequest && !delivered[e.Seq] {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out
}

// startRelayPump runs the owner's side of the relay until ctx is done or stop is closed.
// It first delivers whatever the stream says is still pending (a resumed loop), then
// delivers each new request as it appears. onCancel (may be nil) receives each cancel
// the stream carries; it must not block.
func (r *inProcRuntime) startRelayPump(ctx context.Context, key event.StreamKey, bus *agent.HumanBus, nodeID string, stop <-chan struct{}, onCancel func(relayedCancel)) {
	ds := r.deps.DurableStore
	go func() {
		wake, unsub, err := ds.Subscribe(ctx, key)
		if err != nil {
			wake = nil // the periodic read still works
		} else {
			defer unsub()
		}
		var cursor event.Seq
		first := true
		deliver := func(e event.Event) {
			var p event.SteerRequestPayload
			if json.Unmarshal(e.Payload, &p) != nil {
				return // a malformed request is skipped, never fatal
			}
			bus.Relay(nodeID, p.Source, p.Interrupt, p.Content, e.Seq)
		}
		cancelReq := func(e event.Event) {
			var p event.LoopCancelRequestPayload
			if onCancel == nil || json.Unmarshal(e.Payload, &p) != nil {
				return // a malformed request is skipped, never fatal
			}
			onCancel(relayedCancel{requestSeq: e.Seq, reason: p.Reason, requestedBy: p.RequestedBy})
		}
		sealed := func(e event.Event, p event.LoopCancelledPayload) {
			if onCancel != nil {
				onCancel(relayedCancel{requestSeq: event.Seq(p.RequestSeq), reason: p.Reason, requestedBy: p.RequestedBy, sealedSeq: e.Seq})
			}
		}
		pass := func() {
			evs, err := ds.Replay(ctx, key, cursor)
			if err != nil {
				return // the next pass retries
			}
			if first {
				for _, e := range pendingRequests(evs) {
					deliver(e)
				}
				if e, p, ok := firstCancelled(evs); ok {
					sealed(e, p)
				} else if e, _, ok := pendingCancel(evs); ok {
					cancelReq(e)
				}
				first = false
			} else {
				for _, e := range evs {
					switch e.Kind {
					case event.KindSteerRequest:
						deliver(e)
					case event.KindLoopCancelRequest:
						cancelReq(e)
					case event.KindLoopCancelled:
						if _, p, ok := firstCancelled([]event.Event{e}); ok {
							sealed(e, p)
						}
					}
				}
			}
			for _, e := range evs {
				if e.Seq > cursor {
					cursor = e.Seq
				}
			}
		}
		pass()
		tick := time.NewTicker(r.relayPoll())
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-stop:
				return
			case ev, ok := <-wake:
				if !ok {
					wake = nil
					continue
				}
				switch ev.Kind {
				case event.KindSteerRequest, event.KindLoopCancelRequest, event.KindLoopCancelled:
					pass()
				}
			case <-tick.C:
				pass()
			}
		}
	}()
}
