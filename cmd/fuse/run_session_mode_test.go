package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
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

// bashArgsJSON builds the bash tool's JSON args ({"command":"..."}) for a raw
// shell command, the way the model would emit them.
func bashArgsJSON(cmd string) string {
	b, _ := json.Marshal(struct {
		Command string `json:"command"`
	}{Command: cmd})
	return string(b)
}

// TestSessionModeFlipToAuto_NextGateAutoApprovesReadOnlyBash is the end-to-end
// regression guard for the reopening symptom (toggle to auto, still prompted).
// It drives the SAME seam the interactive shell uses each turn: the session
// source is flipped smart→auto (the effect of a Shift+Tab toggle), then a NEWLY
// built gate — exactly what ShellModel.startPrompt constructs on the next turn —
// must resolve a read-only bash call under auto WITHOUT consulting the human
// approval func. `git status` is wholly read-only: it is approved deterministically
// by the auto pipeline's safe-list layer (allSegmentsReadOnlySafe) BEFORE any
// classifier is consulted, so the assertion is hermetic (no network / no LLM
// classifier call) even though a classifier is wired here.
func TestSessionModeFlipToAuto_NextGateAutoApprovesReadOnlyBash(t *testing.T) {
	// Start the session at smart, as the shell does by default.
	sm := permissions.NewSessionMode(permissions.ModeSmart)
	// Gateway configured so a classifier IS constructible and wired — proving the
	// safe-list layer short-circuits ahead of it (no prompt, no classifier).
	cfg := gatewayConfig("smart")
	reg := testRegistry()
	toolReg := safeToolRegistry(t)
	var prompted bool

	// Flip the session source smart→auto (the effect of a Shift+Tab toggle). No
	// live SetMode is needed — the shell mutates only the shared SessionMode and
	// the NEXT turn's gate reads it at construction.
	sm.Set(permissions.ModeAuto)

	// Build the gate the way the next turn's startPrompt closure does.
	g := buildGate(cfg, toolReg, approveAllFunc(&prompted), reg, nil, sm)
	if got := g.Mode(); got != permissions.ModeAuto {
		t.Fatalf("after flip to auto, newly built gate Mode() = %v, want ModeAuto", got)
	}

	// Resolve a read-only bash call under auto. It must auto-approve deterministically
	// via the safe-list layer without ever consulting the human approval func.
	res := g.Execute(context.Background(), "bash", bashArgsJSON("git status"))
	if res.IsError {
		t.Fatalf("read-only bash under auto should auto-approve, got error: %s", res.Output)
	}
	if prompted {
		t.Fatal("read-only bash under auto must NOT consult the approval func (regression: toggle to auto still prompted)")
	}
}

// TestBuildGate_MidTurnFlipBitesSameGate is the wiring-seam regression guard for
// the mid-turn fix: a gate built via buildGate with a session source has its
// holder flipped smart→auto WITHOUT a rebuild, and the SAME gate must now
// auto-approve a read-only bash call and report Mode()==auto. This is the exact
// scenario a human hits reaching for Shift+Tab mid-run: the running turn's gate
// (and its children) must observe the switch live.
func TestBuildGate_MidTurnFlipBitesSameGate(t *testing.T) {
	sm := permissions.NewSessionMode(permissions.ModeSmart)
	cfg := gatewayConfig("smart")
	reg := testRegistry()
	toolReg := safeToolRegistry(t)
	var prompted bool

	g := buildGate(cfg, toolReg, approveAllFunc(&prompted), reg, nil, sm)
	if got := g.Mode(); got != permissions.ModeSmart {
		t.Fatalf("gate built at smart: Mode() = %v, want ModeSmart", got)
	}

	// Flip the holder mid-turn — NO rebuild. The same gate must now be auto.
	sm.Set(permissions.ModeAuto)
	if got := g.Mode(); got != permissions.ModeAuto {
		t.Fatalf("after mid-turn holder flip, SAME gate Mode() = %v, want ModeAuto", got)
	}
	res := g.Execute(context.Background(), "bash", bashArgsJSON("git status"))
	if res.IsError {
		t.Fatalf("after mid-turn flip to auto, read-only bash should auto-approve, got: %s", res.Output)
	}
	if prompted {
		t.Fatal("after mid-turn flip to auto, read-only bash must NOT prompt (mid-turn regression)")
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

// TestAutoModeOptionsWiresWorkspaceContext is the production-wiring proof for
// the #0069 D1b context line: autoModeOptions must hand the classifier the same
// writable geography the gate is scoped to. Without this, deleting the
// WithWorkspaceContext call in autoModeOptions leaves the whole suite green
// while the feature silently stops shipping — the classifier-side tests only
// prove the line renders once someone sets it.
//
// The configured write root is asserted specifically, not just the workspace
// root: gateWriteRoots(cfg) feeds the gate's allowedRoots(), so a configured
// permissions.auto.write_roots entry that the prompt never names is a geography
// the gate allows and the classifier has never heard of.
func TestAutoModeOptionsWiresWorkspaceContext(t *testing.T) {
	extra := t.TempDir()
	canonExtra, err := filepath.EvalSymlinks(extra)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", extra, err)
	}

	cfg := gatewayConfig("auto")
	cfg.Permissions.Auto.WriteRoots = []string{extra}
	reg := testRegistry()

	opts := autoModeOptions(cfg, reg, nil)
	if len(opts) == 0 {
		t.Fatal("gateway configured: expected auto-mode options")
	}
	g := permissions.New(config.PermissionsConfig{Mode: "auto"}, safeToolRegistry(t), permissions.AlwaysApprove, opts...)

	line := g.ClassifierWorkspaceContextLine()
	if line == "" {
		t.Fatal("autoModeOptions must give the classifier a workspace context line (WithWorkspaceContext is not wired)")
	}
	if root := workspaceRoot(); root != "" && !strings.Contains(line, root) {
		t.Errorf("context line must name the workspace root %q; got %q", root, line)
	}
	if s := sessionScratchDir(); s != "" && !strings.Contains(line, s) {
		t.Errorf("context line must name the session scratch dir %q; got %q", s, line)
	}
	if !strings.Contains(line, canonExtra) {
		t.Errorf("context line must name every configured write root (%q), matching the gate's allowedRoots(); got %q", canonExtra, line)
	}
}
