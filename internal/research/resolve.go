package research

import (
	"fmt"

	"github.com/ethanhinson/fuse/internal/config"
)

// Environment variables and config keys that supply a provider's credentials or
// endpoint. Named as constants so error messages and lookups stay in sync.
const (
	envBraveAPIKey  = "BRAVE_SEARCH_API_KEY"
	envTavilyAPIKey = "TAVILY_API_KEY"
	customURLKey    = "[research.custom].url"
)

// Resolve selects the SearchProvider to use from cfg and the process
// environment, failing loudly rather than silently degrading. env is injected
// (os.Getenv in real callers) so tests can supply a fake map lookup.
//
// Resolution order:
//
//  1. If cfg.Provider is explicitly set, honor it and require its credentials:
//     "brave" needs BRAVE_SEARCH_API_KEY, "tavily" needs TAVILY_API_KEY,
//     "custom" needs cfg.Custom.URL. A missing credential — or an unknown
//     provider value — is an error naming what to set. An explicit provider
//     always wins over the environment.
//
//  2. Otherwise (empty Provider = auto), pick the first configured backend in
//     priority order: BRAVE_SEARCH_API_KEY, then TAVILY_API_KEY, then
//     cfg.Custom.URL. If none is configured, return a loud error naming every
//     setup path and hinting at research.provider.
//
// Resolution is meant to happen at first use, so a misconfiguration surfaces
// immediately instead of failing silently at query time.
func Resolve(cfg config.ResearchConfig, env func(string) string) (SearchProvider, error) {
	switch cfg.Provider {
	case "brave":
		key := env(envBraveAPIKey)
		if key == "" {
			return nil, fmt.Errorf("research: provider %q selected but %s is not set", cfg.Provider, envBraveAPIKey)
		}
		return NewBraveProvider(key), nil

	case "tavily":
		key := env(envTavilyAPIKey)
		if key == "" {
			return nil, fmt.Errorf("research: provider %q selected but %s is not set", cfg.Provider, envTavilyAPIKey)
		}
		return NewTavilyProvider(key), nil

	case "custom":
		if cfg.Custom.URL == "" {
			return nil, fmt.Errorf("research: provider %q selected but %s is not set", cfg.Provider, customURLKey)
		}
		return NewCustomProvider(cfg.Custom), nil

	case "":
		// auto: first configured backend wins, in priority order.
		if key := env(envBraveAPIKey); key != "" {
			return NewBraveProvider(key), nil
		}
		if key := env(envTavilyAPIKey); key != "" {
			return NewTavilyProvider(key), nil
		}
		if cfg.Custom.URL != "" {
			return NewCustomProvider(cfg.Custom), nil
		}
		return nil, fmt.Errorf(
			"research: no search provider configured. Set one of: %s (Brave), %s (Tavily), or %s (custom endpoint); or set research.provider to force a specific provider",
			envBraveAPIKey, envTavilyAPIKey, customURLKey,
		)

	default:
		return nil, fmt.Errorf("research: unknown provider %q; valid values are \"brave\", \"tavily\", \"custom\", or \"\" (auto)", cfg.Provider)
	}
}
