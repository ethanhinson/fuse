package mcp

import (
	"context"
	"testing"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/tools"
)

func TestManagerEmpty(t *testing.T) {
	reg := tools.NewRegistry()
	m, err := NewManager(nil, reg)
	if err != nil {
		t.Fatalf("NewManager(nil): %v", err)
	}
	defer m.Close()

	if servers := m.Servers(); len(servers) != 0 {
		t.Errorf("Servers() on empty manager = %v", servers)
	}
	if statuses := m.Status(); len(statuses) != 0 {
		t.Errorf("Status() on empty manager = %v", statuses)
	}
}

func TestManagerStopNoop(t *testing.T) {
	reg := tools.NewRegistry()
	m, err := NewManager(nil, reg)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer m.Close()

	if err := m.Stop("nonexistent"); err != nil {
		t.Errorf("Stop nonexistent: %v", err)
	}
}

func TestManagerAddFailedServer(t *testing.T) {
	reg := tools.NewRegistry()
	m, err := NewManager(nil, reg)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer m.Close()

	// A server with an invalid command will fail to start.
	srv := config.MCPServerConfig{Name: "broken", Transport: "stdio", Command: []string{"/nonexistent/cmd"}}
	if err := m.Add(srv); err == nil {
		t.Error("expected error adding a server with bad command")
	}

	// The broken server entry still appears in Status() with Connected=false.
	found := false
	for _, s := range m.Status() {
		if s.Name == "broken" {
			found = true
			if s.Connected {
				t.Error("broken server should not be Connected")
			}
			if s.Error == "" {
				t.Error("broken server should have an Error string")
			}
		}
	}
	if !found {
		t.Error("broken server should appear in Status()")
	}
}

func TestToolsRegistryUnregister(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(fakeRegistryTool("alpha"))
	reg.Register(fakeRegistryTool("beta"))

	if len(reg.Schemas()) != 2 {
		t.Fatalf("want 2 schemas, got %d", len(reg.Schemas()))
	}

	reg.Unregister("alpha")
	schemas := reg.Schemas()
	if len(schemas) != 1 {
		t.Fatalf("after unregister: want 1 schema, got %d", len(schemas))
	}
	if schemas[0].Name != "beta" {
		t.Errorf("remaining schema = %q, want beta", schemas[0].Name)
	}

	// Unregistering a nonexistent name is a no-op.
	reg.Unregister("nope")
	if len(reg.Schemas()) != 1 {
		t.Error("unregister nonexistent should not change schema count")
	}
}

// fakeRegistryTool satisfies tools.Tool for tests.
type fakeRegistryTool string

func (f fakeRegistryTool) Name() string                                     { return string(f) }
func (f fakeRegistryTool) Description() string                              { return "" }
func (f fakeRegistryTool) Parameters() map[string]any                       { return nil }
func (f fakeRegistryTool) Execute(_ context.Context, _ string) tools.Result { return tools.Result{} }
