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

// safeList is the baseline set of built-in read-only tools that always
// auto-approve in smart mode.
var safeList = map[string]bool{
	"read_file":      true,
	"list_directory": true,
	"grep":           true,
}

// onSafeList returns true for the hard-coded safe set and all codeindex_* tools.
func onSafeList(name string) bool {
	if strings.HasPrefix(name, "codeindex_") {
		return true
	}
	return safeList[name]
}
