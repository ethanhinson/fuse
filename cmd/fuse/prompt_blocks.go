package main

import (
	"strings"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/skills"
	"github.com/ethanhinson/fuse/internal/tools"
)

// The system prompt carries instruction blocks that only make sense when the
// tool they describe is on offer: the skills listing drives the skill tool,
// the parallel-subagents block drives spawn_agent. A block for a tool the
// model cannot call is dead weight on every call of the run (the skills
// listing alone is several thousand characters) and, for spawn_agent, an
// instruction the model is told to follow "aggressively" and then refused.
// These helpers keep the blocks tied to tool availability at both places it
// is decided: the deployment's permissions.disabled list, applied where the
// root prompt is composed, and the agent's own registry, applied where each
// agent is built (children have spawn_agent unregistered).

// toolDisabled reports whether name is on the deployment's permissions.disabled
// list, which the gate denies unconditionally and no longer advertises.
func toolDisabled(cfg config.Config, name string) bool {
	for _, d := range cfg.Permissions.Disabled {
		if d == name {
			return true
		}
	}
	return false
}

// rootSystemBlock composes the root agent's tool-driven prompt blocks: the
// skills listing when the skill tool is enabled and the parallel-subagents
// block when spawn_agent is. Every root binding (one-shot, shell, loop
// server, networked runtime) composes through here so the rule is the same
// everywhere.
func rootSystemBlock(cfg config.Config, skillSet *skills.Set) string {
	var b strings.Builder
	if skillSet != nil && !toolDisabled(cfg, "skill") {
		b.WriteString(skillSet.SystemPromptBlock())
	}
	if !toolDisabled(cfg, "spawn_agent") {
		b.WriteString(spawnAgentBlock)
	}
	return b.String()
}

// withoutUnavailableSpawnBlock strips the parallel-subagents block from an
// agent's extra prompt when that agent cannot call spawn_agent: the tool is
// disabled by policy, or absent from the registry this agent was built with
// (a child's registry has it unregistered). The block is a fixed constant, so
// an exact-substring removal is precise; extra without the block is returned
// unchanged.
func withoutUnavailableSpawnBlock(extra string, cfg config.Config, toolReg *tools.Registry) string {
	if !strings.Contains(extra, spawnAgentBlock) {
		return extra
	}
	if !toolDisabled(cfg, "spawn_agent") && toolReg != nil && toolReg.Has("spawn_agent") {
		return extra
	}
	return strings.ReplaceAll(extra, spawnAgentBlock, "")
}
