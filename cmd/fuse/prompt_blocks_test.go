package main

import (
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/tools"
)

// TestRootSystemBlockFollowsDisabledTools: the spawn block rides only while
// spawn_agent is enabled, and the skills listing only while skill is.
func TestRootSystemBlockFollowsDisabledTools(t *testing.T) {
	var cfg config.Config
	if got := rootSystemBlock(cfg, nil); !strings.Contains(got, spawnAgentBlock) {
		t.Fatalf("enabled: spawn block missing")
	}
	cfg.Permissions.Disabled = []string{"spawn_agent", "skill"}
	if got := rootSystemBlock(cfg, nil); got != "" {
		t.Fatalf("disabled: want empty block, got %q", got)
	}
}

// TestWithoutUnavailableSpawnBlock: the block is stripped when spawn_agent is
// disabled by policy or absent from the agent's registry, and left alone
// otherwise; text without the block passes through byte-identical.
func TestWithoutUnavailableSpawnBlock(t *testing.T) {
	var cfg config.Config
	withSpawn := tools.NewRegistry()
	withSpawn.Register(staticTool{name: "spawn_agent"})
	extra := "persona" + spawnAgentBlock + "\n\ntail"

	if got := withoutUnavailableSpawnBlock(extra, cfg, withSpawn); got != extra {
		t.Errorf("available: block should be kept")
	}
	if got := withoutUnavailableSpawnBlock(extra, cfg, tools.NewRegistry()); got != "persona\n\ntail" {
		t.Errorf("unregistered: got %q", got)
	}
	cfg.Permissions.Disabled = []string{"spawn_agent"}
	if got := withoutUnavailableSpawnBlock(extra, cfg, withSpawn); got != "persona\n\ntail" {
		t.Errorf("disabled: got %q", got)
	}
	if got := withoutUnavailableSpawnBlock("no block here", cfg, withSpawn); got != "no block here" {
		t.Errorf("passthrough: got %q", got)
	}
}

// TestRootSystemBlockNoRolesUnchanged: with no roles defined the block is the
// bare spawn block, byte for byte.
func TestRootSystemBlockNoRolesUnchanged(t *testing.T) {
	var cfg config.Config
	if got := rootSystemBlock(cfg, nil); got != spawnAgentBlock {
		t.Fatalf("no roles: block changed:\n%q", got)
	}
}

// TestRootSystemBlockRolesAppendsHint: with a role defined, exactly one
// role-preference sentence follows the spawn block.
func TestRootSystemBlockRolesAppendsHint(t *testing.T) {
	cfg := config.Config{Roles: map[string]config.RoleConfig{"writer": {Description: "writes"}}}
	got := rootSystemBlock(cfg, nil)
	want := spawnAgentBlock + "\n\nPrefer a named role when one fits the task."
	if got != want {
		t.Fatalf("roles: got %q\nwant %q", got, want)
	}
	if n := strings.Count(got, "Prefer a named role when one fits the task."); n != 1 {
		t.Fatalf("sentence count = %d, want 1", n)
	}
}

// TestRootSystemBlockSpawnDisabledNoHint: a disabled spawn_agent drops the
// block and the sentence together.
func TestRootSystemBlockSpawnDisabledNoHint(t *testing.T) {
	cfg := config.Config{Roles: map[string]config.RoleConfig{"writer": {}}}
	cfg.Permissions.Disabled = []string{"spawn_agent"}
	if got := rootSystemBlock(cfg, nil); got != "" {
		t.Fatalf("disabled: want empty, got %q", got)
	}
}

// TestWithoutUnavailableSpawnBlockStripsHint: the role-preference sentence is
// stripped with the block when spawn_agent is unavailable, kept otherwise.
func TestWithoutUnavailableSpawnBlockStripsHint(t *testing.T) {
	var cfg config.Config
	extra := "X" + spawnAgentBlock + spawnRoleHint + "Y"
	if got := withoutUnavailableSpawnBlock(extra, cfg, tools.NewRegistry()); got != "XY" {
		t.Errorf("unregistered: got %q", got)
	}
	withSpawn := tools.NewRegistry()
	withSpawn.Register(staticTool{name: "spawn_agent"})
	if got := withoutUnavailableSpawnBlock(extra, cfg, withSpawn); got != extra {
		t.Errorf("available: extra changed")
	}
}

// The spawn guidance must agree with the coding prompt: no "spawn aggressively"
// and no spawning to read files.
func TestSpawnGuidanceDoesNotPushFanOut(t *testing.T) {
	low := strings.ToLower(spawnAgentBlock)
	for _, bad := range []string{"aggressive", "never read 3+ files", "spawn one agent per source"} {
		if strings.Contains(low, bad) {
			t.Errorf("spawn guidance still says %q", bad)
		}
	}
	for _, want := range []string{"truly run in parallel", "do not spawn to read files", "in one response"} {
		if !strings.Contains(low, want) {
			t.Errorf("spawn guidance lost %q", want)
		}
	}
}
