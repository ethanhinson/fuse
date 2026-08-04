package model

import "testing"

func TestDefaultRegistryContainsNamedModels(t *testing.T) {
	r := DefaultRegistry()
	want := map[string]string{
		"deepseek-flash": "cloud/deepseek-v4-flash",
		"deepseek-pro":   "cloud/deepseek-v4-pro",
		"kimi":           "cloud/kimi-k3",
		"glm":            "cloud/glm-5.2",
		"qwen-cloud":     "cloud/qwen3-8b",
		"qwen-coder":     "local/qwen3-coder:30b",
		"qwen-local":     "local/qwen3.6:27b",
		"llama":          "local/llama3.1:8b",
		"claude":         "claude/sonnet",
	}
	for alias, id := range want {
		mc, err := r.Resolve(alias)
		if err != nil {
			t.Fatalf("resolve %s: %v", alias, err)
		}
		if mc.ID != id {
			t.Errorf("%s id = %q, want %q", alias, mc.ID, id)
		}
		if mc.MaxTokens == 0 {
			t.Errorf("%s max_tokens is zero", alias)
		}
	}
	if r.Default != "deepseek-flash" {
		t.Errorf("default = %q", r.Default)
	}
}

func TestResolveUnknownIsError(t *testing.T) {
	r := DefaultRegistry()
	if _, err := r.Resolve("nope"); err == nil {
		t.Fatal("expected error for unknown model")
	}
}

func TestNamesSorted(t *testing.T) {
	r := NewRegistry("b", map[string]ModelConfig{
		"c": {ID: "x", MaxTokens: 1},
		"a": {ID: "y", MaxTokens: 1},
		"b": {ID: "z", MaxTokens: 1},
	})
	got := r.Names()
	want := []string{"a", "b", "c"}
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("names = %v", got)
	}
}
