package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// systemPrompt returns the first message when it is the system prompt.
func (r *request) systemPrompt() string {
	if len(r.Messages) > 0 && r.Messages[0].Role == "system" {
		return r.Messages[0].Content
	}
	return ""
}

// TestAgentsMD_InRootPromptNotChild proves through real requests that a
// sentinel in the working directory's AGENTS.md reaches the root agent's
// system prompt and not a spawned child's.
func TestAgentsMD_InRootPromptNotChild(t *testing.T) {
	srv := newServer(t,
		// Turn 1 (root): spawn a child.
		tool("spawn_agent", map[string]any{"label": "kid", "task": "do the thing"}),
		// The child (request 2) and the root's follow-up (request 3) answer
		// with plain text via the default "done" step.
	)
	h := newHarness(t, srv)
	h.write(filepath.Join(h.cwd, "AGENTS.md"), "AGENTSMD-SENTINEL-ROOT-ONLY\n", 0o644)

	res := h.run()
	wantCode(t, res, 0)
	srv.waitForRequests(t, 3, defaultTimeout)

	reqs := srv.requests()
	rootSys := reqs[0].systemPrompt()
	wantContains(t, rootSys, "AGENTSMD-SENTINEL-ROOT-ONLY", "root system prompt")
	wantContains(t, rootSys, "project_instructions", "root system prompt")

	childSys := reqs[1].systemPrompt()
	wantNotContains(t, childSys, "AGENTSMD-SENTINEL-ROOT-ONLY", "child system prompt")
	wantNotContains(t, childSys, "project_instructions", "child system prompt")
}

// TestAgentsMD_AbsentChangesNothing proves that with no AGENTS.md anywhere the
// system prompt carries no project_instructions block at all.
func TestAgentsMD_AbsentChangesNothing(t *testing.T) {
	srv := newServer(t, text("done"))
	h := newHarness(t, srv)

	res := h.run()
	wantCode(t, res, 0)

	sys := h.lastRequest().systemPrompt()
	wantNotContains(t, sys, "project_instructions", "system prompt without AGENTS.md")
	// Guard against the test passing vacuously because the sentinel helper
	// itself changed: the feature's marker really is this string.
	if strings.Contains(sys, "AGENTS.md") {
		t.Fatalf("unexpected AGENTS.md mention without a file:\n%s", sys)
	}
	// Sanity: the fake HOME really has no ~/.fuse/AGENTS.md.
	if _, err := os.Stat(filepath.Join(h.home, ".fuse", "AGENTS.md")); !os.IsNotExist(err) {
		t.Fatalf("home AGENTS.md unexpectedly present: %v", err)
	}
}
