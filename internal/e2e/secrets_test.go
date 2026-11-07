package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const canaryEnv = "E2E_CANARY_SECRET"

func canaryValue(h *harness) string { return "e2e-sentinel-env-" + filepath.Base(h.home) }

// scanSecrets checks stdout, stderr, the trace and everything under ~/.fuse except
// the config file the sentinel key legitimately lives in.
func scanSecrets(t *testing.T, h *harness, r result, trace string, secrets ...string) {
	t.Helper()
	sources := map[string]string{"stdout": r.stdout, "stderr": r.stderr, "trace": readFile(t, trace)}
	configPath := filepath.Join(h.home, ".fuse", "config.yml")
	walkFiles(filepath.Join(h.home, ".fuse"), func(p string, info os.FileInfo) {
		if info.Mode().IsRegular() && p != configPath {
			b, err := os.ReadFile(p)
			if err == nil {
				sources[p] = string(b)
			}
		}
	})
	for name, content := range sources {
		for _, s := range secrets {
			if strings.Contains(content, s) {
				t.Errorf("secret %q found in %s", s, name)
			}
		}
	}
}

func TestSecrets_NotInOutputOrTrace_FailingRequest(t *testing.T) {
	t.Parallel()
	for name, st := range map[string]step{
		"401": status(401, `{"error":"bad key"}`),
		"500": status(500, "boom"),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := newRouter(t, func(*request) step { return st })
			h := newHarness(t, srv)
			h.env[canaryEnv] = canaryValue(h)
			trace := filepath.Join(t.TempDir(), "trace")
			r := h.run("--trace", trace)
			wantCode(t, r, 1)
			scanSecrets(t, h, r, trace, h.key, canaryValue(h))
		})
	}
}

func TestSecrets_NotInOutputOrTrace_ToolRunWithSpill(t *testing.T) {
	t.Parallel()
	srv, h := scripted(t)
	h.hostSandbox()
	h.env[canaryEnv] = canaryValue(h)
	trace := filepath.Join(t.TempDir(), "trace")
	srv.script(
		tool("bash", map[string]string{"command": "env; seq 1 20000"}),
		text("done"),
	)
	r := h.run("--trace", trace, "--verbose", "--approve-all")
	wantCode(t, r, 0)
	scanSecrets(t, h, r, trace, h.key, canaryValue(h))
	for _, req := range srv.requests() {
		wantNotContains(t, string(req.Raw), h.key, "request body")
		wantNotContains(t, string(req.Raw), canaryValue(h), "request body")
	}
}

// The scrub is an allowlist: bash sees PATH, HOME and LANG and nothing else.
func TestSecrets_BashCannotSeeThemThroughEnv(t *testing.T) {
	t.Parallel()
	srv, h := scripted(t)
	h.hostSandbox()
	h.env[canaryEnv] = canaryValue(h)
	out := runOneTool(t, srv, h, "bash", map[string]string{"command": "env; echo ENDENV"}, "--approve-all")
	wantContains(t, out, "ENDENV", "env output")
	wantNotContains(t, out, h.key, "env output")
	wantNotContains(t, out, canaryValue(h), "env output")
	wantNotContains(t, out, canaryEnv, "env output")
	wantNotContains(t, out, "LLM_GATEWAY", "env output")
}

func TestSecrets_FusePermissions_NoGroupOrWorldAccess(t *testing.T) {
	t.Parallel()
	srv, h := scripted(t)
	h.hostSandbox()
	trace := filepath.Join(t.TempDir(), "trace")
	// A spill writes an artifact, a long-lived file holding raw command output.
	srv.script(tool("bash", map[string]string{"command": "seq 1 20000"}), text("done"))
	wantCode(t, h.run("--trace", trace, "--approve-all"), 0)

	var loose []string
	var files int
	walkFiles(filepath.Join(h.home, ".fuse"), func(p string, info os.FileInfo) {
		if info.Mode().IsRegular() {
			files++
		}
		if info.Mode().Perm()&0o077 != 0 && p != filepath.Join(h.home, ".fuse") {
			loose = append(loose, fmt.Sprintf("%s %o", strings.TrimPrefix(p, h.home), info.Mode().Perm()))
		}
	})
	if files < 2 {
		t.Fatalf("only %d files under ~/.fuse: the spill artifact was not produced", files)
	}
	if len(loose) > 0 {
		t.Fatalf("group/world accessible under ~/.fuse:\n%s", strings.Join(loose, "\n"))
	}
	info, err := os.Stat(trace)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("trace mode = %o, want 600", info.Mode().Perm())
	}
}
