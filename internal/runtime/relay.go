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
// delivers each new request as it appears.
func (r *inProcRuntime) startRelayPump(ctx context.Context, key event.StreamKey, bus *agent.HumanBus, nodeID string, stop <-chan struct{}) {
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
		pass := func() {
			evs, err := ds.Replay(ctx, key, cursor)
			if err != nil {
				return // the next pass retries
			}
			if first {
				for _, e := range pendingRequests(evs) {
					deliver(e)
				}
				first = false
			} else {
				for _, e := range evs {
					if e.Kind == event.KindSteerRequest {
						deliver(e)
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
				if ev.Kind == event.KindSteerRequest {
					pass()
				}
			case <-tick.C:
				pass()
			}
		}
	}()
}
