package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstructionsFromYAML(t *testing.T) {
	c, err := FromYAML([]byte("instructions: |\n  You are a release-notes writer.\n  Keep it short: one line per change.\n"), "t")
	if err != nil {
		t.Fatal(err)
	}
	want := "You are a release-notes writer.\nKeep it short: one line per change.\n"
	if c.Instructions != want {
		t.Errorf("Instructions = %q, want %q", c.Instructions, want)
	}
	if Default().Instructions != "" {
		t.Errorf("Default().Instructions = %q, want empty", Default().Instructions)
	}
}

func writeInstructionsLayers(t *testing.T, local string) {
	t.Helper()
	cwd := chdirTemp(t)
	home := filepath.Join(cwd, "home")
	if err := os.MkdirAll(filepath.Join(home, ".fuse"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".fuse", "config.yml"), []byte("instructions: home\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	os.Unsetenv("LLM_GATEWAY_URL")
	os.Unsetenv("LLM_GATEWAY_KEY")
	if err := os.WriteFile(filepath.Join(cwd, ".fuse.local.yml"), []byte(local), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestInstructionsHonoredFromLocalLayer(t *testing.T) {
	writeInstructionsLayers(t, "instructions: local\n")
	warnings := captureWarnings(t)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Instructions != "local" {
		t.Errorf("Instructions = %q, want local", c.Instructions)
	}
	if strings.Contains(warnings(), "instructions") {
		t.Errorf("unexpected warning mentioning instructions: %q", warnings())
	}
}

func TestInstructionsLocalLayerWithoutKeyKeepsHome(t *testing.T) {
	writeInstructionsLayers(t, "max_turns: 3\n")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Instructions != "home" {
		t.Errorf("Instructions = %q, want home", c.Instructions)
	}
}

func TestOverlaySpecInstructions(t *testing.T) {
	base := Default()
	base.Instructions = "operator"
	c, refused, err := OverlaySpec(base, []byte("instructions: spec\n"), "s")
	if err != nil {
		t.Fatal(err)
	}
	if c.Instructions != "spec" || len(refused) != 0 {
		t.Errorf("Instructions = %q refused = %v, want spec and none", c.Instructions, refused)
	}
	c, _, err = OverlaySpec(base, []byte("max_tokens: 5\n"), "s")
	if err != nil {
		t.Fatal(err)
	}
	if c.Instructions != "operator" {
		t.Errorf("Instructions = %q, want operator", c.Instructions)
	}
	if base.Instructions != "operator" {
		t.Errorf("base mutated: %q", base.Instructions)
	}
}

func TestProblemsInstructionsLength(t *testing.T) {
	c := Default()
	c.Instructions = strings.Repeat("a", MaxInstructionsChars+1)
	ps := c.Problems()
	if len(ps) != 1 || ps[0].Path != "instructions" || !strings.Contains(ps[0].Message, "16384") {
		t.Fatalf("Problems = %+v", ps)
	}
	c.Instructions = strings.Repeat("é", MaxInstructionsChars)
	if ps := c.Problems(); len(ps) != 0 {
		t.Fatalf("Problems = %+v, want none", ps)
	}
}

// TestInstructionsWhitespaceLayerKeepsInherited: a layer whose instructions
// are whitespace-only counts as unset, matching prompt composition, so it never
// silently clears an inherited brief.
func TestInstructionsWhitespaceLayerKeepsInherited(t *testing.T) {
	writeInstructionsLayers(t, "instructions: \"  \\n\\t \"\n")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Instructions != "home" {
		t.Errorf("Instructions = %q, want home", c.Instructions)
	}
}

func TestOverlaySpecWhitespaceInstructionsKeepsOperator(t *testing.T) {
	base := Default()
	base.Instructions = "operator"
	c, _, err := OverlaySpec(base, []byte("instructions: \"  \\n\\t \"\n"), "s")
	if err != nil {
		t.Fatal(err)
	}
	if c.Instructions != "operator" {
		t.Errorf("Instructions = %q, want operator", c.Instructions)
	}
}
