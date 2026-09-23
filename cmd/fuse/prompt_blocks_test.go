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
