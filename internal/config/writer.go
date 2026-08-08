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
// Every other key in the file is preserved verbatim — only the mcp_servers list
// is rewritten. The write is atomic (write to temp + rename) so fsnotify sees a
// single event.
func AddMCPServer(srv MCPServerConfig) error {
	path, err := configPath()
	if err != nil {
		return err
	}
	return updateServers(path, func(servers []MCPServerConfig) []MCPServerConfig {
		for i, s := range servers {
			if s.Name == srv.Name {
				servers[i] = srv
				return servers
			}
		}
		return append(servers, srv)
	})
}

// RemoveMCPServer removes the named server from ~/.fuse/config.yml, preserving
// every other key. No-op if the name is not present.
func RemoveMCPServer(name string) error {
	path, err := configPath()
	if err != nil {
		return err
	}
	return updateServers(path, func(servers []MCPServerConfig) []MCPServerConfig {
		out := servers[:0]
		for _, s := range servers {
			if s.Name != name {
				out = append(out, s)
			}
		}
		return out
	})
}

// Path returns the resolved path to ~/.fuse/config.yml.
func Path() string {
	p, _ := configPath()
	return p
}

// updateServers loads the config file as a YAML document, applies mutate to the
// current mcp_servers list, splices the result back into the document (leaving
// every other key untouched), and writes it atomically. When the file does not
// exist, a fresh document is created from an empty server list.
//
// Operating on the *document* rather than the typed Config guarantees no data
// loss: keys the Config struct does not model (or future keys) round-trip
// unchanged, and only the mcp_servers node is replaced.
func updateServers(path string, mutate func([]MCPServerConfig) []MCPServerConfig) error {
	doc, current, err := loadDocument(path)
	if err != nil {
		return err
	}

	updated := mutate(current)
	if updated == nil {
		updated = []MCPServerConfig{}
	}

	if err := setMCPServers(doc, updated); err != nil {
		return err
	}
	return writeDocumentAtomic(path, doc)
}

// loadDocument reads path and returns the parsed document root (a mapping node)
// together with the decoded mcp_servers list. A missing file yields an empty
// mapping document and an empty list.
func loadDocument(path string) (*yaml.Node, []MCPServerConfig, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return newMappingDocument(), nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", path, err)
	}

	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, nil, fmt.Errorf("parse %s: %w", path, err)
	}
	// An empty file unmarshals to a zero node; treat it as an empty mapping.
	if root.Kind == 0 {
		return newMappingDocument(), nil, nil
	}

	mapping := documentMapping(&root)
	if mapping == nil {
		return nil, nil, fmt.Errorf("parse %s: top-level YAML is not a mapping", path)
	}

	var servers []MCPServerConfig
	if _, valNode := findKey(mapping, "mcp_servers"); valNode != nil {
		if err := valNode.Decode(&servers); err != nil {
			return nil, nil, fmt.Errorf("parse %s: mcp_servers: %w", path, err)
		}
	}
	return &root, servers, nil
}

// setMCPServers encodes servers and replaces (or inserts) the mcp_servers key in
// the document's top-level mapping, leaving all other keys in place.
func setMCPServers(root *yaml.Node, servers []MCPServerConfig) error {
	mapping := documentMapping(root)
	if mapping == nil {
		return fmt.Errorf("config document is not a mapping")
	}

	var valNode yaml.Node
	if err := valNode.Encode(servers); err != nil {
		return fmt.Errorf("encode mcp_servers: %w", err)
	}

	if _, existing := findKey(mapping, "mcp_servers"); existing != nil {
		*existing = valNode
		return nil
	}
	keyNode := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "mcp_servers"}
	mapping.Content = append(mapping.Content, keyNode, &valNode)
	return nil
}

// documentMapping returns the top-level mapping node for a parsed document,
// unwrapping the DocumentNode wrapper produced by yaml.Unmarshal into *yaml.Node.
// Returns nil if the top level is not a mapping.
func documentMapping(root *yaml.Node) *yaml.Node {
	n := root
	if n.Kind == yaml.DocumentNode {
		if len(n.Content) == 0 {
			return nil
		}
		n = n.Content[0]
	}
	if n.Kind != yaml.MappingNode {
		return nil
	}
	return n
}

// findKey scans a mapping node's key/value pairs for key and returns the key
// node and its value node, or (nil, nil) if absent.
func findKey(mapping *yaml.Node, key string) (*yaml.Node, *yaml.Node) {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i], mapping.Content[i+1]
		}
	}
	return nil, nil
}

// newMappingDocument builds an empty document whose root is an empty mapping,
// ready for setMCPServers to populate.
func newMappingDocument() *yaml.Node {
	return &yaml.Node{
		Kind:    yaml.DocumentNode,
		Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}},
	}
}

// writeDocumentAtomic serialises the document to a temp file in the same
// directory and renames it into place so the fsnotify watcher in the shell
// receives one event.
func writeDocumentAtomic(path string, doc *yaml.Node) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
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
