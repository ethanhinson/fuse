package permissions

import (
	"context"
	"sync"
	"testing"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/model"
)

// TestPermissionMode_String pins the canonical token for each mode — the single
// source of the indicator / /mode names, so no string literals are duplicated.
func TestPermissionMode_String(t *testing.T) {
	cases := []struct {
		mode PermissionMode
		want string
	}{
		{ModeSmart, "smart"},
		{ModeAuto, "auto"},
		{ModePromptAll, "prompt-all"},
		{ModeOff, "off"},
	}
	for _, tc := range cases {
		if got := tc.mode.String(); got != tc.want {
			t.Errorf("%d.String() = %q, want %q", tc.mode, got, tc.want)
		}
	}
}

// TestSessionMode_GetSetRoundTrip proves the holder returns its seeded value and
// picks up a Set.
func TestSessionMode_GetSetRoundTrip(t *testing.T) {
	sm := NewSessionMode(ModeSmart)
	if got := sm.Get(); got != ModeSmart {
		t.Fatalf("seeded Get() = %v, want ModeSmart", got)
	}
	sm.Set(ModeAuto)
	if got := sm.Get(); got != ModeAuto {
		t.Fatalf("after Set(ModeAuto), Get() = %v, want ModeAuto", got)
	}
}

// TestSessionMode_ConcurrentGetSet exercises concurrent Get/Set so the -race
// detector proves both sides are guarded by the same mutex.
func TestSessionMode_ConcurrentGetSet(t *testing.T) {
	sm := NewSessionMode(ModeSmart)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); sm.Set(ModeAuto) }()
		go func() { defer wg.Done(); _ = sm.Get() }()
	}
	wg.Wait()
}

// TestGate_SetMode_ObservedByResolve proves the root gate switches immediately:
// a gate built in prompt-all mode (which always asks) auto-approves a safe-list
// tool after SetMode(ModeSmart).
func TestGate_SetMode_ObservedByResolve(t *testing.T) {
	prompted := false
	approve := func(_ context.Context, _ ApprovalRequest) (bool, bool, error) {
		prompted = true
		return true, false, nil
	}
	cfg := config.PermissionsConfig{Mode: "prompt-all"}
	g := New(cfg, newTestRegistry("read_file"), approve)

	// In prompt-all the safe-list tool prompts.
	g.Execute(context.Background(), "read_file", `{}`)
	if !prompted {
		t.Fatal("prompt-all should prompt for read_file")
	}

	// Switch to smart: the safe-list tool must now auto-approve without prompting.
	prompted = false
	g.SetMode(ModeSmart)
	res := g.Execute(context.Background(), "read_file", `{}`)
	if res.IsError {
		t.Fatalf("after SetMode(smart), read_file should auto-approve, got: %s", res.Output)
	}
	if prompted {
		t.Fatal("after SetMode(smart), safe-list read_file must not prompt")
	}
}

// TestGate_CloneForChild_SnapshotsModeBeforeSetMode proves a child cloned BEFORE
// a parent SetMode keeps resolving under the old mode — a running child is not
// disturbed by a later parent switch.
func TestGate_CloneForChild_SnapshotsModeBeforeSetMode(t *testing.T) {
	approve := func(_ context.Context, _ ApprovalRequest) (bool, bool, error) {
		return true, false, nil
	}
	cfg := config.PermissionsConfig{Mode: "off"}
	parent := New(cfg, newTestRegistry("read_file"), approve)

	child := parent.CloneForChild("worker")

	// Parent switches to prompt-all; the already-spawned child must stay in off.
	parent.SetMode(ModePromptAll)

	if got := child.currentMode(); got != ModeOff {
		t.Fatalf("child spawned before SetMode should keep ModeOff, got %v", got)
	}
	if got := parent.currentMode(); got != ModePromptAll {
		t.Fatalf("parent should have switched to ModePromptAll, got %v", got)
	}

	// A child cloned AFTER the switch snapshots the new mode.
	child2 := parent.CloneForChild("worker2")
	if got := child2.currentMode(); got != ModePromptAll {
		t.Fatalf("child spawned after SetMode should snapshot ModePromptAll, got %v", got)
	}
}

// TestGate_SetMode_LeavingAuto_ResetsValve_ButCacheSurvives proves that leaving
// auto zeroes the valve counters, while a session-cache grant made before the
// switch still auto-approves after.
func TestGate_SetMode_LeavingAuto_ResetsValve_ButCacheSurvives(t *testing.T) {
	stub := &stubCompleter{resp: model.CompletionResp{Content: `{"verdict":"deny","reason":"x"}`}}
	cls := newTestClassifier(t, stub)
	approve := func(_ context.Context, _ ApprovalRequest) (bool, bool, error) {
		return true, true, nil // allow for session
	}
	g := New(autoCfg(config.AutoConfig{}, nil, nil), newTestRegistry("bash", "read_file"), approve,
		WithWorkspaceRoot(t.TempDir()), WithClassifier(cls), WithInteractive(true))

	// Seed a session-cache grant via the human path in smart-like flow: use a
	// gray-area command approved for session. First switch to smart so the human
	// path records the grant.
	g.SetMode(ModeSmart)
	grantArgs := bashArgs("touch /etc/escapes-workspace-grant")
	if res := g.Execute(context.Background(), "bash", grantArgs); res.IsError {
		t.Fatalf("granting session approval should succeed: %s", res.Output)
	}
	g.SetMode(ModeAuto)

	// Absorb 2 classifier blocks in auto so the valve counters are non-zero.
	g.Execute(context.Background(), "bash", bashArgs(grayAreaCmd(0)))
	g.Execute(context.Background(), "bash", bashArgs(grayAreaCmd(1)))
	if c, tot := g.valve.counts(); c == 0 && tot == 0 {
		t.Fatal("valve counts should be non-zero after 2 auto blocks")
	}

	// Leave auto: the valve resets to 0,0.
	g.SetMode(ModeSmart)
	if c, tot := g.valve.counts(); c != 0 || tot != 0 {
		t.Fatalf("leaving auto must reset the valve, got consecutive=%d total=%d", c, tot)
	}

	// The session-cache grant made before the switch still auto-approves. Reset
	// the approve func to a prompt-detector to prove no human prompt fires.
	prompted := false
	g.approve = func(_ context.Context, _ ApprovalRequest) (bool, bool, error) {
		prompted = true
		return false, false, nil
	}
	if res := g.Execute(context.Background(), "bash", grantArgs); res.IsError {
		t.Fatalf("session-cache grant should survive the mode switch: %s", res.Output)
	}
	if prompted {
		t.Fatal("cached grant must auto-approve without prompting after the switch")
	}
}

// TestGate_SetMode_EnteringAuto_LeavesValveCounters proves entering auto does not
// zero counters (only leaving auto does).
func TestGate_SetMode_EnteringAuto_LeavesValveCounters(t *testing.T) {
	stub := &stubCompleter{resp: model.CompletionResp{Content: `{"verdict":"deny","reason":"x"}`}}
	cls := newTestClassifier(t, stub)
	approve, _ := newApproveRecorder(true)
	g := New(autoCfg(config.AutoConfig{}, nil, nil), newTestRegistry("bash"), approve,
		WithWorkspaceRoot(t.TempDir()), WithClassifier(cls), WithInteractive(true))

	g.Execute(context.Background(), "bash", bashArgs(grayAreaCmd(0)))
	c, tot := g.valve.counts()
	if c == 0 && tot == 0 {
		t.Fatal("expected non-zero valve counts after a block")
	}

	// auto -> auto is not a leaving transition: counters must be preserved.
	g.SetMode(ModeAuto)
	if c2, tot2 := g.valve.counts(); c2 != c || tot2 != tot {
		t.Fatalf("entering/staying auto must not reset the valve, got %d,%d want %d,%d", c2, tot2, c, tot)
	}
}

// TestGate_ConcurrentSetModeResolve runs a SetMode/resolve goroutine pair so the
// -race detector proves the gate mode getter and setter share one mutex.
func TestGate_ConcurrentSetModeResolve(t *testing.T) {
	approve := func(_ context.Context, _ ApprovalRequest) (bool, bool, error) {
		return true, false, nil
	}
	g := New(config.PermissionsConfig{Mode: "smart"}, newTestRegistry("read_file"), approve)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); g.SetMode(ModePromptAll) }()
		go func() {
			defer wg.Done()
			g.Execute(context.Background(), "read_file", `{}`)
		}()
	}
	wg.Wait()
}
