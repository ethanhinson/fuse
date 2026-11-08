package main

import (
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/internal/config"
)

func offered(cfg config.ResearchConfig) map[string]bool {
	have := map[string]bool{}
	for _, s := range defaultToolRegistry(nil, cfg, nil).Schemas() {
		have[s.Name] = true
	}
	return have
}

// web_search is offered only when a provider resolves; web_fetch always is.
// codeindex is not a core tool: users add it themselves if they want it.
func TestWebSearchIsOfferedOnlyWithAProvider(t *testing.T) {
	t.Setenv("BRAVE_SEARCH_API_KEY", "")
	t.Setenv("TAVILY_API_KEY", "")
	have := offered(config.ResearchConfig{})
	if have["web_search"] || !have["web_fetch"] {
		t.Errorf("no provider: offered %v", have)
	}
	for name := range have {
		if strings.HasPrefix(name, "codeindex") {
			t.Errorf("%s is offered as a core tool", name)
		}
	}

	t.Setenv("BRAVE_SEARCH_API_KEY", "k")
	if !offered(config.ResearchConfig{})["web_search"] {
		t.Error("web_search missing with BRAVE_SEARCH_API_KEY set")
	}
	t.Setenv("BRAVE_SEARCH_API_KEY", "")
	if !offered(config.ResearchConfig{Custom: config.CustomProviderConfig{URL: "http://search.local"}})["web_search"] {
		t.Error("web_search missing with a custom provider URL")
	}
}

// A spawn that names web_search when no provider is configured says why.
func TestSpawnNamingAnUnofferedWebSearchSaysWhy(t *testing.T) {
	t.Setenv("BRAVE_SEARCH_API_KEY", "")
	t.Setenv("TAVILY_API_KEY", "")
	_, err := childToolRegistry(defaultToolRegistry(nil, config.ResearchConfig{}, nil), []string{"web_search", "read_file"})
	if err == nil || !strings.Contains(err.Error(), "no search provider is configured") {
		t.Fatalf("err = %v", err)
	}
}
