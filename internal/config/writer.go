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

// SetModel inserts or replaces a single alias entry under the `models` mapping
// in ~/.fuse/config.yml, preserving every other key — including models.default
// and sibling aliases — verbatim. The write is atomic (temp + rename) so the
// shell's fsnotify watcher sees one event.
//
// The `models` mapping is created if absent. Only the one alias's sub-map is
// (re)written; operating on the YAML document rather than the typed Config
// guarantees unmodelled or future keys round-trip unchanged.
func SetModel(alias string, mc ModelConfig) error {
	path, err := configPath()
	if err != nil {
		return err
	}
	return updateDocument(path, func(root *yaml.Node) error {
		models := ensureChildMapping(documentMapping(root), "models")
		if models == nil {
			return fmt.Errorf("config document is not a mapping")
		}
		var valNode yaml.Node
		if err := valNode.Encode(mc); err != nil {
			return fmt.Errorf("encode model %q: %w", alias, err)
		}
		if _, existing := findKey(models, alias); existing != nil {
			*existing = valNode
			return nil
		}
		keyNode := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: alias}
		models.Content = append(models.Content, keyNode, &valNode)
		return nil
	})
}

// RemoveModel deletes a single alias entry from the `models` mapping in
// ~/.fuse/config.yml, preserving every other key. No-op if the alias (or the
// models mapping) is absent.
func RemoveModel(alias string) error {
	path, err := configPath()
	if err != nil {
		return err
	}
	return updateDocument(path, func(root *yaml.Node) error {
		mapping := documentMapping(root)
		if mapping == nil {
			return nil
		}
		_, models := findKey(mapping, "models")
		if models == nil || models.Kind != yaml.MappingNode {
			return nil
		}
		removeKey(models, alias)
		return nil
	})
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
	root, err := loadDocumentNode(path)
	if err != nil {
		return nil, nil, err
	}
	mapping := documentMapping(root)
	if mapping == nil {
		return nil, nil, fmt.Errorf("parse %s: top-level YAML is not a mapping", path)
	}

	var servers []MCPServerConfig
	if _, valNode := findKey(mapping, "mcp_servers"); valNode != nil {
		if err := valNode.Decode(&servers); err != nil {
			return nil, nil, fmt.Errorf("parse %s: mcp_servers: %w", path, err)
		}
	}
	return root, servers, nil
}

// loadDocumentNode reads path and returns the parsed document root. A missing
// or empty file yields a fresh empty-mapping document, and a top level that is
// not a mapping is an error — the shared parse path for every field writer.
func loadDocumentNode(path string) (*yaml.Node, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return newMappingDocument(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	// An empty file unmarshals to a zero node; treat it as an empty mapping.
	if root.Kind == 0 {
		return newMappingDocument(), nil
	}
	if documentMapping(&root) == nil {
		return nil, fmt.Errorf("parse %s: top-level YAML is not a mapping", path)
	}
	return &root, nil
}

// updateDocument loads path as a YAML document, applies mutate to the document
// root (a DocumentNode wrapping a top-level mapping), and writes it back
// atomically. mutate receives the document root; helpers like documentMapping
// unwrap it. Every key mutate does not touch round-trips unchanged.
func updateDocument(path string, mutate func(root *yaml.Node) error) error {
	root, err := loadDocumentNode(path)
	if err != nil {
		return err
	}
	if err := mutate(root); err != nil {
		return err
	}
	return writeDocumentAtomic(path, root)
}

// ensureChildMapping returns the mapping node stored under key in parent,
// creating an empty mapping (and the key) if absent. Returns nil only when
// parent itself is nil, or when key exists but its value is not a mapping.
func ensureChildMapping(parent *yaml.Node, key string) *yaml.Node {
	if parent == nil {
		return nil
	}
	if _, val := findKey(parent, key); val != nil {
		if val.Kind != yaml.MappingNode {
			return nil
		}
		return val
	}
	child := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	keyNode := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}
	parent.Content = append(parent.Content, keyNode, child)
	return child
}

// removeKey drops the key/value pair for key from a mapping node, if present.
func removeKey(mapping *yaml.Node, key string) {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			mapping.Content = append(mapping.Content[:i], mapping.Content[i+2:]...)
			return
		}
	}
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
