package config

import "fmt"

// validModes is the set of permission modes ParseMode understands. It is checked
// explicitly because ParseMode silently maps any unknown token to "smart", which
// turns a typo (mode: smrat) into a silent, wrong posture instead of an error.
var validModes = map[string]bool{
	"off":        true,
	"prompt-all": true,
	"smart":      true,
	"auto":       true,
}

// Validate performs structural validation of a resolved Config and returns the
// first problem found, or nil. It deliberately does NOT resolve model aliases —
// that requires the model.Registry, which lives above this leaf package, so
// model-reference checks are done by the caller (see cmd/fuse) where both the
// config and the registry are in scope. Validate covers the checks that need
// only the config itself: enum membership and numeric sanity.
//
// The loader already clamps some numerics (e.g. negative MaxSpawns → 64); this
// method catches the values the loader passes through untouched and the enums it
// silently defaults, so a malformed config fails loudly at startup rather than
// behaving surprisingly at runtime.
func (c Config) Validate() error {
	if m := c.Permissions.Mode; m != "" && !validModes[m] {
		return fmt.Errorf("config: permissions.mode %q is invalid (want off, prompt-all, smart, or auto)", m)
	}

	// Note: agents.tool_timeout_seconds is intentionally NOT checked here. The
	// loader's tighten-only merge (ADR-0006) only applies a value when it is > 0
	// and silently drops a negative, so a negative can never reach the resolved
	// Config — a Validate check for it would be dead code implying a protection
	// that does not exist. MaxTurns/MaxTokens ARE passed through unclamped, so
	// they are checked below.
	if c.MaxTokens < 0 {
		return fmt.Errorf("config: max_tokens must be >= 0, got %d", c.MaxTokens)
	}
	if c.MaxTurns != nil && *c.MaxTurns < 0 {
		return fmt.Errorf("config: max_turns must be >= 0 (0 = unlimited), got %d", *c.MaxTurns)
	}

	// Summarization threshold is a context-window fraction; only 0 (unset) or a
	// value in (0,1] is meaningful. A value >1 or <0 would never (or always)
	// trigger, silently defeating Tier-2 summarization.
	if t := c.Context.Summarization.Threshold; t < 0 || t > 1 {
		return fmt.Errorf("config: context.summarization.threshold must be in [0,1], got %g", t)
	}
	// Relevance borderline band must be ordered and within [0,1] when set.
	rel := c.Context.Relevance
	if rel.BorderlineLo < 0 || rel.BorderlineHi > 1 || (rel.BorderlineHi > 0 && rel.BorderlineLo > rel.BorderlineHi) {
		return fmt.Errorf("config: context.relevance borderline band [%g,%g] is invalid (want 0<=lo<=hi<=1)", rel.BorderlineLo, rel.BorderlineHi)
	}
	if rel.RecencyFloorPct < 0 || rel.RecencyFloorPct > 100 {
		return fmt.Errorf("config: context.relevance.recency_floor_pct must be in [0,100], got %d", rel.RecencyFloorPct)
	}

	return nil
}
