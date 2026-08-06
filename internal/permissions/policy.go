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
}

// onSafeList returns true for the hard-coded safe set and all codeindex_* tools.
func onSafeList(name string) bool {
	if strings.HasPrefix(name, "codeindex_") {
		return true
	}
	return safeList[name]
}
