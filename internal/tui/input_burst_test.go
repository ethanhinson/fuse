package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ethanhinson/fuse/internal/permissions"
)

// Text that arrives as one multi-character burst (fast input, automation, a terminal
// without bracketed paste) must be typed into the prompt, not read as a key. A burst of
// the word "right" has the same string form as the Right-arrow key, and used to be
// swallowed as cursor movement, so "doing right now" came out as "doing  now".
func TestInputBurstsAreTextNotKeys(t *testing.T) {
	for _, word := range []string{"right", "left", "up", "down", "home", "end", "enter", "tab", "esc"} {
		t.Run(word, func(t *testing.T) {
			m := sized(NewShellModel("alpha", false, "dark", testRegistry(), nil, nilBuilder, permissions.NewSessionMode(permissions.ModeSmart), true))
			// The words arrive the way a fast typist or a tmux send-keys delivers them: the
			// text around the word, then the word alone as one multi-character message.
			var cur tea.Model = m
			for _, chunk := range []string{"doing ", word, " now"} {
				cur, _ = cur.(ShellModel).Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(chunk)})
			}
			if got := cur.(ShellModel).input.Value(); got != "doing "+word+" now" {
				t.Fatalf("input = %q, want %q", got, "doing "+word+" now")
			}
		})
	}
}
