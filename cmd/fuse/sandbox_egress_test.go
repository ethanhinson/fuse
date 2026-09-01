package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"

	"github.com/ethanhinson/fuse/internal/config"
	"strings"
	"testing"
)

// writeEgressConfig writes the off-switch file the sandbox loader reads.
func writeEgressConfig(t *testing.T, root, body string) {
	t.Helper()
	dir := filepath.Join(root, ".fuse")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir .fuse: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sandbox.local.yml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write sandbox.local.yml: %v", err)
	}
}

// withForwarderCandidates swaps the artifact-discovery seam for the test and
// restores it afterwards. The seam exists because discovery is anchored to the
// running executable, which under `go test` is the test binary.
func withForwarderCandidates(t *testing.T, paths []string) {
	t.Helper()
	prev := egressForwarderCandidates
	egressForwarderCandidates = func() []string { return paths }
	t.Cleanup(func() { egressForwarderCandidates = prev })
}

// fakeForwarder creates an executable regular file standing in for the
// cross-compiled dist/fuse-egress-forward-linux-<arch> artifact.
func fakeForwarder(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "fuse-egress-forward-linux-"+runtime.GOARCH)
	if err := os.WriteFile(path, []byte("#!/bin/true\n"), 0o755); err != nil {
		t.Fatalf("write forwarder: %v", err)
	}
	return path
}

const enforceConfig = "egress:\n  mode: enforce\n  allow:\n    - host: api.example.com\n      port: 443\n"

// TestEgressEnforceWithoutDatapathWarnsLoudly is the finding this test file
// exists for: `egress.mode: enforce` with no forwarder artifact on the machine
// is a TOTAL network blackout for every contained command, and it must never be
// silent. The composition root refuses to open a datapath (fail-closed) AND says
// so, in the same place the UNCONTAINED notice is emitted.
func TestEgressEnforceWithoutDatapathWarnsLoudly(t *testing.T) {
	root := t.TempDir()
	writeEgressConfig(t, root, enforceConfig)
	withForwarderCandidates(t, []string{filepath.Join(t.TempDir(), "absent")})

	var buf bytes.Buffer
	proxy, forwarder, _ := resolveEgressDatapath(config.Config{}, root, &buf)

	if proxy != nil {
		t.Errorf("proxy = %v, want nil — no datapath may be opened without a forwarder", proxy)
	}
	if forwarder != "" {
		t.Errorf("forwarder = %q, want \"\"", forwarder)
	}
	out := buf.String()
	for _, want := range []string{"EGRESS ENFORCED", "NO DATAPATH", "make egress-forwarder"} {
		if !strings.Contains(out, want) {
			t.Errorf("diagnostic %q does not contain %q", out, want)
		}
	}
}

// TestEgressEnforceWiresDatapath proves the advertised knob is not inert: with
// the artifact present the composition root builds a real Proxy and hands back
// the forwarder path, so `egress.allow` has a way to be reached.
func TestEgressEnforceWiresDatapath(t *testing.T) {
	root := t.TempDir()
	writeEgressConfig(t, root, enforceConfig)
	artifact := fakeForwarder(t, t.TempDir())
	withForwarderCandidates(t, []string{artifact})

	var buf bytes.Buffer
	proxy, forwarder, _ := resolveEgressDatapath(config.Config{}, root, &buf)

	if proxy == nil {
		t.Fatalf("proxy = nil, want a live proxy; diagnostics: %s", buf.String())
	}
	t.Cleanup(func() { _ = proxy.Close() })
	if forwarder != artifact {
		t.Errorf("forwarder = %q, want %q", forwarder, artifact)
	}
	if info, err := os.Stat(proxy.Root()); err != nil || !info.IsDir() {
		t.Errorf("proxy root %q: stat = %v, %v; want a directory", proxy.Root(), info, err)
	}
	if out := buf.String(); strings.Contains(out, "NO DATAPATH") {
		t.Errorf("blackout diagnostic emitted for a WIRED datapath: %s", out)
	}
	// The operator still learns enforcement is on, and through what.
	if out := buf.String(); !strings.Contains(out, "egress") || !strings.Contains(out, artifact) {
		t.Errorf("wired-datapath notice %q does not name the posture and the forwarder %q", out, artifact)
	}
	root0 := proxy.Root()
	if err := proxy.Close(); err != nil {
		t.Errorf("proxy.Close() = %v", err)
	}
	if _, err := os.Stat(root0); !os.IsNotExist(err) {
		t.Errorf("after Close, proxy root %q still exists (err = %v)", root0, err)
	}
}

// TestEgressAllowAllBuildsNoProxy: the default posture must cost nothing and say
// nothing — no proxy, no temp directory, no diagnostic.
func TestEgressAllowAllBuildsNoProxy(t *testing.T) {
	root := t.TempDir()
	writeEgressConfig(t, root, "contained: true\n")
	artifact := fakeForwarder(t, t.TempDir())
	withForwarderCandidates(t, []string{artifact})

	var buf bytes.Buffer
	proxy, forwarder, _ := resolveEgressDatapath(config.Config{}, root, &buf)

	if proxy != nil {
		_ = proxy.Close()
		t.Errorf("proxy = %v, want nil under allow-all", proxy)
	}
	if forwarder != "" {
		t.Errorf("forwarder = %q, want \"\" under allow-all", forwarder)
	}
	if out := buf.String(); out != "" {
		t.Errorf("allow-all emitted %q, want nothing", out)
	}
}

// TestForwarderCandidatesAreArchNamedAndExecutableRelative pins the two
// distribution locations an operator can use, and that both are named for the
// architecture rather than being a single arch-blind path.
func TestForwarderCandidatesAreArchNamedAndExecutableRelative(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("os.Executable: %v", err)
	}
	// Anchored to the RESOLVED executable: a fuse reached through a package
	// manager's bin/ shim must look beside the real binary, where the artifact was
	// installed — not beside the symlink.
	if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
		exe = resolved
	}
	exeDir := filepath.Dir(exe)

	got := defaultEgressForwarderCandidates()
	if len(got) == 0 {
		t.Fatal("defaultEgressForwarderCandidates() is empty")
	}
	wantName := "fuse-egress-forward-linux-" + runtime.GOARCH
	for _, p := range got {
		if filepath.Base(p) != wantName {
			t.Errorf("candidate %q: base = %q, want %q", p, filepath.Base(p), wantName)
		}
		if !strings.HasPrefix(p, exeDir+string(filepath.Separator)) {
			t.Errorf("candidate %q is not anchored to the executable directory %q", p, exeDir)
		}
	}
	if want := filepath.Join(exeDir, "dist", wantName); got[0] != want {
		t.Errorf("first candidate = %q, want %q", got[0], want)
	}
}

// TestForwarderCandidateMustBeExecutableRegularFile: a directory, or a
// non-executable file, is not a forwarder. Accepting either would mount
// something that cannot run as the container's entry command, turning every
// bash call into an exec failure — strictly worse than the deny-all floor.
func TestForwarderCandidateMustBeExecutableRegularFile(t *testing.T) {
	dir := t.TempDir()
	notExec := filepath.Join(dir, "not-exec")
	if err := os.WriteFile(notExec, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	subdir := filepath.Join(dir, "a-directory")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	good := fakeForwarder(t, dir)

	for name, cands := range map[string][]string{
		"non-executable": {notExec},
		"directory":      {subdir},
		"absent":         {filepath.Join(dir, "nope")},
	} {
		if got := firstForwarderArtifact(cands); got != "" {
			t.Errorf("%s: firstForwarderArtifact = %q, want \"\"", name, got)
		}
	}
	if got := firstForwarderArtifact([]string{notExec, subdir, good}); got != good {
		t.Errorf("firstForwarderArtifact = %q, want %q", got, good)
	}
}

// TestEgressNoticeCorrectedOnHostSubstrate: the datapath is resolved before the
// substrate is known, so an off-switch that selects the HOST must not leave
// "egress ENFORCED" standing as the last word — on the host there is no
// --network none floor and no allowlist, and the operator has to be told.
func TestEgressNoticeCorrectedOnHostSubstrate(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	writeEgressConfig(t, root, "contained: false\nhandler: host\n"+enforceConfig)
	withForwarderCandidates(t, []string{fakeForwarder(t, t.TempDir())})

	var buf bytes.Buffer
	svc, closeFn := newSandboxService(config.Config{}, false, &buf)
	t.Cleanup(closeFn)

	if svc == nil || svc.Contained() {
		t.Fatalf("want the host substrate; svc = %v", svc)
	}
	out := buf.String()
	if !strings.Contains(out, "UNCONTAINED") {
		t.Errorf("output %q lost the UNCONTAINED notice", out)
	}
	if !strings.Contains(out, "does NOT apply on the host substrate") {
		t.Errorf("output %q leaves the egress-enforced claim uncorrected on the host substrate", out)
	}
}

// TestNewSandboxServiceReturnsCloser: the Proxy is owned by the composition
// root, so every entry point gets a closer it can defer. It is non-nil even when
// no proxy was built, so no caller has to nil-check it.
func TestNewSandboxServiceReturnsCloser(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	writeEgressConfig(t, root, enforceConfig)
	artifact := fakeForwarder(t, t.TempDir())
	withForwarderCandidates(t, []string{artifact})

	var buf bytes.Buffer
	_, closeFn := newSandboxService(config.Config{}, false, &buf)
	if closeFn == nil {
		t.Fatal("newSandboxService returned a nil closer")
	}
	// The composition root resolved the datapath, rather than leaving the knob
	// inert: enforcement plus the artifact yields the wired notice naming both.
	if out := buf.String(); !strings.Contains(out, "datapath via "+artifact) {
		t.Errorf("newSandboxService output %q does not report the wired datapath %q", out, artifact)
	}
	closeFn()
	closeFn() // idempotent: an entry point may defer it beside an early return
}
