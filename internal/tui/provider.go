package tui

import "fmt"

// SlashKind identifies the source of a slash command for display.
type SlashKind string

const (
	KindBuiltin SlashKind = "builtin"
	KindSkill   SlashKind = "skill"
	KindMCP     SlashKind = "mcp"
)

// SlashEntry is one item in the autocomplete list.
type SlashEntry struct {
	Command     string // e.g. "/model", "/code-review", "/mcp:everything/echo"
	Syntax      string // arg hint shown beside Command, e.g. "NAME" for /model
	Description string // one-line description
	Kind        SlashKind
	Server      string // populated for KindMCP
	expand      func() string
}

// Expansion returns the text to inject into the shell input on selection.
func (e SlashEntry) Expansion() string {
	if e.expand == nil {
		return e.Command + " "
	}
	return e.expand()
}

// KindTag returns the display string for the kind column, e.g. "[builtin]" or "[mcp:server]".
func (e SlashEntry) KindTag() string {
	switch e.Kind {
	case KindMCP:
		return fmt.Sprintf("[mcp:%s]", e.Server)
	case KindSkill:
		return "[skill]"
	default:
		return "[builtin]"
	}
}

// CommandProvider is a live source of slash commands.
type CommandProvider interface {
	// Commands returns the current snapshot of entries from this source.
	// Must be safe for concurrent calls.
	Commands() []SlashEntry

	// Changes returns a channel that receives a signal whenever the provider's
	// command set may have changed. A nil channel marks a static provider.
	Changes() <-chan struct{}

	// Close releases any background resources.
	Close()
}
