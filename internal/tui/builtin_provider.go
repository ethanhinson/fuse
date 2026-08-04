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
			{Command: "/model", Syntax: "NAME", Description: "Switch model (e.g. sonnet, opus)", Kind: KindBuiltin, expand: func() string { return "/model " }},
		},
	}
}

func (b *BuiltinProvider) Commands() []SlashEntry     { return b.entries }
func (b *BuiltinProvider) Changes() <-chan struct{}    { return nil }
func (b *BuiltinProvider) Close()                     {}
