package sandbox

import (
	"errors"
	"testing"
)

// containRemoteWorkingDir is the containment algorithm for a sandbox whose
// filesystem is NOT fuse's own. Its whole reason to exist is that
// resolveWorkspace canonicalises against the LOCAL filesystem — EvalSymlinks and
// Stat — which on a remote substrate judges the wrong subject: a "/workspace"
// that exists in the Pod and not in fuse's container refuses every working_dir,
// and a "/workspace" that exists in BOTH decides containment against fuse's tree.
//
// So the table below deliberately names paths NOTHING on this host has. A case
// that only passes because the directory happens to exist locally would be
// asserting the defect.
func TestContainRemoteWorkingDirIsLexicalAndNeverTouchesTheLocalFilesystem(t *testing.T) {
	const mount = "/workspace"

	cases := []struct {
		name       string
		mountRoot  string
		workingDir string
		want       string
		wantErr    error
	}{
		// --- the default: no working_dir runs at the mount root -------------
		{
			name:      "no working dir runs at the mount root",
			mountRoot: mount,
			want:      mount,
		},
		{
			name:       "whitespace-only working dir is the no-working-dir case",
			mountRoot:  mount,
			workingDir: "   ",
			want:       mount,
		},
		{
			name:       "a non-default mount root is honoured",
			mountRoot:  "/sandbox/work",
			workingDir: "",
			want:       "/sandbox/work",
		},

		// --- honoured subpaths, none of which exist on THIS host ------------
		{
			name:       "a relative subpath resolves against the mount root",
			mountRoot:  mount,
			workingDir: "internal/tools",
			want:       "/workspace/internal/tools",
		},
		{
			name:       "an absolute in-mount subpath is honoured",
			mountRoot:  mount,
			workingDir: "/workspace/internal/tools",
			want:       "/workspace/internal/tools",
		},
		{
			name:       "the mount root itself is the bare mount root",
			mountRoot:  mount,
			workingDir: "/workspace",
			want:       mount,
		},
		{
			name:       "a trailing slash is cleaned away",
			mountRoot:  mount,
			workingDir: "/workspace/pkg/",
			want:       "/workspace/pkg",
		},
		{
			name:       "dot is the bare mount root",
			mountRoot:  mount,
			workingDir: ".",
			want:       mount,
		},
		{
			name:       "an interior dot-dot that stays inside is honoured after cleaning",
			mountRoot:  mount,
			workingDir: "a/../b",
			want:       "/workspace/b",
		},
		{
			name:       "a doubled separator is cleaned, not a prefix bypass",
			mountRoot:  mount,
			workingDir: "//workspace//pkg",
			want:       "/workspace/pkg",
		},
		{
			name:       "a subpath under a non-default mount root is rooted there",
			mountRoot:  "/sandbox/work",
			workingDir: "pkg",
			want:       "/sandbox/work/pkg",
		},
		{
			// A traversal that leaves and comes BACK is not an escape: the cleaned
			// path is genuinely inside the mount, so the command runs where the
			// caller asked. Pinned deliberately — the containment question is about
			// the destination, not about the spelling used to reach it.
			name:       "traversal that leaves and returns inside is honoured",
			mountRoot:  mount,
			workingDir: "../../workspace/pkg",
			want:       "/workspace/pkg",
		},
		{
			// A mount root of "/" narrows nothing, and this reports that honestly
			// rather than pretending to contain. A substrate that declared the
			// sandbox root as its workspace has declared no narrowing; the Pod is
			// still the isolation boundary. No substrate in this repo does it.
			name:       "a mount root of slash contains everything in the sandbox",
			mountRoot:  "/",
			workingDir: "/etc",
			want:       "/etc",
		},

		// --- refusals ------------------------------------------------------
		{
			name:       "the parent is refused",
			mountRoot:  mount,
			workingDir: "..",
			wantErr:    ErrWorkingDirRefused,
		},
		{
			name:       "multi-segment traversal out is refused",
			mountRoot:  mount,
			workingDir: "../..",
			wantErr:    ErrWorkingDirRefused,
		},
		{
			name:       "traversal that dips out and back is refused on the cleaned path",
			mountRoot:  mount,
			workingDir: "/workspace/../etc",
			wantErr:    ErrWorkingDirRefused,
		},
		{
			name:       "a deep in-then-out traversal is refused",
			mountRoot:  mount,
			workingDir: "a/b/../../../etc",
			wantErr:    ErrWorkingDirRefused,
		},
		{
			name:       "a trailing dot-dot that lands outside is refused",
			mountRoot:  mount,
			workingDir: "/workspace/pkg/../..",
			wantErr:    ErrWorkingDirRefused,
		},
		{
			name:       "an absolute path outside the mount is refused",
			mountRoot:  mount,
			workingDir: "/etc",
			wantErr:    ErrWorkingDirRefused,
		},
		{
			name:       "the sandbox root is refused",
			mountRoot:  mount,
			workingDir: "/",
			wantErr:    ErrWorkingDirRefused,
		},
		{
			// A SIBLING whose name merely starts with the mount root's text. A
			// prefix test written on raw strings accepts this one.
			name:       "a sibling sharing the mount root's textual prefix is refused",
			mountRoot:  mount,
			workingDir: "/workspaceXXX/pkg",
			wantErr:    ErrWorkingDirRefused,
		},
		{
			name:       "a sibling of a nested mount root sharing its prefix is refused",
			mountRoot:  "/sandbox/work",
			workingDir: "/sandbox/workspace",
			wantErr:    ErrWorkingDirRefused,
		},
		{
			name:       "an embedded NUL is refused",
			mountRoot:  mount,
			workingDir: "pkg\x00/etc",
			wantErr:    ErrWorkingDirRefused,
		},

		// --- no trusted root ----------------------------------------------
		{
			// The substrate reported no mount root at all. There is nothing to
			// contain the request against, and promoting the model's own path to
			// the working directory is the fail-open direction ADR-0044 forbids.
			name:       "an empty mount root with a working dir refuses with ErrNoTrustedRoot",
			workingDir: "/anywhere",
			wantErr:    ErrNoTrustedRoot,
		},
		{
			name:      "an empty mount root with no working dir refuses too",
			wantErr:   ErrNoTrustedRoot,
			mountRoot: "",
		},
		{
			// A mount root the substrate reported as RELATIVE is not a root.
			name:       "a relative mount root is refused",
			mountRoot:  "workspace",
			workingDir: "pkg",
			wantErr:    ErrNoTrustedRoot,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := containRemoteWorkingDir(tc.mountRoot, tc.workingDir)

			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want it to wrap %v", err, tc.wantErr)
				}
				// A refusal must produce nothing usable: a caller that ignored
				// the error must not find a directory to cd into.
				if got != "" {
					t.Fatalf("refusal still returned workdir %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("containRemoteWorkingDir(%q, %q): %v", tc.mountRoot, tc.workingDir, err)
			}
			if got != tc.want {
				t.Fatalf("workdir = %q, want %q", got, tc.want)
			}
		})
	}
}
