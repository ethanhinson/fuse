package config

import (
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestModelMetadataKeysLoad(t *testing.T) {
	cfg, err := FromYAML([]byte(`
models:
  default: fast
  fast: {id: cloud/fast, label: Fast coder, description: Cheap, tags: [tools, house-special]}
  hidden: {id: cloud/hidden, advertise: false}
`), "test")
	if err != nil {
		t.Fatal(err)
	}
	fast := cfg.Models.Entries["fast"]
	if fast.Label != "Fast coder" || fast.Description != "Cheap" || !reflect.DeepEqual(fast.Tags, []string{"tools", "house-special"}) || fast.Advertise != nil {
		t.Fatalf("fast = %+v", fast)
	}
	if a := cfg.Models.Entries["hidden"].Advertise; a == nil || *a {
		t.Fatalf("hidden.advertise = %v, want false", a)
	}
}

func TestOldModelEntriesLoadUnchanged(t *testing.T) {
	cfg, err := FromYAML([]byte("models:\n  old: {id: cloud/old, max_tokens: 100, context_window: 2000, persona: coding, system_prefix: /x}\n"), "test")
	if err != nil {
		t.Fatal(err)
	}
	want := ModelConfig{ID: "cloud/old", MaxTokens: 100, ContextWindow: 2000, Persona: "coding", SystemPrefix: "/x"}
	if got := cfg.Models.Entries["old"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("old = %+v", got)
	}
}

// A TUI save of an entry without metadata must not sprout the new keys.
func TestModelMetadataKeysOmittedWhenEmpty(t *testing.T) {
	b, _ := yaml.Marshal(ModelConfig{ID: "cloud/x"})
	for _, k := range []string{"label:", "description:", "tags:", "advertise:"} {
		if strings.Contains(string(b), k) {
			t.Errorf("empty entry encoded %s\n%s", k, b)
		}
	}
}
