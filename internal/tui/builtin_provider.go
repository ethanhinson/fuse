package tui

// BuiltinProvider is a static CommandProvider. Changes() returns nil.
type BuiltinProvider struct {
	entries []SlashEntry
}

// NewBuiltinProvider returns a provider with the fixed built-in commands.
func NewBuiltinProvider() *BuiltinProvider {
	return &BuiltinProvider{
		entries: []SlashEntry{
			{Command: "/exit", Description: "Exit the shell", Kind: KindBuiltin, expand: func() string { return "/exit" }},
			{Command: "/quit", Description: "Exit the shell", Kind: KindBuiltin, expand: func() string { return "/quit" }},
			{Command: "/verbose", Description: "Toggle verbose tool output", Kind: KindBuiltin, expand: func() string { return "/verbose" }},
			{Command: "/models", Syntax: "[edit]", Description: "List available models; /models edit opens the mapping editor", Kind: KindBuiltin, expand: func() string { return "/models" }},
			{Command: "/model", Syntax: "NAME", Description: "Switch model (Tab to complete an alias)", Kind: KindBuiltin, expand: func() string { return "/model " }},
			{Command: "/mode", Syntax: "NAME", Description: "Show or set the permission mode (smart/auto/prompt-all/off)", Kind: KindBuiltin, expand: func() string { return "/mode " }},
			{Command: "/config", Description: "Open the tabbed settings screen (models, permissions, MCP)", Kind: KindBuiltin, expand: func() string { return "/config" }},
			{Command: "/agents", Description: "Open the live agent tree (also: Tab)", Kind: KindBuiltin, expand: func() string { return "/agents" }},
			{Command: "/approvals", Description: "Show this session's permission decisions", Kind: KindBuiltin, expand: func() string { return "/approvals" }},
			{Command: "/questions", Description: "Show this session's answered ask_user questions", Kind: KindBuiltin, expand: func() string { return "/questions" }},
			{Command: "/interrupt", Syntax: "MESSAGE", Description: "Stop the agent's current step and redirect it", Kind: KindBuiltin, expand: func() string { return "/interrupt " }},
			{Command: "/clear", Description: "Clear the conversation history and screen (start a fresh task)", Kind: KindBuiltin, expand: func() string { return "/clear" }},
			{Command: "/queue", Description: "Open the pending human-message queue editor (edit/reorder/delete)", Kind: KindBuiltin, expand: func() string { return "/queue" }},
		},
	}
}

func (b *BuiltinProvider) Commands() []SlashEntry   { return b.entries }
func (b *BuiltinProvider) Changes() <-chan struct{} { return nil }
func (b *BuiltinProvider) Close()                   {}
