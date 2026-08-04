package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/skills"
)

// shellState is the mutable state of an interactive session.
type shellState struct {
	cfg        config.Config
	reg        *model.Registry
	alias      string
	verbose    bool
	history    []model.Message
	skillBlock string
	slash      map[string]skills.Skill
}

// runShell initializes shell state (loading skills) and enters the REPL.
func runShell(args []string, cfg config.Config, reg *model.Registry, stdout, stderr io.Writer) int {
	alias := reg.Default
	verbose := false
	// Minimal flag handling: --model NAME and --verbose before entering.
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

	st := &shellState{
		cfg:        cfg,
		reg:        reg,
		alias:      alias,
		verbose:    verbose,
		skillBlock: set.SystemPromptBlock(),
		slash:      set.SlashCommands(),
	}
	fmt.Fprintf(stdout, "Fuse  %s\n", st.alias)
	fmt.Fprintln(stdout, "Type a task, /model NAME to switch, /verbose to toggle, /exit to quit.")
	return replLoop(os.Stdin, stdout, st)
}

// systemPrompt composes the system context: skill listing block.
func (st *shellState) systemPrompt() string {
	return st.skillBlock
}

// replLoop reads lines from in, dispatching slash commands and otherwise
// running an agent turn on the accumulating history. Returns an exit code.
func replLoop(in io.Reader, out io.Writer, st *shellState) int {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for {
		fmt.Fprintf(out, "[%s] > ", st.alias)
		if !sc.Scan() {
			return 0
		}
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "/") {
			if quit := st.handleSlash(line, out); quit {
				return 0
			}
			continue
		}
		st.runPrompt(line, out)
	}
}

// handleSlash dispatches a slash command. Returns true if the shell should quit.
func (st *shellState) handleSlash(line string, out io.Writer) bool {
	fields := strings.Fields(line)
	cmd := fields[0]
	switch cmd {
	case "/exit", "/quit":
		return true
	case "/verbose":
		st.verbose = !st.verbose
		fmt.Fprintf(out, "verbose = %v\n", st.verbose)
		return false
	case "/model":
		if len(fields) < 2 {
			fmt.Fprintln(out, "usage: /model NAME")
			return false
		}
		name := fields[1]
		if _, err := st.reg.Resolve(name); err != nil {
			fmt.Fprintf(out, "unknown model %q\n", name)
			return false
		}
		st.alias = name
		fmt.Fprintf(out, "switched to %s\n", name)
		return false
	}
	if sk, ok := st.slash[cmd]; ok {
		// A skill slash command injects the skill body as the next prompt.
		st.runPrompt(sk.Body, out)
		return false
	}
	fmt.Fprintf(out, "unknown command %s\n", cmd)
	return false
}

// runPrompt appends the user line to history and runs one agent loop over it.
func (st *shellState) runPrompt(line string, out io.Writer) {
	a, _, err := buildAgent(st.cfg, st.reg, st.alias, out, st.verbose, st.systemPrompt())
	if err != nil {
		fmt.Fprintf(out, "error: %v\n", err)
		return
	}
	// The agent already renders its turn output through the renderer injected
	// by buildAgent; emit only the per-turn model header here so exactly one
	// renderer handles a given turn (header + body) with no orphaned instance
	// and no redundant per-turn renderer allocation.
	fmt.Fprintf(out, "\n── %s ──────────────\n", st.alias)
	st.history = append(st.history, model.Message{Role: "user", Content: line})
	updated, err := a.Run(context.Background(), st.history)
	if err != nil {
		fmt.Fprintf(out, "run error: %v\n", err)
	}
	st.history = updated
}
