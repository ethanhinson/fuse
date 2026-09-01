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

func TestEntriesSortedByAlias(t *testing.T) {
	r := NewRegistry("b", map[string]ModelConfig{
		"c": {ID: "x", MaxTokens: 3},
		"a": {ID: "y", MaxTokens: 1},
		"b": {ID: "z", MaxTokens: 2},
	})
	got := r.Entries()
	if len(got) != 3 {
		t.Fatalf("entries len = %d", len(got))
	}
	wantAlias := []string{"a", "b", "c"}
	wantID := []string{"y", "z", "x"}
	for i, nm := range got {
		if nm.Alias != wantAlias[i] {
			t.Errorf("entry %d alias = %q, want %q", i, nm.Alias, wantAlias[i])
		}
		if nm.Config.ID != wantID[i] {
			t.Errorf("entry %d id = %q, want %q", i, nm.Config.ID, wantID[i])
		}
	}
}

func TestDefaultAliasAndHas(t *testing.T) {
	r := NewRegistry("b", map[string]ModelConfig{"a": {ID: "y"}, "b": {ID: "z"}})
	if r.DefaultAlias() != "b" {
		t.Errorf("DefaultAlias = %q, want b", r.DefaultAlias())
	}
	if !r.Has("a") {
		t.Error("Has(a) = false, want true")
	}
	if r.Has("nope") {
		t.Error("Has(nope) = true, want false")
	}
}

func TestSetInsertsAndReplaces(t *testing.T) {
	r := NewRegistry("a", map[string]ModelConfig{"a": {ID: "y"}})
	// Insert a new alias.
	r.Set("new", ModelConfig{ID: "cloud/new", MaxTokens: 5, Persona: "coding"})
	mc, err := r.Resolve("new")
	if err != nil {
		t.Fatalf("resolve new: %v", err)
	}
	if mc.ID != "cloud/new" || mc.MaxTokens != 5 {
		t.Errorf("new = %+v", mc)
	}
	// Replace an existing alias.
	r.Set("a", ModelConfig{ID: "cloud/replaced"})
	mc, _ = r.Resolve("a")
	if mc.ID != "cloud/replaced" {
		t.Errorf("replaced a = %q, want cloud/replaced", mc.ID)
	}
}

func TestSetOnNilEntriesMap(t *testing.T) {
	// A registry built with an empty map still accepts Set.
	r := NewRegistry("", nil)
	r.Set("x", ModelConfig{ID: "cloud/x"})
	if !r.Has("x") {
		t.Fatal("Set did not register x")
	}
}

func TestRemove(t *testing.T) {
	r := NewRegistry("def", map[string]ModelConfig{
		"def":   {ID: "cloud/def"},
		"extra": {ID: "cloud/extra"},
	})
	// Removing a non-default alias succeeds.
	if !r.Remove("extra") {
		t.Error("Remove(extra) = false, want true")
	}
	if r.Has("extra") {
		t.Error("extra still present after Remove")
	}
	// Removing an unknown alias reports false.
	if r.Remove("nope") {
		t.Error("Remove(nope) = true, want false")
	}
	// The default alias is protected.
	if r.Remove("def") {
		t.Error("Remove(def) = true, want false — default must be protected")
	}
	if !r.Has("def") {
		t.Error("default was removed despite protection")
	}
}
