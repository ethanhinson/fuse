package main

import (
	"context"
	"testing"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/permissions"
	"github.com/ethanhinson/fuse/internal/tools"
)

// approveAllFunc is a test ApprovalFunc that records whether it was consulted.
func approveAllFunc(prompted *bool) permissions.ApprovalFunc {
	return func(_ context.Context, _ permissions.ApprovalRequest) (bool, bool, error) {
		*prompted = true
		return true, false, nil
	}
}

// gatewayConfig returns a config whose gateway is configured, so a classifier is
// constructible.
func gatewayConfig(mode string) config.Config {
	return config.Config{
		Gateway:     config.Gateway{URL: "http://gateway.local", Key: "k"},
		Permissions: config.PermissionsConfig{Mode: mode},
	}
}

func testRegistry() *model.Registry {
	return model.DefaultRegistry()
}

// safeToolRegistry registers a single tool so the gate has an inner registry.
func safeToolRegistry(t *testing.T) *tools.Registry {
	t.Helper()
	r := tools.NewRegistry()
	for _, tl := range tools.DefaultTools() {
		r.Register(tl)
	}
	return r
}

// TestBuildGateReadsSessionModeAtConstruction proves a gate built from a session
// source is constructed at the session mode, not the raw cfg.Permissions.Mode:
// at `smart` a safe-list read_file auto-approves without prompting; after the
// source flips to `auto` a NEWLY built gate routes the same call through the
// auto pipeline (still auto-approves read_file as a safe tool, but the mode the
// gate was built at is `auto`, proven by a read_file being handled without the
// smart-path always_prompt semantics — we assert the constructed mode directly).
func TestBuildGateReadsSessionModeAtConstruction(t *testing.T) {
	sm := permissions.NewSessionMode(permissions.ModeSmart)
	cfg := gatewayConfig("smart")
	reg := testRegistry()
	toolReg := safeToolRegistry(t)
	var prompted bool

	g := buildGate(cfg, toolReg, approveAllFunc(&prompted), reg, nil, sm)
	if got := g.Mode(); got != permissions.ModeSmart {
		t.Fatalf("gate built from smart session source: Mode() = %v, want ModeSmart", got)
	}

	// Flip the session source to auto; a newly built gate must construct at auto.
	sm.Set(permissions.ModeAuto)
	g2 := buildGate(cfg, toolReg, approveAllFunc(&prompted), reg, nil, sm)
	if got := g2.Mode(); got != permissions.ModeAuto {
		t.Fatalf("after session flip to auto, newly built gate Mode() = %v, want ModeAuto", got)
	}
}

// TestBuildGateNilSessionModeDefaultsToConfig proves the one-shot / mcp posture:
// a nil session source falls back to cfg.Permissions.Mode exactly as before.
func TestBuildGateNilSessionModeDefaultsToConfig(t *testing.T) {
	cfg := gatewayConfig("prompt-all")
	reg := testRegistry()
	toolReg := safeToolRegistry(t)
	var prompted bool

	g := buildGate(cfg, toolReg, approveAllFunc(&prompted), reg, nil, nil)
	if got := g.Mode(); got != permissions.ModePromptAll {
		t.Fatalf("nil session source: Mode() = %v, want ModePromptAll (from cfg)", got)
	}
}

// TestAutoModeOptionsBuildsClassifierRegardlessOfMode proves D10 item 5: the
// classifier is wired whenever constructible, even when the configured mode is
// smart — so a later switch into auto is fully powered.
func TestAutoModeOptionsBuildsClassifierRegardlessOfMode(t *testing.T) {
	cfg := gatewayConfig("smart") // NOT auto
	reg := testRegistry()

	opts := autoModeOptions(cfg, reg, nil)
	if len(opts) == 0 {
		t.Fatal("classifier is constructible (gateway configured); expected auto-mode options even at mode=smart")
	}

	// Applying the options to a fresh auto-mode gate must yield a wired classifier
	// (a gray-area call reaches the classifier rather than the nil fail-closed ask
	// path). We assert construction wired a non-nil classifier via HasClassifier.
	g := permissions.New(config.PermissionsConfig{Mode: "auto"}, safeToolRegistry(t), permissions.AlwaysApprove, opts...)
	if !g.HasClassifier() {
		t.Fatal("expected a wired classifier from autoModeOptions at mode=smart")
	}
}

// TestAutoModeOptionsNoneWhenGatewayUnconfigured proves a classifier is NOT
// constructed when the gateway is entirely unconfigured — the gate stays nil
// (fail-closed asks), not an erroring stub.
func TestAutoModeOptionsNoneWhenGatewayUnconfigured(t *testing.T) {
	cfg := config.Config{Permissions: config.PermissionsConfig{Mode: "auto"}} // no gateway
	reg := testRegistry()

	opts := autoModeOptions(cfg, reg, nil)
	if opts != nil {
		t.Fatalf("gateway unconfigured: expected nil options, got %d", len(opts))
	}
}
