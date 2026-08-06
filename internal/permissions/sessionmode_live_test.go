package permissions

import (
	"context"
	"sync"
	"testing"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/model"
)

// TestGate_WithSessionMode_LiveRead proves a gate wired with a SessionMode holder
// reads the mode LIVE: flipping the holder after construction changes the SAME
// gate's Mode() with no rebuild. This is the core of the mid-turn fix.
func TestGate_WithSessionMode_LiveRead(t *testing.T) {
	sm := NewSessionMode(ModeSmart)
	approve := func(_ context.Context, _ ApprovalRequest) (bool, bool, error) {
		return true, false, nil
	}
	g := New(config.PermissionsConfig{Mode: "smart"}, newTestRegistry("read_file"), approve,
		WithSessionMode(sm))

	if got := g.Mode(); got != ModeSmart {
		t.Fatalf("gate wired with smart holder: Mode() = %v, want ModeSmart", got)
	}

	// Flip the holder — no gate rebuild.
	sm.Set(ModeAuto)
	if got := g.Mode(); got != ModeAuto {
		t.Fatalf("after holder flip to auto, same gate Mode() = %v, want ModeAuto (live read)", got)
	}
}

// TestGate_HolderlessReadsSnapshot proves a gate with no holder still resolves off
// its own snapshot field — one-shot / mcp-server / test posture unchanged.
func TestGate_HolderlessReadsSnapshot(t *testing.T) {
	approve := func(_ context.Context, _ ApprovalRequest) (bool, bool, error) {
		return true, false, nil
	}
	g := New(config.PermissionsConfig{Mode: "smart"}, newTestRegistry("read_file"), approve)
	if got := g.Mode(); got != ModeSmart {
		t.Fatalf("holderless gate Mode() = %v, want ModeSmart", got)
	}
	// SetMode still drives the holderless gate.
	g.SetMode(ModePromptAll)
	if got := g.Mode(); got != ModePromptAll {
		t.Fatalf("holderless gate after SetMode = %v, want ModePromptAll", got)
	}
}

// TestGate_WithSessionMode_MidTurnFlipAutoApproves is the end-to-end mid-turn
// symptom: a gate built at smart (would prompt for a bash call) has its holder
// flipped to auto; the SAME gate now auto-approves a read-only bash via the auto
// pipeline's safe-list layer without ever consulting the human approval func.
func TestGate_WithSessionMode_MidTurnFlipAutoApproves(t *testing.T) {
	sm := NewSessionMode(ModeSmart)
	stub := &stubCompleter{resp: model.CompletionResp{Content: `{"verdict":"allow","reason":"x"}`}}
	cls := newTestClassifier(t, stub)
	prompted := false
	approve := func(_ context.Context, _ ApprovalRequest) (bool, bool, error) {
		prompted = true
		return true, false, nil
	}
	g := New(autoCfg(config.AutoConfig{}, nil, nil), newTestRegistry("bash", "read_file"), approve,
		WithWorkspaceRoot(t.TempDir()), WithClassifier(cls), WithInteractive(true),
		WithSessionMode(sm))

	// In smart, a non-safe-list bash call prompts.
	g.Execute(context.Background(), "bash", bashArgs("rm -rf /tmp/whatever"))
	if !prompted {
		t.Fatal("smart mode should prompt for a non-safe-list bash call")
	}

	// Flip the holder to auto mid-turn — no rebuild. A read-only bash must now
	// auto-approve via the safe-list layer without consulting the human.
	prompted = false
	sm.Set(ModeAuto)
	res := g.Execute(context.Background(), "bash", bashArgs("git status"))
	if res.IsError {
		t.Fatalf("after holder flip to auto, read-only bash should auto-approve, got: %s", res.Output)
	}
	if prompted {
		t.Fatal("after holder flip to auto, read-only bash must NOT prompt (mid-turn regression)")
	}
}

// TestGate_WithSessionMode_ObservedTransition_ResetsValve proves the valve reset
// still fires when the auto→non-auto transition happens via the HOLDER (not
// SetMode): currentMode observes the transition and zeroes the valve.
func TestGate_WithSessionMode_ObservedTransition_ResetsValve(t *testing.T) {
	sm := NewSessionMode(ModeAuto)
	stub := &stubCompleter{resp: model.CompletionResp{Content: `{"verdict":"deny","reason":"x"}`}}
	cls := newTestClassifier(t, stub)
	approve := func(_ context.Context, _ ApprovalRequest) (bool, bool, error) {
		return true, false, nil
	}
	g := New(autoCfg(config.AutoConfig{}, nil, nil), newTestRegistry("bash"), approve,
		WithWorkspaceRoot(t.TempDir()), WithClassifier(cls), WithInteractive(true),
		WithSessionMode(sm))

	// Absorb 2 classifier blocks in auto so the valve counters are non-zero.
	g.Execute(context.Background(), "bash", bashArgs(grayAreaCmd(0)))
	g.Execute(context.Background(), "bash", bashArgs(grayAreaCmd(1)))
	if c, tot := g.valve.counts(); c == 0 && tot == 0 {
		t.Fatal("valve counts should be non-zero after 2 auto blocks")
	}

	// Flip the holder to smart. The next mode observation (any Execute) must see
	// the auto→non-auto transition and reset the valve.
	sm.Set(ModeSmart)
	g.Execute(context.Background(), "read_file", `{}`) // any resolve observes the mode
	if c, tot := g.valve.counts(); c != 0 || tot != 0 {
		t.Fatalf("observed auto→non-auto via holder must reset the valve, got consecutive=%d total=%d", c, tot)
	}
}

// TestGate_WithSessionMode_EnteringAuto_LeavesValve proves a non-auto→auto
// transition observed via the holder does NOT reset the valve (only leaving auto
// does), mirroring SetMode's semantics.
func TestGate_WithSessionMode_EnteringAuto_LeavesValve(t *testing.T) {
	sm := NewSessionMode(ModeAuto)
	stub := &stubCompleter{resp: model.CompletionResp{Content: `{"verdict":"deny","reason":"x"}`}}
	cls := newTestClassifier(t, stub)
	approve := func(_ context.Context, _ ApprovalRequest) (bool, bool, error) {
		return true, false, nil
	}
	g := New(autoCfg(config.AutoConfig{}, nil, nil), newTestRegistry("bash"), approve,
		WithWorkspaceRoot(t.TempDir()), WithClassifier(cls), WithInteractive(true),
		WithSessionMode(sm))

	g.Execute(context.Background(), "bash", bashArgs(grayAreaCmd(0)))
	c, tot := g.valve.counts()
	if c == 0 && tot == 0 {
		t.Fatal("expected non-zero valve counts after a block")
	}

	// Flip to smart then back to auto, observing each. Leaving auto resets; the
	// re-entry starts clean and must not itself reset again on a later read.
	sm.Set(ModeSmart)
	g.currentMode() // observe leaving-auto (resets)
	sm.Set(ModeAuto)
	g.currentMode() // observe entering-auto (must NOT reset — already 0 anyway)
	// Add a block in the fresh auto budget.
	g.Execute(context.Background(), "bash", bashArgs(grayAreaCmd(1)))
	if c2, _ := g.valve.counts(); c2 == 0 {
		t.Fatal("entering auto must leave the (fresh) budget accruing, not perpetually reset")
	}
}

// TestGate_WithSessionMode_ConcurrentFlipAndResolve runs a holder-flip / resolve
// goroutine pair so -race proves the live read path is race-free.
func TestGate_WithSessionMode_ConcurrentFlipAndResolve(t *testing.T) {
	sm := NewSessionMode(ModeSmart)
	approve := func(_ context.Context, _ ApprovalRequest) (bool, bool, error) {
		return true, false, nil
	}
	g := New(config.PermissionsConfig{Mode: "smart"}, newTestRegistry("read_file"), approve,
		WithSessionMode(sm))

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); sm.Set(ModeAuto) }()
		go func() {
			defer wg.Done()
			g.Execute(context.Background(), "read_file", `{}`)
		}()
	}
	wg.Wait()
}

// TestGate_CloneForChild_PropagatesHolder proves a child cloned from a
// holder-backed parent follows the SESSION mode live — flipping the holder after
// the clone changes the child's Mode() (supersedes D10's "children keep their
// spawn mode" for holder-backed sessions).
func TestGate_CloneForChild_PropagatesHolder(t *testing.T) {
	sm := NewSessionMode(ModeSmart)
	approve := func(_ context.Context, _ ApprovalRequest) (bool, bool, error) {
		return true, false, nil
	}
	parent := New(config.PermissionsConfig{Mode: "smart"}, newTestRegistry("read_file"), approve,
		WithSessionMode(sm))

	child := parent.CloneForChild("worker")
	if got := child.Mode(); got != ModeSmart {
		t.Fatalf("freshly cloned child Mode() = %v, want ModeSmart", got)
	}

	// Flip the holder AFTER the clone — the running child follows it live.
	sm.Set(ModeAuto)
	if got := child.Mode(); got != ModeAuto {
		t.Fatalf("after holder flip, holder-backed child Mode() = %v, want ModeAuto (follows session live)", got)
	}
}
