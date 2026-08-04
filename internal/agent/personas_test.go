package agent

import (
	"strings"
	"testing"
)

func TestPersonaPrompt(t *testing.T) {
	for _, persona := range []string{"coding", "research", "reasoning", "general"} {
		p := PersonaPrompt(persona)
		if p == "" {
			t.Errorf("PersonaPrompt(%q) returned empty string", persona)
		}
	}
	// Unknown persona falls back to general prompt.
	if PersonaPrompt("unknown") != PersonaPrompt("general") {
		t.Error("unknown persona should fall back to general")
	}
}

func TestComposeSystemPrompt(t *testing.T) {
	base := PersonaPrompt("coding")

	// No extra: returns just the persona prompt.
	if got := ComposeSystemPrompt("coding", ""); got != base {
		t.Errorf("expected base prompt, got %q", got)
	}

	// With extra: persona prompt is prepended, extra appended.
	extra := "## Skills\n- /foo"
	composed := ComposeSystemPrompt("coding", extra)
	if !strings.HasPrefix(composed, base) {
		t.Error("composed prompt should start with persona prompt")
	}
	if !strings.HasSuffix(composed, extra) {
		t.Error("composed prompt should end with extra block")
	}
}
