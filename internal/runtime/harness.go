package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/ethanhinson/fuse/harness"
	harnessv1 "github.com/ethanhinson/fuse/harness/v1"
	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/loopsecret"
	"github.com/ethanhinson/fuse/internal/tools"
)

// ErrHarnessUnsupported is returned by StartLoop when a LoopConfig carries a
// harness spec but the runtime was built without a HarnessResolver: the
// binding cannot run a client-supplied harness (change 0085).
var ErrHarnessUnsupported = errors.New("runtime: this runtime does not run harness specs")

// HarnessResolver resolves the harness spec a loop is started with into that
// loop's own construction (change 0085, ADR-0061). The runtime stays policy-free: it
// never interprets a spec, it only asks the binding's resolver for the factory
// that builds a loop from one. The composition root implements it by
// validating the spec and layering it over its trusted operator config.
//
// ResolveHarness is called before anything for the loop exists. An invalid spec
// must return an error (a *harness.ValidationError listing every violation, so
// the binding can report them); StartLoop then returns that error and creates
// no loop. It is called again, with the recorded spec, when a loop is resumed.
type HarnessResolver interface {
	ResolveHarness(ctx context.Context, tenant event.TenantID, spec *harnessv1.HarnessSpec) (LoopFactory, error)
}

// LoopFactory is the per-loop construction a HarnessResolver returns: the same
// collaborators Deps carries process-wide, built from the loop's resolved
// harness instead of the process config. A loop with a spec uses these in place
// of Deps.NewToolRegistry, Deps.BuildAgent and Deps.MaxConcurrent; Deps'
// LoopTeardown and LoopContext still apply to it.
type LoopFactory struct {
	// NewToolRegistry builds the loop's tool registry (nil: an empty one).
	NewToolRegistry func() *tools.Registry
	// BuildAgent builds the loop's root agent and child-builder, as
	// Deps.BuildAgent does.
	BuildAgent func(store event.EventStore, tree *agent.AgentTree, modelID string, toolReg *tools.Registry) (*agent.Agent, agent.ChildBuilder, string, error)
	// MaxConcurrent seeds the loop tree's scheduler concurrency.
	MaxConcurrent int
}

// resolveHarness returns the loop factory for cfg's harness spec, or nil when
// the loop has none (the Deps collaborators apply, exactly as before 0085).
// cfg.Secrets (change 0102) reach the resolver on its call's context only
// (loopsecret.WithSecrets); the wrapped context never leaves this call.
func (r *inProcRuntime) resolveHarness(ctx context.Context, cfg LoopConfig) (*LoopFactory, error) {
	if cfg.Harness == nil {
		return nil, nil
	}
	if r.deps.Harness == nil {
		return nil, ErrHarnessUnsupported
	}
	f, err := r.deps.Harness.ResolveHarness(loopsecret.WithSecrets(ctx, cfg.Secrets), event.NormalizeTenant(cfg.Tenant), cfg.Harness)
	if err != nil {
		return nil, fmt.Errorf("runtime: harness: %w", err)
	}
	if f.BuildAgent == nil {
		return nil, errors.New("runtime: harness: resolver returned no BuildAgent")
	}
	return &f, nil
}

// harnessEvent is the loop.harness record of spec: its deterministic encoding,
// the digest over it, and a protojson rendering for readers.
func harnessEvent(rootID string, spec *harnessv1.HarnessSpec) (event.Event, error) {
	js, err := protojson.Marshal(spec)
	if err != nil {
		return event.Event{}, fmt.Errorf("runtime: encode harness spec: %w", err)
	}
	b := harness.Encode(spec)
	payload, err := event.MarshalPayload(event.LoopHarnessPayload{Spec: js, SpecBytes: b, Digest: harness.DigestBytes(b)})
	if err != nil {
		return event.Event{}, fmt.Errorf("runtime: encode harness record: %w", err)
	}
	return event.Event{NodeID: rootID, Kind: event.KindLoopHarness, Payload: payload}, nil
}

// principalEvent is the loop.principal record of p: a copy of it with a nil
// Scopes normalized to an empty slice, so the recorded scopes are always a JSON
// array ([] meaning the credential was unrestricted).
func principalEvent(rootID string, p *event.LoopPrincipalPayload) (event.Event, error) {
	rec := *p
	if rec.Scopes == nil {
		rec.Scopes = []string{}
	}
	payload, err := event.MarshalPayload(rec)
	if err != nil {
		return event.Event{}, fmt.Errorf("runtime: encode principal record: %w", err)
	}
	return event.Event{NodeID: rootID, Kind: event.KindLoopPrincipal, Payload: payload}, nil
}

// harnessFromEvents recovers the harness spec a loop was started with from its
// durable stream: the recorded bytes, after checking the recorded digest over
// them. It returns nil for a loop started without one. A record that does not
// decode, or whose digest does not match, is an error: a resume must never
// rebuild a loop from a harness other than the one it was started with.
func harnessFromEvents(events []event.Event) (*harnessv1.HarnessSpec, error) {
	for _, e := range events {
		if e.Kind != event.KindLoopHarness {
			continue
		}
		var p event.LoopHarnessPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			return nil, fmt.Errorf("runtime: decode harness record: %w", err)
		}
		if got := harness.DigestBytes(p.SpecBytes); got != p.Digest {
			return nil, fmt.Errorf("runtime: harness record digest %q does not match its spec (%q)", p.Digest, got)
		}
		spec := &harnessv1.HarnessSpec{}
		if err := proto.Unmarshal(p.SpecBytes, spec); err != nil {
			return nil, fmt.Errorf("runtime: decode harness spec: %w", err)
		}
		return spec, nil
	}
	return nil, nil
}
