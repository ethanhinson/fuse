package main

import (
	"context"
	"io"
	"reflect"

	harnessv1 "github.com/ethanhinson/fuse/harness/v1"
	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/capabilities"
	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/harnessresolve"
	"github.com/ethanhinson/fuse/internal/loopconnect"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/runtime"
	"github.com/ethanhinson/fuse/internal/skills"
)

// ResolveHarness implements runtime.HarnessResolver for the loop server
// bindings (change 0085). One networked fuse server serves many tenants and
// clients, and each may start a loop from its own harness spec. The spec is
// validated and layered over the server's trusted config, which stays the
// floor (internal/harnessresolve): tighten-only fields may only tighten it,
// operator-only surfaces always come from it, skill names resolve against the
// skills installed here, and secret_refs resolve through the operator's
// loop_server.secrets allowlist for the loop's tenant (deny by default). Any
// problem is returned as a *harness.ValidationError before the loop exists.
//
// The returned factory is the server's own per-loop factory over the loop's
// resolved settings, so the root agent, subagents, workflows and pipeline
// pools are all built from the loop's config rather than the process config.
func (h *loopServerHost) ResolveHarness(ctx context.Context, tenant event.TenantID, spec *harnessv1.HarnessSpec) (runtime.LoopFactory, error) {
	res, err := h.resolver().Resolve(ctx, string(tenant), spec)
	if err != nil {
		return runtime.LoopFactory{}, err
	}
	return h.factory(h.settingsFor(res)), nil
}

// resolver is the one Resolver this server layers specs with: ResolveHarness
// enforces through it and Capabilities advertises from it.
func (h *loopServerHost) resolver() harnessresolve.Resolver {
	r := harnessresolve.Resolver{
		Base:    h.operator.cfg,
		Secrets: harnessresolve.ConfigSecrets(h.operator.cfg.LoopServer.Secrets),
	}
	if h.skills != nil {
		r.Skills = func(name string) bool {
			_, ok := h.skills.Lookup(name)
			return ok
		}
	}
	return r
}

// Capabilities implements loopconnect.CapabilitySource. Every part reads what
// StartLoop enforces against: the operator registry, the installed skill set
// ResolveHarness looks skill names up in, and resolver() for the ceilings,
// secret names and spec MCP auth kinds.
func (h *loopServerHost) Capabilities(ctx context.Context, tenant event.TenantID) (capabilities.Capabilities, error) {
	r := h.resolver()
	return capabilities.Capabilities{
		Models:           capabilities.Models(h.operator.reg, agent.DefaultContextWindow, h.operator.cfg.MaxTokens),
		Skills:           capabilities.Skills(h.skills),
		Ceilings:         r.Ceilings(),
		SecretRefs:       r.SecretRefs(ctx, string(tenant)),
		SpecMCPAuthKinds: harnessresolve.SpecMCPAuthKinds(),
		Server:           capabilities.ServerInfo(),
	}, nil
}

var _ loopconnect.CapabilitySource = (*loopServerHost)(nil)

// settingsFor derives one loop's construction settings from its resolved spec.
func (h *loopServerHost) settingsFor(res harnessresolve.Resolved) loopSettings {
	cfg := res.Config
	reg := registryFromConfig(cfg)

	// The loop's skills: exactly the ones the spec names, or every installed
	// skill when it names none. Both the skill tool's lookup and the system
	// prompt's skill listing use this set.
	skillSet := h.skills
	if len(res.Skills) > 0 {
		skillSet = skillSet.Only(res.Skills)
	}
	var lookup func(string) (skills.Skill, bool)
	if skillSet != nil {
		lookup = skillSet.Lookup
	}

	// The loop's MCP attach options follow its resolved MCP servers. The identity
	// posture was already logged once for the server's own config at startup, so
	// a per-loop resolution logs nothing.
	mcpOpts, _ := mcpAttach(cfg, io.Discard)

	return loopSettings{
		cfg:         cfg,
		reg:         reg,
		modelAlias:  reg.Default,
		toolReg:     defaultToolRegistry(h.sb, cfg.Research, lookup),
		systemBlock: rootSystemBlock(cfg, skillSet),
		rateGate:    loopRateGate(h.operator.cfg, h.operator.rateGate, cfg),
		mcpOpts:     mcpOpts,
	}
}

// loopRateGate is the rate gate for a loop whose resolved throughput is
// loopCfg's. The server's shared gate always applies, so every loop still
// counts against the server's budget. When the loop's own limits differ from
// the server's (a spec may only tighten them, or add per-provider limits), the
// loop also gets a bucket of its own and waits on both, so a spec can slow its
// own loop down but never speed it past the server.
func loopRateGate(server config.Config, serverGate model.RateGate, loopCfg config.Config) model.RateGate {
	if reflect.DeepEqual(server.Throughput, loopCfg.Throughput) {
		return serverGate
	}
	own := sessionRateGate(loopCfg)
	switch {
	case own == nil:
		return serverGate
	case serverGate == nil:
		return own
	}
	return chainedRateGate{own, serverGate}
}

// chainedRateGate waits on every gate in order and reports to all of them.
type chainedRateGate []model.RateGate

func (c chainedRateGate) Wait(ctx context.Context, provider string, estTokens int) error {
	for _, g := range c {
		if err := g.Wait(ctx, provider, estTokens); err != nil {
			return err
		}
	}
	return nil
}

func (c chainedRateGate) Report(provider string, estTokens, inTokens, outTokens int) {
	for _, g := range c {
		g.Report(provider, estTokens, inTokens, outTokens)
	}
}
