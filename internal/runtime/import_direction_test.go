package runtime

import (
	"os/exec"
	"strings"
	"testing"
)

const modPrefix = "github.com/ethanhinson/fuse/"

// goListDeps runs `go list -deps <pkgs...>` and returns the module-local import
// paths (stripped of the module prefix). It skips the whole test when `go` is not on
// PATH so the guard only runs where the toolchain is available.
func goListDeps(t *testing.T, pkgs ...string) []string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH; skipping dependency-graph guard")
	}
	args := append([]string{"list", "-deps"}, pkgs...)
	out, err := exec.Command("go", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps %v: %v\n%s", pkgs, err, out)
	}
	var deps []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, modPrefix) {
			deps = append(deps, strings.TrimPrefix(line, modPrefix))
		}
	}
	return deps
}

func containsDep(deps []string, want string) bool {
	for _, d := range deps {
		if d == want {
			return true
		}
	}
	return false
}

// TestImportDirection_EngineDoesNotDependOnRuntime enforces the load-bearing import
// direction (learning break-import-cycle-with-agent-free-subpackage): internal/agent
// and internal/tools must NEVER depend on internal/runtime — the seam imports THEM,
// not the reverse, or an import cycle forms.
func TestImportDirection_EngineDoesNotDependOnRuntime(t *testing.T) {
	deps := goListDeps(t,
		"github.com/ethanhinson/fuse/internal/agent",
		"github.com/ethanhinson/fuse/internal/tools",
	)
	if containsDep(deps, "internal/runtime") {
		t.Fatalf("internal/agent or internal/tools depends on internal/runtime — that forms an import cycle")
	}
}

// TestImportDirection_RuntimeComposesEngine proves the seam actually composes the
// engine: internal/runtime's transitive deps DO include internal/agent and
// internal/event (it wraps them). This is the positive half of the direction guard.
func TestImportDirection_RuntimeComposesEngine(t *testing.T) {
	deps := goListDeps(t, "github.com/ethanhinson/fuse/internal/runtime")
	if !containsDep(deps, "internal/agent") {
		t.Errorf("internal/runtime does not compose internal/agent")
	}
	if !containsDep(deps, "internal/event") {
		t.Errorf("internal/runtime does not compose internal/event")
	}
}
