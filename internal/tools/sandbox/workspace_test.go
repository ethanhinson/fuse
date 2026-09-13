package sandbox

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// resolveWorkspace is the containment algorithm ADR-0044 rests on, promoted out
// of (*containerHandler).workspace so that EVERY substrate — the container
// handler and, from change 0075, the remote adapter — resolves a model-supplied
// working_dir through ONE implementation (change 0075, task 1).
//
// The container handler's own containment tests (container_test.go,
// tenant_root_test.go) are the behaviour-PRESERVATION proof: they exercise this
// algorithm through Exec and must pass unchanged. What the table below adds is
// direct coverage of the one axis those tests structurally cannot reach — a
// mountPoint that is not "/workspace", which is exactly what the remote adapter
// passes (a Pod's MountRoot()).
func TestResolveWorkspaceContainment(t *testing.T) {
	root := t.TempDir()
	// EvalSymlinks the root: on darwin t.TempDir() lives under /var, a symlink
	// to /private/var, and the algorithm compares fully-resolved paths. The
	// container handler canonicalises its root at construction for exactly this
	// reason, so a direct caller must do the same.
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("EvalSymlinks(root): %v", err)
	}

	sub := filepath.Join(root, "internal", "tools")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	file := filepath.Join(root, "README.md")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	escape := filepath.Join(root, "escape")
	if err := os.Symlink("/", escape); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	cases := []struct {
		name       string
		root       string
		workingDir string
		mountPoint string
		wantMount  string
		wantDir    string
		wantErr    error
	}{
		// --- the default: no working_dir still mounts the tree --------------
		{
			name:       "no working dir mounts the root at the mount point",
			root:       root,
			mountPoint: containerWorkspace,
			wantMount:  root,
			wantDir:    containerWorkspace,
		},
		{
			// The axis the container tests cannot reach: the remote adapter
			// passes the sandbox's own MountRoot(), not "/workspace".
			name:       "no working dir honours a non-default mount point",
			root:       root,
			mountPoint: "/sandbox/work",
			wantMount:  root,
			wantDir:    "/sandbox/work",
		},
		{
			name:       "whitespace-only working dir is the no-working-dir case",
			root:       root,
			workingDir: "   ",
			mountPoint: "/sandbox/work",
			wantMount:  root,
			wantDir:    "/sandbox/work",
		},

		// --- honoured subpaths ---------------------------------------------
		{
			name:       "absolute in-tree subpath moves the workdir only",
			root:       root,
			workingDir: sub,
			mountPoint: containerWorkspace,
			wantMount:  root,
			wantDir:    containerWorkspace + "/internal/tools",
		},
		{
			name:       "relative subpath resolves against the root",
			root:       root,
			workingDir: filepath.Join("internal", "tools"),
			mountPoint: containerWorkspace,
			wantMount:  root,
			wantDir:    containerWorkspace + "/internal/tools",
		},
		{
			name:       "a subpath under a non-default mount point is rooted there",
			root:       root,
			workingDir: filepath.Join("internal", "tools"),
			mountPoint: "/sandbox/work",
			wantMount:  root,
			wantDir:    "/sandbox/work/internal/tools",
		},
		{
			name:       "the root itself resolves to the bare mount point",
			root:       root,
			workingDir: root,
			mountPoint: "/sandbox/work",
			wantMount:  root,
			wantDir:    "/sandbox/work",
		},
		{
			name:       "dot resolves to the bare mount point",
			root:       root,
			workingDir: ".",
			mountPoint: containerWorkspace,
			wantMount:  root,
			wantDir:    containerWorkspace,
		},

		// --- refusals ------------------------------------------------------
		{
			name:       "relative traversal out is refused",
			root:       root,
			workingDir: "../..",
			mountPoint: containerWorkspace,
			wantErr:    ErrWorkingDirRefused,
		},
		{
			name:       "the parent relatively is refused",
			root:       root,
			workingDir: "..",
			mountPoint: containerWorkspace,
			wantErr:    ErrWorkingDirRefused,
		},
		{
			name:       "an absolute path outside the root is refused",
			root:       root,
			workingDir: filepath.Dir(root),
			mountPoint: containerWorkspace,
			wantErr:    ErrWorkingDirRefused,
		},
		{
			name:       "the whole host is refused",
			root:       root,
			workingDir: "/",
			mountPoint: "/sandbox/work",
			wantErr:    ErrWorkingDirRefused,
		},
		{
			name:       "a symlink inside the tree pointing out of it is refused",
			root:       root,
			workingDir: escape,
			mountPoint: containerWorkspace,
			wantErr:    ErrWorkingDirRefused,
		},
		{
			name:       "an in-tree path that is not a directory is refused",
			root:       root,
			workingDir: file,
			mountPoint: containerWorkspace,
			wantErr:    ErrWorkingDirRefused,
		},
		{
			name:       "a path that does not exist is refused, never clamped to the root",
			root:       root,
			workingDir: filepath.Join(root, "nope", "missing"),
			mountPoint: containerWorkspace,
			wantErr:    ErrWorkingDirRefused,
		},

		// --- no trusted root ----------------------------------------------
		{
			name:       "no root and no working dir mounts nothing",
			mountPoint: containerWorkspace,
			wantMount:  "",
			wantDir:    containerWorkspace,
		},
		{
			// The one refusal that must NOT be ErrWorkingDirRefused: with no
			// trusted root there is nothing to contain the request against, and
			// promoting the model's path to a mount source is the fail-open
			// direction ADR-0044 forbids.
			name:       "no root with a working dir refuses with ErrNoTrustedRoot",
			workingDir: "/Users/someone",
			mountPoint: containerWorkspace,
			wantErr:    ErrNoTrustedRoot,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mount, workdir, err := resolveWorkspace(tc.root, tc.workingDir, tc.mountPoint)

			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want it to wrap %v", err, tc.wantErr)
				}
				// A refusal must produce NOTHING usable: an argv builder that
				// ignored the error must not find a mount to use.
				if mount != "" || workdir != "" {
					t.Fatalf("refusal still returned mount=%q workdir=%q", mount, workdir)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveWorkspace: %v", err)
			}
			if mount != tc.wantMount {
				t.Fatalf("mount = %q, want %q", mount, tc.wantMount)
			}
			if workdir != tc.wantDir {
				t.Fatalf("workdir = %q, want %q", workdir, tc.wantDir)
			}
		})
	}
}

// The container handler must not carry a second copy of the algorithm: its
// method is a delegation, so the two agree by construction rather than by
// convention. This pins that agreement — if someone re-inlines the algorithm
// into the method, a later divergence here is what catches it.
func TestContainerHandlerWorkspaceDelegatesToResolveWorkspace(t *testing.T) {
	root := t.TempDir()
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	sub := filepath.Join(root, "pkg")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	h := &containerHandler{}

	for _, workingDir := range []string{"", "pkg", sub, "..", "/", filepath.Join(root, "gone")} {
		gotMount, gotDir, gotErr := h.workspace(root, workingDir)
		wantMount, wantDir, wantErr := resolveWorkspace(root, workingDir, containerWorkspace)

		if gotMount != wantMount || gotDir != wantDir {
			t.Fatalf("workspace(%q) = (%q, %q), resolveWorkspace = (%q, %q)",
				workingDir, gotMount, gotDir, wantMount, wantDir)
		}
		switch {
		case (gotErr == nil) != (wantErr == nil):
			t.Fatalf("workspace(%q) err = %v, resolveWorkspace err = %v", workingDir, gotErr, wantErr)
		case gotErr != nil && gotErr.Error() != wantErr.Error():
			t.Fatalf("workspace(%q) err = %q, resolveWorkspace err = %q", workingDir, gotErr, wantErr)
		}
	}
}
