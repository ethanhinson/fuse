// Package permissions provides the HITL permission gate for tool execution.
package permissions

import "strings"

// PermissionMode controls global gate behaviour.
type PermissionMode int

const (
	// ModeSmart applies the built-in safe list and config patterns (default).
	ModeSmart PermissionMode = iota
	// ModeOff bypasses the gate; every tool auto-approves.
	ModeOff
	// ModePromptAll prompts for every call, ignoring the safe list.
	ModePromptAll
	// ModeAuto layers deterministic rules and a classifier for autonomous
	// approval. The gate pipeline is wired in a later task; this value only
	// exists so the mode can be parsed and configured.
	ModeAuto
)

// String returns the canonical token for the mode: "smart", "auto",
// "prompt-all", or "off". This is the single source of the mode name tokens
// shared by the config parser, the TUI indicator, and the /mode command — no
// mode-name string literal is duplicated across packages. It round-trips with
// ParseMode for the four known modes.
func (m PermissionMode) String() string {
	switch m {
	case ModeOff:
		return "off"
	case ModePromptAll:
		return "prompt-all"
	case ModeAuto:
		return "auto"
	default:
		return "smart"
	}
}

// ParseMode converts a yaml string to PermissionMode.
func ParseMode(s string) PermissionMode {
	switch s {
	case "off":
		return ModeOff
	case "prompt-all":
		return ModePromptAll
	case "auto":
		return ModeAuto
	default:
		return ModeSmart
	}
}

// ToolPolicy is the resolved approval stance for a single tool at a single call site.
type ToolPolicy struct {
	Enabled     bool
	AutoApprove bool
	// DenyReason, when non-empty, carries a layer-named explanation for an
	// auto-mode denial (e.g. "denied by auto-mode rules layer: <cmd>"). It is
	// empty for the legacy smart/off/prompt-all deny path, which surfaces the
	// fixed "tool call denied by user" message. Execute prefers DenyReason when
	// set. A deny is not an error: the model may retry with a different call.
	DenyReason string
}

// safeList is the baseline set of built-in tools that always auto-approve in
// smart and auto mode: the read-only tools, plus spawn_agent — harness-internal
// orchestration whose spawned child inherits a cloned gate (same mode, rules,
// classifier), so every action the child takes is independently gated and the
// spawn call itself is inert.
var safeList = map[string]bool{
	"read_file":      true,
	"list_directory": true,
	"grep":           true,
	"spawn_agent":    true,
	// ask_user only poses an interactive question to the human and blocks for
	// their answer — the human is already in the loop and can dismiss it, so a
	// separate y/s/n permission prompt in front of the question is redundant (a
	// double prompt). The question overlay IS the human gate.
	"ask_user": true,
	// segment_read is read-only over an existing transcript segment; no side
	// effects.
	"segment_read": true,
	// web_search issues a controlled query to a config-fixed engine (Brave/Tavily/
	// custom); the endpoint is not model-chosen, so there is no arbitrary-egress
	// surface (unlike web_fetch, which is deliberately NOT safe-listed).
	"web_search": true,
	// skill is orchestration: it loads skill instructions, and any spawned child
	// inherits a cloned, independently-gated gate — same rationale as spawn_agent,
	// so the skill call itself is inert.
	"skill": true,
	// pipeline_run is orchestration: spawned children inherit a cloned gate and are
	// independently re-gated — same rationale as spawn_agent.
	"pipeline_run": true,
}

// onSafeList returns true for the hard-coded safe set, all codeindex_* tools, and
// all blackboard_* tools. The blackboard (change 0023) is a session-scoped,
// in-memory shared scratchpad: its five tools (read/keys/wait are read-only;
// write/delete mutate) touch no disk, no network, and nothing outside the current
// agent tree, which is discarded when the run ends. Writing to it has no
// side-effect a user needs to gate — its worst case is corrupting a scratchpad
// that dies with the session, strictly less consequential than spawn_agent (also
// safe-listed). Auto-approving them lets agents coordinate through the board in
// smart mode without an approval prompt on every write; a user who wants the
// prompt back can still demote any of them via permissions.always_prompt.
func onSafeList(name string) bool {
	if strings.HasPrefix(name, "codeindex_") || strings.HasPrefix(name, "blackboard_") {
		return true
	}
	return safeList[name]
}
