package main

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/artifact"
	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/permissions"
	"github.com/ethanhinson/fuse/internal/probe"
)

// TestLoopServerDepsWireArtifacts: the strict builder both server bindings use
// hands the runtime a real artifact store, so a hosted loop's spill and segments
// do not silently fall back to the in-memory default.
func TestLoopServerDepsWireArtifacts(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := config.Default()
	reg := registryFromConfig(cfg)
	deps, err := buildLoopServerRuntimeDepsStrict(nil, cfg, reg, reg.Default, defaultToolRegistry(nil, cfg.Research, nil), "", permissions.Approver{}, nil, nil, nil)
	if err != nil {
		t.Fatalf("strict builder: %v", err)
	}
	if deps.Artifacts == nil {
		t.Fatal("strict loop-server deps left Artifacts nil")
	}
	lenient := buildLoopServerRuntimeDeps(nil, cfg, reg, reg.Default, defaultToolRegistry(nil, cfg.Research, nil), "", permissions.Approver{}, nil)
	if lenient.Artifacts == nil {
		t.Fatal("lenient loop-server deps left Artifacts nil with a working selector")
	}
}

// TestLoopServerStrictRefusesArtifactStoreError: a configured artifact store
// that cannot be opened refuses to start a server (strict), naming the remedy
// for its kind, while the lenient builder degrades to the runtime's default.
func TestLoopServerStrictRefusesArtifactStoreError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := config.Default()
	reg := registryFromConfig(cfg)

	for _, tc := range []struct {
		name    string
		err     error
		want    string
		notWant string
	}{
		{"dsn", errors.New("artifact pgstore: pool: connection refused"), "fix the DSN or wait for Postgres", ""},
		{"fs", fsBackendError{errors.New("filesystem artifact store: create: denied")}, "can be created and written", "Postgres"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := selectArtifactStoreFn
			selectArtifactStoreFn = func(config.Config) (artifact.Store, error) { return nil, tc.err }
			defer func() { selectArtifactStoreFn = old }()

			_, err := buildLoopServerRuntimeDepsStrict(nil, cfg, reg, reg.Default, defaultToolRegistry(nil, cfg.Research, nil), "", permissions.Approver{}, nil, nil, nil)
			if err == nil {
				t.Fatal("strict builder absorbed the artifact store error")
			}
			if !strings.Contains(err.Error(), "artifact store") || !strings.Contains(err.Error(), tc.want) ||
				!strings.Contains(err.Error(), tc.err.Error()) {
				t.Fatalf("err = %v, want artifact store refusal containing %q and the cause", err, tc.want)
			}
			if tc.notWant != "" && strings.Contains(err.Error(), tc.notWant) {
				t.Fatalf("err = %v must not mention %q", err, tc.notWant)
			}

			lenient := buildLoopServerRuntimeDeps(nil, cfg, reg, reg.Default, defaultToolRegistry(nil, cfg.Research, nil), "", permissions.Approver{}, nil)
			if lenient.Artifacts != nil {
				t.Fatalf("lenient builder Artifacts = %T, want nil (runtime MemStore default)", lenient.Artifacts)
			}
		})
	}
}

// TestLoopServerBuildAgentInstallsSegmentSink: the loop server's per-loop
// factory builds the loop's segment sink from that loop's own bound registry,
// so compaction archives under the loop's (tenant, loop) scope instead of
// discarding the region.
func TestLoopServerBuildAgentInstallsSegmentSink(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("LLM_GATEWAY_URL", "http://127.0.0.1:1")
	t.Setenv("LLM_GATEWAY_KEY", "tkn")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config load: %v", err)
	}
	cfg.Context.Summarization.Enabled = true
	reg := registryFromConfig(cfg)

	var bindings []artifact.Binding
	var sinks []agent.SegmentSink
	old := newLoopSegmentSink
	newLoopSegmentSink = func(b artifact.Binding) agent.SegmentSink {
		s := old(b)
		bindings = append(bindings, b)
		sinks = append(sinks, s)
		return s
	}
	defer func() { newLoopSegmentSink = old }()

	deps := buildLoopServerRuntimeDeps(nil, cfg, reg, reg.Default, defaultToolRegistry(nil, cfg.Research, nil), spawnAgentBlock, permissions.AlwaysApprove, nil)
	mem := artifact.NewMemStore()
	loopReg := deps.NewToolRegistry()
	loopReg.WithArtifacts(mem, "acme", "loopx")
	tree := agent.NewAgentTree("loopx", reg.Default)
	if _, _, _, err := deps.BuildAgent(nil, tree, reg.Default, loopReg); err != nil {
		t.Fatalf("BuildAgent: %v", err)
	}
	if deps.LoopTeardown != nil {
		deps.LoopTeardown(loopReg)
	}

	if len(bindings) != 1 {
		t.Fatalf("segment sink built %d times, want once per loop", len(bindings))
	}
	b := bindings[0]
	if b.Store != artifact.Store(mem) || b.Tenant != "acme" || b.Loop != "loopx" {
		t.Fatalf("sink binding = %+v, want the loop's own (acme, loopx) store", b)
	}
	if sinks[0] == nil {
		t.Fatal("loop-server sink is nil for a bound registry")
	}
	ptr, err := sinks[0].Archive(agent.SegmentRegion{TurnStart: 1, TurnEnd: 2, Summary: "s"})
	if err != nil || !strings.HasPrefix(ptr, "art://acme/loopx/segment/") {
		t.Fatalf("Archive = %q, %v; want an art://acme/loopx/segment/ pointer", ptr, err)
	}
}

// TestCLIBindingsWireArtifacts: one-shot and research-probe hand the runtime the
// filesystem artifact store, so their spilled output is recoverable under
// ~/.fuse/artifacts as it was under the retired spill directory, and the shell
// hands it the store its registry is already bound to.
func TestCLIBindingsWireArtifacts(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := config.Default()
	reg := registryFromConfig(cfg)

	tree := agent.NewAgentTreeWithConcurrency(reg.Default, reg.Default, cfg.Agents.MaxConcurrent)
	deps, closeMCP := buildOneShotRuntimeDeps(cfg, reg, reg.Default, defaultToolRegistry(nil, cfg.Research, nil), tree, io.Discard, false, nil,
		permissions.AlwaysApprove, "block", false, nil, nil)
	t.Cleanup(closeMCP)
	if deps.Artifacts == nil {
		t.Fatal("one-shot deps left Artifacts nil")
	}

	ptree := agent.NewAgentTreeWithConcurrency(reg.Default, reg.Default, cfg.Agents.MaxConcurrent)
	pdeps := buildResearchProbeRuntimeDeps(researchProbeDepsInput{
		cfg: cfg, reg: reg, alias: reg.Default, toolReg: defaultToolRegistry(nil, cfg.Research, nil), tree: ptree,
		logSink: probe.NewLog(),
	})
	if pdeps.Artifacts == nil {
		t.Fatal("research-probe deps left Artifacts nil")
	}

	// The shell binds its registry itself (to ~/.fuse/artifacts); its deps carry
	// that same store so a StartLoop never swaps it for the in-memory default.
	shellReg := defaultToolRegistry(nil, cfg.Research, nil)
	arts := artifact.NewMemStore()
	shellReg.WithArtifacts(arts, "", "shell-root")
	stree := agent.NewAgentTreeWithConcurrency(reg.Default, reg.Default, cfg.Agents.MaxConcurrent)
	sdeps := buildShellRuntimeDeps(shellDepsInput{
		cfg: cfg, reg: reg, alias: reg.Default, toolReg: shellReg, tree: stree,
		skillBlock: "block", childApprove: permissions.AlwaysApprove, rootApprove: permissions.AlwaysApprove,
	})
	if sdeps.Artifacts != artifact.Store(arts) {
		t.Fatalf("shell deps Artifacts = %T, want the session registry's store", sdeps.Artifacts)
	}
}
