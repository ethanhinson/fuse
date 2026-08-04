package main

import (
	"fmt"
	"io"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/skills"
	"github.com/ethanhinson/fuse/internal/tui"
)

// runShell loads skills and runs the interactive bubbletea TUI. It parses a
// minimal set of pre-flags (--model NAME, --verbose) and then hands control to
// the ShellModel. One-shot mode (cmd/fuse/main.go) is unaffected.
func runShell(args []string, cfg config.Config, reg *model.Registry, stdout, stderr io.Writer) int {
	alias := reg.Default
	verbose := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--model":
			if i+1 < len(args) {
				alias = args[i+1]
				i++
			}
		case "--verbose":
			verbose = true
		}
	}

	set, err := skills.Load(skills.DefaultDirs())
	if err != nil {
		fmt.Fprintf(stderr, "skills error: %v\n", err)
		return 1
	}
	skillBlock := set.SystemPromptBlock()

	// build binds an agent to the given renderer and the currently-selected
	// alias for one turn.
	build := func(a string, r agent.Renderer) (*agent.Agent, error) {
		return buildAgentWithRenderer(cfg, reg, a, r, verbose, skillBlock)
	}

	m := tui.NewShellModel(alias, verbose, reg, set.SlashCommands(), build)
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion(), tea.WithOutput(stdout))
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(stderr, "tui error: %v\n", err)
		return 1
	}
	return 0
}
