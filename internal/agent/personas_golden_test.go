package agent

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

var updateGolden = flag.Bool("update", false, "rewrite golden files")

const composeGoldenPath = "testdata/compose_golden.json"

// composeNoInstructions is the single call into ComposeSystemPrompt this golden
// test makes, so a signature change touches one line here.
func composeNoInstructions(persona, prefix, extra string) string {
	return ComposeSystemPrompt(persona, prefix, "", extra)
}

// composeGoldenCases returns every persona (plus an unknown name) crossed with
// an empty and a set prefix and an empty and a set extra block, keyed
// "persona|prefix|extra-flag".
func composeGoldenCases() map[string]string {
	personas := []string{"coding", "research", "reasoning", "general", "nope"}
	prefixes := []string{"", "/no_think"}
	extras := []struct{ flag, text string }{{"no-extra", ""}, {"extra", "## Skills\n- /foo"}}
	out := map[string]string{}
	for _, p := range personas {
		for _, pre := range prefixes {
			for _, ex := range extras {
				out[p+"|"+pre+"|"+ex.flag] = composeNoInstructions(p, pre, ex.text)
			}
		}
	}
	return out
}

// TestComposeSystemPromptGolden pins the composed system prompt byte for byte
// for every persona, prefix and extra-block combination.
func TestComposeSystemPromptGolden(t *testing.T) {
	got := composeGoldenCases()
	if *updateGolden {
		b, err := json.MarshalIndent(got, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(composeGoldenPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(composeGoldenPath, append(b, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	b, err := os.ReadFile(composeGoldenPath)
	if err != nil {
		t.Fatalf("read golden (run with -update to create it): %v", err)
	}
	var want map[string]string
	if err := json.Unmarshal(b, &want); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Errorf("case count = %d, golden has %d", len(got), len(want))
	}
	for name, w := range want {
		g, ok := got[name]
		if !ok {
			t.Errorf("golden case %q is no longer produced", name)
			continue
		}
		if g != w {
			t.Errorf("case %q differs from golden:\n got: %q\nwant: %q", name, g, w)
		}
	}
}
