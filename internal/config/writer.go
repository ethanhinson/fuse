package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// configPath returns the default user config file path.
func configPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".fuse", "config.yml"), nil
}

// AddMCPServer appends or replaces the named server entry in ~/.fuse/config.yml.
// The write is atomic (write to temp + rename) so fsnotify sees a single event.
func AddMCPServer(srv MCPServerConfig) error {
	path, err := configPath()
	if err != nil {
		return err
	}
	cfg, err := loadOrEmpty(path)
	if err != nil {
		return err
	}

	// Replace if exists, else append.
	found := false
	for i, s := range cfg.MCPServers {
		if s.Name == srv.Name {
			cfg.MCPServers[i] = srv
			found = true
			break
		}
	}
	if !found {
		cfg.MCPServers = append(cfg.MCPServers, srv)
	}
	return writeAtomic(path, cfg)
}

// RemoveMCPServer removes the named server from ~/.fuse/config.yml.
// No-op if the name is not present.
func RemoveMCPServer(name string) error {
	path, err := configPath()
	if err != nil {
		return err
	}
	cfg, err := loadOrEmpty(path)
	if err != nil {
		return err
	}

	out := cfg.MCPServers[:0]
	for _, s := range cfg.MCPServers {
		if s.Name != name {
			out = append(out, s)
		}
	}
	cfg.MCPServers = out
	return writeAtomic(path, cfg)
}

// Path returns the resolved path to ~/.fuse/config.yml.
func Path() string {
	p, _ := configPath()
	return p
}

// loadOrEmpty reads and returns a Config; if the file doesn't exist it returns
// the default config so MCPServers can be appended to an empty list.
func loadOrEmpty(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return Default(), nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("read %s: %w", path, err)
	}
	var raw rawConfig
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", path, err)
	}
	cfg := Default()
	cfg.MCPServers = raw.MCPServers
	cfg.Gateway = raw.Gateway
	return cfg, nil
}

// writeAtomic serialises cfg to a temp file in the same directory and renames
// it into place so the fsnotify watcher in the shell receives one event.
func writeAtomic(path string, cfg Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}

	// Build a minimal YAML document preserving only the MCPServers list.
	// Other keys are intentionally left out — this writer only manages MCP entries.
	type writeDoc struct {
		MCPServers []MCPServerConfig `yaml:"mcp_servers,omitempty"`
	}

	// Round-trip through full Load to get the actual on-disk content, then
	// overlay our changes. Simpler: write the full existing config with updates.
	// Since loadOrEmpty only restores MCPServers and Gateway, we write those.
	doc := struct {
		Gateway    Gateway           `yaml:"gateway,omitempty"`
		MCPServers []MCPServerConfig `yaml:"mcp_servers,omitempty"`
	}{
		Gateway:    cfg.Gateway,
		MCPServers: cfg.MCPServers,
	}

	data, err := yaml.Marshal(doc)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".fuse-config-*.yml")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}
