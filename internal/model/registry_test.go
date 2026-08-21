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
		"qwen-coder":     "local/qwen-coder-7b",
		"qwen-local":     "local/qwen-7b",
		"llama":          "local/llama3.1:8b",
		"claude":         "claude/sonnet",
		"sonnet-5":       "claude/sonnet-5",
		"minimax":        "cloud/minimax-m3",
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

// TestCapableAliasesHaveSynthesisHeadroom: the capable cloud models used to
// drive research must have an output ceiling large enough for a full cited
// report (body + numbered source list). 8192 truncated sonnet-5 mid-report.
func TestCapableAliasesHaveSynthesisHeadroom(t *testing.T) {
	r := DefaultRegistry()
	for _, alias := range []string{"sonnet-5", "minimax", "deepseek-pro", "kimi", "glm"} {
		mc, err := r.Resolve(alias)
		if err != nil {
			t.Fatalf("resolve %s: %v", alias, err)
		}
		if mc.MaxTokens < 16384 {
			t.Errorf("%s max_tokens = %d, want >= 16384 for synthesis headroom", alias, mc.MaxTokens)
		}
	}
}

func TestResolveUnknownIsError(t *testing.T) {
	r := DefaultRegistry()
	if _, err := r.Resolve("nope"); err == nil {
		t.Fatal("expected error for unknown model")
	}
}

func TestDefaultAlias(t *testing.T) {
	r := NewRegistry("glm", map[string]ModelConfig{"glm": {ID: "cloud/glm-5.2", MaxTokens: 1}})
	if got := r.DefaultAlias(); got != "glm" {
		t.Errorf("DefaultAlias() = %q, want %q", got, "glm")
	}

	dr := DefaultRegistry()
	if got := dr.DefaultAlias(); got != "deepseek-flash" {
		t.Errorf("DefaultRegistry().DefaultAlias() = %q, want %q", got, "deepseek-flash")
	}
	if dr.DefaultAlias() != dr.Default {
		t.Errorf("DefaultAlias() = %q, want equal to Default field %q", dr.DefaultAlias(), dr.Default)
	}

	var zero Registry
	if got := zero.DefaultAlias(); got != "" {
		t.Errorf("zero-value Registry.DefaultAlias() = %q, want empty", got)
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
