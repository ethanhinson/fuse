package permissions

import (
	"context"
	"testing"

	"github.com/ethanhinson/fuse/internal/config"
)

func TestApprovalCacheClone(t *testing.T) {
	parent := newApprovalCache()
	parent.Allow("bash", `{"command":"ls"}`)

	clone := parent.Clone()

	// Clone must contain the entry that was in parent before cloning.
	if !clone.Check("bash", `{"command":"ls"}`) {
		t.Error("clone should contain the entry that existed in parent before clone")
	}

	// Adding to parent after cloning must not appear in clone.
	parent.Allow("bash", `{"command":"rm -rf /"}`)
	if clone.Check("bash", `{"command":"rm -rf /"}`) {
		t.Error("post-clone parent addition must not propagate to clone")
	}

	// Adding to clone must not appear in parent.
	clone.Allow("read_file", `{}`)
	if parent.Check("read_file", `{}`) {
		t.Error("clone addition must not propagate back to parent")
	}
}

// TestClassifierCloneForChildPropagatesWorkspaceContext pins that a child gate's
// classifier inherits the parent's workspace context (#0069 D1b): the context
// line is per-session configuration, not per-verdict state, so a child must not
// silently drop it.
func TestClassifierCloneForChildPropagatesWorkspaceContext(t *testing.T) {
	stub := &stubCompleter{}
	parent := newTestClassifier(t, stub).WithWorkspaceContext("/ws", "/ws/.scratch")

	child := parent.cloneForChild()
	if child == nil {
		t.Fatal("cloneForChild must return a non-nil classifier")
	}
	if child.workspaceRoot != "/ws" {
		t.Errorf("child workspaceRoot = %q, want %q", child.workspaceRoot, "/ws")
	}
	if child.scratchDir != "/ws/.scratch" {
		t.Errorf("child scratchDir = %q, want %q", child.scratchDir, "/ws/.scratch")
	}
}

func TestPermissionGateCloneForChild(t *testing.T) {
	reg := newTestRegistry("bash")
	cfg := config.PermissionsConfig{Mode: "off"}
	parent := New(cfg, reg, AlwaysApprove)

	child := parent.CloneForChild("worker")
	if child == nil {
		t.Fatal("CloneForChild must return a non-nil gate")
	}

	// The child's approve func must be non-nil and functional — call Execute on
	// a known tool and verify it delegates to the inner registry.
	res := child.Execute(context.Background(), "bash", `{"command":"echo hi"}`)
	if res.IsError {
		t.Errorf("child gate Execute returned error: %s", res.Output)
	}
}
