package tools

import (
	"context"
	"testing"
)

// spawnFakeTool is a minimal tool whose name is "spawn_agent".
type spawnFakeTool struct{}

func (spawnFakeTool) Name() string               { return "spawn_agent" }
func (spawnFakeTool) Description() string        { return "spawn agent stub" }
func (spawnFakeTool) Parameters() map[string]any { return map[string]any{"type": "object"} }
func (spawnFakeTool) Execute(context.Context, string) Result {
	return Result{Output: "spawned"}
}

// TestRegistrySubset_SpawnAgentNotForced pins the change 0034 folded-in fix:
// a subset that omits spawn_agent yields a registry WITHOUT it. Before 0034,
// Subset force-included spawn_agent unconditionally, which made a parent
// structurally unable to withhold the spawn tool from a child — the exact
// enforcement gap workflow worker allowlists (and freeform tools subsets) close.
func TestRegistrySubset_SpawnAgentNotForced(t *testing.T) {
	r := NewRegistry()
	r.Register(fakeTool{name: "bash"})
	r.Register(spawnFakeTool{})

	sub, unknown := r.Subset([]string{"bash"})
	if len(unknown) != 0 {
		t.Errorf("unexpected unknown tools: %v", unknown)
	}

	schemas := sub.Schemas()
	names := make(map[string]bool, len(schemas))
	for _, s := range schemas {
		names[s.Name] = true
	}
	if !names["bash"] {
		t.Error("subset should contain 'bash'")
	}
	if names["spawn_agent"] {
		t.Error("spawn_agent must NOT be force-included when a subset omits it (change 0034)")
	}
}

// A subset that explicitly names spawn_agent still gets it.
func TestRegistrySubset_SpawnAgentWhenNamed(t *testing.T) {
	r := NewRegistry()
	r.Register(fakeTool{name: "bash"})
	r.Register(spawnFakeTool{})

	sub, unknown := r.Subset([]string{"bash", "spawn_agent"})
	if len(unknown) != 0 {
		t.Errorf("unexpected unknown tools: %v", unknown)
	}
	names := make(map[string]bool)
	for _, s := range sub.Schemas() {
		names[s.Name] = true
	}
	if !names["spawn_agent"] {
		t.Error("spawn_agent should be present when explicitly named in the subset")
	}
}

func TestRegistrySubset_UnknownDropped(t *testing.T) {
	r := NewRegistry()
	r.Register(fakeTool{name: "bash"})
	// spawn_agent is NOT registered here.

	sub, unknown := r.Subset([]string{"bash", "unknown"})
	if len(unknown) != 1 || unknown[0] != "unknown" {
		t.Errorf("expected [unknown] in second return, got %v", unknown)
	}

	schemas := sub.Schemas()
	if len(schemas) != 1 {
		t.Fatalf("expected 1 tool in subset (only bash; spawn_agent absent), got %d", len(schemas))
	}
	if schemas[0].Name != "bash" {
		t.Errorf("expected 'bash', got %q", schemas[0].Name)
	}
}

func TestRegistrySubset_EmptyNames(t *testing.T) {
	t.Run("empty_names_yields_empty_subset", func(t *testing.T) {
		// With force-include removed (change 0034), Subset(nil) selects nothing.
		// Empty-names inheritance for children is handled by childToolRegistry's
		// Clone() path, not by Subset.
		r := NewRegistry()
		r.Register(spawnFakeTool{})

		sub, unknown := r.Subset(nil)
		if len(unknown) != 0 {
			t.Errorf("unexpected unknowns: %v", unknown)
		}
		if len(sub.Schemas()) != 0 {
			t.Errorf("expected empty subset for nil names, got %v", sub.Schemas())
		}
	})

	t.Run("spawn_agent_absent", func(t *testing.T) {
		r := NewRegistry()
		// no tools at all
		sub, unknown := r.Subset(nil)
		if len(unknown) != 0 {
			t.Errorf("unexpected unknowns: %v", unknown)
		}
		if len(sub.Schemas()) != 0 {
			t.Errorf("expected empty subset, got %v", sub.Schemas())
		}
	})
}
