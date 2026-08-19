package permissions

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ethanhinson/fuse/internal/config"
)

// TestFreedom_Bash drives the change-0068 rules-shrink behaviors end-to-end
// through resolveAuto with no classifier wired: VerdictAllow proves a
// deterministic allow; VerdictAsk proves classifier routing (fail-closed to
// the human here); VerdictDeny proves the catastrophic floor.
func TestFreedom_Bash(t *testing.T) {
	cases := []struct {
		desc string
		cmd  string
		want Verdict
	}{
		// The #1 observed real-world denial: loopback health checks.
		{"curl to localhost allows deterministically", "curl -s localhost:3000/health", VerdictAllow},
		{"curl to 127.0.0.1 allows", "curl -sL http://127.0.0.1:4000/v1/models", VerdictAllow},
		{"curl to a public host routes to the classifier", "curl -s https://example.com", VerdictAsk},
		{"curl with unknown flag is unprovable", "curl --config /tmp/c localhost:3000", VerdictAsk},
		{"curl output outside roots is unprovable", "curl -o /etc/x localhost:3000", VerdictAsk},
		{"wget to loopback allows", "wget -q http://localhost:8080/page", VerdictAllow},

		// Process management.
		{"kill by numeric PID allows", "kill 1234", VerdictAllow},
		{"kill with signal flag allows", "kill -9 1234 5678", VerdictAllow},
		{"kill PID 1 does not allow", "kill 1", VerdictAsk},
		{"kill by jobspec is unprovable", "kill %1", VerdictAsk},
		{"pkill routes to the classifier", "pkill -f devserver", VerdictAsk},
		{"killall routes to the classifier", "killall node", VerdictAsk},

		// rm: in-workspace mutation policy, catastrophic floor.
		{"rm of a workspace file allows", "rm build-output.txt", VerdictAllow},
		{"rm -rf of a workspace dir allows", "rm -rf ./build", VerdictAllow},
		{"rm -rf / denies", "rm -rf /", VerdictDeny},
		{"rm -rf ~ denies", "rm -rf ~", VerdictDeny},
		{"rm outside the workspace routes to the classifier", "rm /etc/hosts", VerdictAsk},
		{"tilde write escapes the workspace (bypass regression)", "touch ~/pwned", VerdictAsk},

		// dd shapes.
		{"dd onto a raw device denies", "dd if=img of=/dev/disk0", VerdictDeny},
		{"dd within the workspace allows", "dd if=in.img of=out.img", VerdictAllow},
		{"dd writing outside roots routes to the classifier", "dd if=in.img of=/etc/out", VerdictAsk},

		// git: reset/clean freed; push routed.
		{"git reset is workspace-local now", "git reset --hard HEAD~1", VerdictAllow},
		{"git clean is workspace-local now", "git clean -fd", VerdictAllow},
		{"git push without allow_push routes to the classifier", "git push origin main", VerdictAsk},

		// chmod freed from the terminal list, still scoped.
		{"chmod in-workspace allows", "chmod +x run.sh", VerdictAllow},
		{"chmod outside the workspace routes to the classifier", "chmod 777 /etc/hosts", VerdictAsk},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			root := t.TempDir()
			// Run with cwd inside the workspace so relative operands resolve there.
			canon, err := filepath.EvalSymlinks(root)
			if err != nil {
				t.Fatal(err)
			}
			restore := chdir(t, canon)
			defer restore()
			g := New(autoCfg(config.AutoConfig{}, nil, nil), newTestRegistry("bash"), AlwaysApprove,
				WithWorkspaceRoot(canon))
			got, _, _, _ := g.resolveAuto(context.Background(), "bash", bashArgs(tc.cmd))
			if got != tc.want {
				t.Errorf("resolveAuto(%q) = %v, want %v", tc.cmd, got, tc.want)
			}
		})
	}
}

// TestFreedom_AllowPushEndToEnd proves the trusted allow_push opt-in produces a
// deterministic allow through the whole gate.
func TestFreedom_AllowPushEndToEnd(t *testing.T) {
	g := New(autoCfg(config.AutoConfig{AllowPush: true}, nil, nil), newTestRegistry("bash"), AlwaysApprove,
		WithWorkspaceRoot(t.TempDir()))
	got, layer, _, _ := g.resolveAuto(context.Background(), "bash", bashArgs("git push origin feat/x"))
	if got != VerdictAllow {
		t.Fatalf("allow_push git push = %v (layer %s), want VerdictAllow", got, layer)
	}
}

// TestFreedom_WriteRoots proves the scratch/write-roots surface: bash mutations
// and edit-tool writes inside an extra root allow; symlink escapes from a root
// still fail toward the human.
func TestFreedom_WriteRoots(t *testing.T) {
	root := t.TempDir()
	scratch := t.TempDir()
	canonRoot, _ := filepath.EvalSymlinks(root)
	canonScratch, _ := filepath.EvalSymlinks(scratch)
	restore := chdir(t, canonRoot)
	defer restore()

	g := New(autoCfg(config.AutoConfig{}, nil, nil), newTestRegistry("bash", "write_file"), AlwaysApprove,
		WithWorkspaceRoot(canonRoot), WithWriteRoots([]string{canonScratch}))

	if got, _, _, _ := g.resolveAuto(context.Background(), "bash", bashArgs("mkdir -p "+canonScratch+"/sub")); got != VerdictAllow {
		t.Errorf("mkdir in scratch root = %v, want VerdictAllow", got)
	}
	if got, _, _, _ := g.resolveAuto(context.Background(), "bash", bashArgs("mkdir -p /tmp/not-a-root/x")); got != VerdictAsk {
		t.Errorf("mkdir outside all roots = %v, want VerdictAsk", got)
	}
	if got, layer, _, _ := g.resolveAuto(context.Background(), "write_file", `{"path":"`+canonScratch+`/notes.md"}`); got != VerdictAllow || layer != LayerEditScope {
		t.Errorf("write_file into scratch = %v/%s, want allow/edit_scope", got, layer)
	}

	// A symlink inside the scratch root pointing outside every root must not
	// prove containment.
	outside := t.TempDir()
	link := filepath.Join(canonScratch, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if got, _, _, _ := g.resolveAuto(context.Background(), "bash", bashArgs("touch "+link+"/f")); got != VerdictAsk {
		t.Errorf("write through scratch symlink escape = %v, want VerdictAsk", got)
	}

	// Children inherit the write roots.
	child := g.CloneForChild("c")
	if got, _, _, _ := child.resolveAuto(context.Background(), "bash", bashArgs("touch "+canonScratch+"/child.txt")); got != VerdictAllow {
		t.Errorf("child mutation in scratch root = %v, want VerdictAllow", got)
	}
}

// chdir switches the working directory for a test and returns the restore func.
func chdir(t *testing.T, dir string) func() {
	t.Helper()
	prev, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	return func() { _ = os.Chdir(prev) }
}
