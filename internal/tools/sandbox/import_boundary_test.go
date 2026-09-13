package sandbox

import (
	"os/exec"
	"strings"
	"testing"
)

// TestSandboxDoesNotImportControlPlaneSDK is the structural assertion behind
// ADR-0058 rule 1's "the dependency runs one way only".
//
// WHY THIS IS A TEST AND NOT A COMMENT. The remote seam's whole reason for
// existing in package sandbox — rather than beside its Kubernetes implementation
// — is that this package must stay a LEAF that the bash tool can depend on. An
// import of client-go here would be silently correct to the compiler and would
// carry ~40 transitive modules, a scheme registry, and a rest-config loader into
// every binary and every test that touches the bash tool. It would also invert
// the seam: once sandbox knows about Pods, the adapter stops being an adapter.
//
// Nothing about that is visible in a diff that adds one import line, so the
// boundary is asserted mechanically. The assertion covers the TRANSITIVE
// closure, because an intermediate package that pulls client-go in breaks the
// property just as thoroughly as a direct import does.
//
// The list is deliberately the whole k8s.io/ prefix rather than client-go alone:
// k8s.io/api and k8s.io/apimachinery are the same boundary (they are the types a
// control-plane object is spelled in), and admitting them "just for a type" is
// how the boundary erodes.
func TestSandboxDoesNotImportControlPlaneSDK(t *testing.T) {
	const self = "github.com/ethanhinson/fuse/internal/tools/sandbox"

	// The transitive import closure of the NON-test build of this package. Test
	// files are excluded on purpose: this very file is in package sandbox, and a
	// test's imports do not reach a production binary.
	out, err := exec.Command("go", "list", "-deps", self).CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps %s: %v\n%s", self, err, out)
	}

	forbidden := []string{"k8s.io/"}
	for _, dep := range strings.Fields(string(out)) {
		for _, bad := range forbidden {
			if strings.HasPrefix(dep, bad) {
				t.Errorf("package sandbox transitively imports %q.\n"+
					"Every control-plane SDK import must stay inside %s/kubernetes:\n"+
					"this package is the leaf the bash tool depends on, and the remote seam\n"+
					"exists precisely so the implementation imports sandbox and never the reverse.",
					dep, self)
			}
		}
	}
}
