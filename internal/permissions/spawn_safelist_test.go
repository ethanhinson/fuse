package permissions

import (
	"context"
	"testing"

	"github.com/ethanhinson/fuse/internal/config"
)

// spawn_agent is harness-internal orchestration: the spawned child inherits a
// cloned gate (same mode, rules, and classifier), so every action the child
// takes is independently gated. The spawn call itself is inert and must not
// prompt — in auto mode or smart mode.
func TestSpawnAgent_AutoApprovesWithoutPrompt(t *testing.T) {
	for _, mode := range []string{"auto", "smart"} {
		t.Run(mode, func(t *testing.T) {
			prompted := false
			approve := func(ctx context.Context, req ApprovalRequest) (bool, bool, error) {
				prompted = true
				return true, false, nil
			}
			g := New(config.PermissionsConfig{Mode: mode}, newTestRegistry("spawn_agent"), approve)
			res := g.Execute(context.Background(), "spawn_agent", `{"task":"do a thing","label":"worker"}`)
			if res.IsError {
				t.Fatalf("spawn_agent errored: %s", res.Output)
			}
			if prompted {
				t.Fatal("spawn_agent must not prompt — the child's own tool calls are gated")
			}
		})
	}
}
