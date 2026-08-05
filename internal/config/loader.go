package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Load resolves configuration by starting from Default(), merging
// ~/.fuse/config.yml if present, then .fuse.local.yml in the CWD if present,
// and finally applying LLM_GATEWAY_URL / LLM_GATEWAY_KEY env overrides.
func Load() (Config, error) {
	c := Default()

	home, err := os.UserHomeDir()
	if err == nil {
		if err := mergeFile(&c, filepath.Join(home, ".fuse", "config.yml")); err != nil {
			return c, err
		}
	}
	if err := mergeFile(&c, ".fuse.local.yml"); err != nil {
		return c, err
	}

	if v := os.Getenv("LLM_GATEWAY_URL"); v != "" {
		c.Gateway.URL = v
	}
	if v := os.Getenv("LLM_GATEWAY_KEY"); v != "" {
		c.Gateway.Key = v
	}
	return c, nil
}

// mergeFile applies a YAML file onto c if the file exists. A missing file is
// not an error; a malformed file is.
func mergeFile(c *Config, path string) error {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read config %s: %w", path, err)
	}

	var raw rawConfig
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("parse config %s: %w", path, err)
	}

	if raw.Gateway.URL != "" {
		c.Gateway.URL = raw.Gateway.URL
	}
	if raw.Gateway.Key != "" {
		c.Gateway.Key = raw.Gateway.Key
	}
	if raw.MaxTurns != 0 {
		c.MaxTurns = raw.MaxTurns
	}
	if raw.MaxTokens != 0 {
		c.MaxTokens = raw.MaxTokens
	}
	if len(raw.SkillPaths) > 0 {
		c.SkillPaths = raw.SkillPaths
	}
	if raw.Permissions.Mode != "" {
		c.Permissions.Mode = raw.Permissions.Mode
	}
	if !raw.Permissions.SessionAllow {
		c.Permissions.SessionAllow = false
	}
	if len(raw.Permissions.AutoApprove) > 0 {
		c.Permissions.AutoApprove = raw.Permissions.AutoApprove
	}
	if len(raw.Permissions.AlwaysPrompt) > 0 {
		c.Permissions.AlwaysPrompt = raw.Permissions.AlwaysPrompt
	}
	if len(raw.Permissions.Disabled) > 0 {
		c.Permissions.Disabled = raw.Permissions.Disabled
	}
	if len(raw.MCPServers) > 0 {
		c.MCPServers = raw.MCPServers
	}

	if raw.Research.Provider != "" {
		c.Research.Provider = raw.Research.Provider
	}
	if raw.Research.MaxQueries != 0 {
		c.Research.MaxQueries = raw.Research.MaxQueries
	}
	if raw.Research.MaxResults != 0 {
		c.Research.MaxResults = raw.Research.MaxResults
	}
	if raw.Research.MaxContentKB != 0 {
		c.Research.MaxContentKB = raw.Research.MaxContentKB
	}
	// RespectRobots defaults true, so a plain zero-check cannot tell "unset"
	// from "false" when a research block is present; a pointer lets an omitted
	// key keep the default while `respect_robots: false` still takes effect.
	if raw.Research.RespectRobots != nil {
		c.Research.RespectRobots = *raw.Research.RespectRobots
	}
	if raw.Research.Custom.URL != "" {
		c.Research.Custom.URL = raw.Research.Custom.URL
	}
	if len(raw.Research.Custom.Headers) > 0 {
		c.Research.Custom.Headers = raw.Research.Custom.Headers
	}
	if raw.Research.Custom.ResultsPath != "" {
		c.Research.Custom.ResultsPath = raw.Research.Custom.ResultsPath
	}
	if raw.Research.Custom.TitleField != "" {
		c.Research.Custom.TitleField = raw.Research.Custom.TitleField
	}
	if raw.Research.Custom.URLField != "" {
		c.Research.Custom.URLField = raw.Research.Custom.URLField
	}
	if raw.Research.Custom.SnippetField != "" {
		c.Research.Custom.SnippetField = raw.Research.Custom.SnippetField
	}

	// The `models` map holds a `default` string alongside model entries.
	for k, v := range raw.Models {
		if k == "default" {
			if s, ok := v.(string); ok {
				c.Models.Default = s
			}
			continue
		}
		mc, err := decodeModelEntry(v)
		if err != nil {
			return fmt.Errorf("parse model %q in %s: %w", k, path, err)
		}
		if c.Models.Entries == nil {
			c.Models.Entries = map[string]ModelConfig{}
		}
		c.Models.Entries[k] = mc
	}
	return nil
}

// decodeModelEntry converts a single YAML model node into a ModelConfig.
func decodeModelEntry(v interface{}) (ModelConfig, error) {
	// Round-trip through YAML to reuse struct tags without a custom decoder.
	data, err := yaml.Marshal(v)
	if err != nil {
		return ModelConfig{}, err
	}
	var mc ModelConfig
	if err := yaml.Unmarshal(data, &mc); err != nil {
		return ModelConfig{}, err
	}
	return mc, nil
}
