package research

import (
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/internal/config"
)

// fakeEnv returns an env-lookup func backed by a static map, so tests never
// touch os.Getenv.
func fakeEnv(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestResolve(t *testing.T) {
	tests := []struct {
		name string
		cfg  config.ResearchConfig
		env  map[string]string

		wantErr      bool
		errContains  []string // substrings the error message must contain
		wantProvider string   // expected Name() of the resolved provider (when no error)
		wantType     string   // concrete Go type name assertion (optional)
	}{
		{
			name:         "explicit brave with key",
			cfg:          config.ResearchConfig{Provider: "brave"},
			env:          map[string]string{"BRAVE_SEARCH_API_KEY": "bk"},
			wantProvider: "brave",
			wantType:     "*research.BraveSearchProvider",
		},
		{
			name:        "explicit brave without key",
			cfg:         config.ResearchConfig{Provider: "brave"},
			env:         map[string]string{},
			wantErr:     true,
			errContains: []string{"BRAVE_SEARCH_API_KEY"},
		},
		{
			name:         "config provider beats env",
			cfg:          config.ResearchConfig{Provider: "tavily"},
			env:          map[string]string{"TAVILY_API_KEY": "tk", "BRAVE_SEARCH_API_KEY": "bk"},
			wantProvider: "tavily",
			wantType:     "*research.TavilyProvider",
		},
		{
			name:         "auto brave beats tavily",
			cfg:          config.ResearchConfig{},
			env:          map[string]string{"BRAVE_SEARCH_API_KEY": "bk", "TAVILY_API_KEY": "tk"},
			wantProvider: "brave",
			wantType:     "*research.BraveSearchProvider",
		},
		{
			name:         "auto only tavily",
			cfg:          config.ResearchConfig{},
			env:          map[string]string{"TAVILY_API_KEY": "tk"},
			wantProvider: "tavily",
			wantType:     "*research.TavilyProvider",
		},
		{
			name:         "auto custom url set",
			cfg:          config.ResearchConfig{Custom: config.CustomProviderConfig{URL: "http://searx.local/search"}},
			env:          map[string]string{},
			wantProvider: "custom",
			wantType:     "*research.CustomHTTPProvider",
		},
		{
			name:        "auto nothing set names all three paths",
			cfg:         config.ResearchConfig{},
			env:         map[string]string{},
			wantErr:     true,
			errContains: []string{"BRAVE_SEARCH_API_KEY", "TAVILY_API_KEY", "research.custom"},
		},
		{
			name:        "explicit custom empty url",
			cfg:         config.ResearchConfig{Provider: "custom"},
			env:         map[string]string{},
			wantErr:     true,
			errContains: []string{"research.custom"},
		},
		{
			name:        "unknown provider value",
			cfg:         config.ResearchConfig{Provider: "bing"},
			env:         map[string]string{},
			wantErr:     true,
			errContains: []string{"bing"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Resolve(tt.cfg, fakeEnv(tt.env))

			if tt.wantErr {
				if err == nil {
					t.Fatalf("Resolve() expected error, got nil (provider %v)", got)
				}
				msg := err.Error()
				for _, sub := range tt.errContains {
					if !strings.Contains(msg, sub) {
						t.Errorf("error %q does not contain %q", msg, sub)
					}
				}
				return
			}

			if err != nil {
				t.Fatalf("Resolve() unexpected error: %v", err)
			}
			if got == nil {
				t.Fatal("Resolve() returned nil provider without error")
			}
			if got.Name() != tt.wantProvider {
				t.Errorf("provider Name() = %q, want %q", got.Name(), tt.wantProvider)
			}
			if tt.wantType != "" {
				gotType := typeName(got)
				if gotType != tt.wantType {
					t.Errorf("provider type = %s, want %s", gotType, tt.wantType)
				}
			}
		})
	}
}

// typeName returns the concrete type of a SearchProvider as a string like
// "*research.BraveSearchProvider", using a type switch so the assertion is tied
// to the real concrete types rather than reflection string formatting quirks.
func typeName(p SearchProvider) string {
	switch p.(type) {
	case *BraveSearchProvider:
		return "*research.BraveSearchProvider"
	case *TavilyProvider:
		return "*research.TavilyProvider"
	case *CustomHTTPProvider:
		return "*research.CustomHTTPProvider"
	default:
		return "unknown"
	}
}
