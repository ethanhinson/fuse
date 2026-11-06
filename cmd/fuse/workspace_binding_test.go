package main

import (
	"context"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	harnessv1 "github.com/ethanhinson/fuse/harness/v1"
	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/observe"
	"github.com/ethanhinson/fuse/internal/permissions"
	"github.com/ethanhinson/fuse/internal/runtime"
	"github.com/ethanhinson/fuse/internal/skills"
	"github.com/ethanhinson/fuse/internal/tools"
	"github.com/ethanhinson/fuse/internal/tools/sandbox"
)

// TestLoopServerPromptHasNoHostPaths is acceptance criterion 5, which was marked
// met on the strength of golden files that replace HOME before comparing. It
// looks at the raw composed prompt on the production wiring.
func TestLoopServerPromptHasNoHostPaths(t *testing.T) {
	cwd, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(cwd)
	rt, gw, _ := harnessServer(t, harnessOperator)
	turns := uint32(1)
	sys := loopRootSystem(t, rt, gw, &harnessv1.HarnessSpec{MaxTurns: &turns})
	for name, p := range map[string]string{"HOME": os.Getenv("HOME"), "cwd": cwd, "workspaces": filepath.Join(os.Getenv("HOME"), ".fuse")} {
		if strings.Contains(sys, p) {
			t.Errorf("prompt contains the %s host path %q", name, p)
		}
	}
	if !strings.Contains(sys, "scratch/") {
		t.Error("prompt does not advertise scratch/")
	}
}

// TestLoopServerWiresConfinedWorkspaces pins that the production composition
// root sets a provider. Every confinement guarantee depends on this one field,
// and a nil provider silently means raw os.* tools.
func TestLoopServerWiresConfinedWorkspaces(t *testing.T) {
	deps, _ := harnessServerWith(t, harnessOperator, &recordingGateway{})
	if deps.Workspaces == nil {
		t.Fatal("the loop server has no workspace provider, so its file tools are unconfined")
	}
	if got := deps.Workspaces.Name(); got != "local" {
		t.Errorf("provider = %q, want local", got)
	}
}

// TestLoopServerLoopWorkspaceIsNotInsideBashTenantTree: bash's hosted root is
// ~/.fuse/workspaces/<tenant>. Nesting each loop's workspace under it means bash
// in one loop can read every sibling loop's files, which is the isolation this
// change exists to add. If bash stops receiving the tenant tree entirely and
// mounts only MountSource, replace this with a test of what bash can list.
func TestLoopServerLoopWorkspaceIsNotInsideBashTenantTree(t *testing.T) {
	deps, _ := harnessServerWith(t, harnessOperator, &recordingGateway{})
	ws, err := deps.Workspaces.Open(context.Background(), "acme", "loop1")
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Release(context.Background())
	mount, err := ws.MountSource(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	tenantTree := filepath.Join(hostedWorkspaceParent(os.Stderr), "acme")
	if strings.HasPrefix(mount+string(filepath.Separator), tenantTree+string(filepath.Separator)) {
		t.Errorf("loop workspace %q sits inside bash's tenant tree %q; sibling loops are visible to bash", mount, tenantTree)
	}
}

// TestCLIScratchIsNotInTheUsersRepo: appendScratchBlock is on every binding's
// prompt, and spec item 4 preserves interactive behaviour. On the CLI the old
// scratch dir was outside the repo; "scratch/" now means a folder in it, which
// does not exist and would show up in git status once the model uses it.
func TestCLIScratchIsNotInTheUsersRepo(t *testing.T) {
	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(repo)
	if out, err := exec.Command("git", "init", "-q").CombinedOutput(); err != nil {
		t.Skipf("git init: %v: %s", err, out)
	}
	block := appendScratchBlock("")
	if !strings.Contains(block, "scratch/") {
		return // a CLI block that names somewhere else is the fix
	}
	if _, err := os.Stat(filepath.Join(repo, "scratch")); err != nil {
		t.Errorf("the prompt advertises scratch/ but nothing created it in the CLI cwd: %v", err)
	}
	if err := exec.Command("git", "check-ignore", "-q", "scratch/x").Run(); err != nil {
		t.Errorf("the advertised scratch/ is not ignored, so throwaway files reach git status")
	}
}

// TestLoopServerWriteThenBashAgree is acceptance criterion 3, which was marked
// met with no test and no wiring: on the production loop-server composition, a
// relative path means the same file to write_file and to bash, in both
// directions, on the host substrate the spec names for this test.
func TestLoopServerWriteThenBashAgree(t *testing.T) {
	serverCwd, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(serverCwd)
	t.Setenv("HOME", t.TempDir())
	srv := httptest.NewServer(&recordingGateway{})
	t.Cleanup(srv.Close)
	cfg, err := config.FromYAML([]byte(harnessOperator), "operator")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Gateway.URL, cfg.Gateway.Key = srv.URL, "tkn"
	sb, err := sandbox.NewService(sandbox.Config{Contained: false, Handler: sandbox.HandlerHost})
	if err != nil {
		t.Fatal(err)
	}
	reg := registryFromConfig(cfg)
	skillSet, err := skills.LoadWithEmbedded(skills.DefaultDirs())
	if err != nil {
		t.Fatal(err)
	}
	toolReg := defaultToolRegistry(sb, cfg.Research, skillSet.Lookup)
	deps := assembleLoopServerRuntimeDeps(sb, cfg, reg, reg.Default, toolReg, rootSystemBlock(cfg, skillSet),
		permissions.AlwaysApprove, sessionRateGate(cfg), observe.NoopObserver{}, nil, nil, skillSet)
	deps.BaseDir = t.TempDir()
	deps.IdleTTL = time.Hour
	var loopReg *tools.Registry
	base := deps.NewToolRegistry
	deps.NewToolRegistry = func() *tools.Registry {
		loopReg = base()
		return loopReg
	}
	rt := runtime.New(deps)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	h, err := rt.StartLoop(ctx, runtime.LoopConfig{Task: "go", Interactive: true})
	if err != nil {
		t.Fatalf("StartLoop: %v", err)
	}
	parked := false
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline) && !parked; time.Sleep(10 * time.Millisecond) {
		hist, err := rt.Attach(ctx, "", h.ID(), 0)
		if err != nil {
			t.Fatalf("Attach: %v", err)
		}
		for _, e := range hist {
			parked = parked || e.Kind == event.KindLoopParked
		}
	}
	if !parked {
		t.Fatal("loop did not park")
	}
	run := func(name, args string) tools.Result { return loopReg.Execute(ctx, name, args) }

	if r := run("write_file", `{"path":"notes/a.md","content":"from-write\n"}`); r.IsError {
		t.Fatal(r.Output)
	}
	if r := run("bash", `{"command":"cat notes/a.md"}`); r.IsError || !strings.Contains(r.Output, "from-write") {
		t.Errorf("bash cannot see what write_file wrote: %+v", r)
	}
	if r := run("bash", `{"command":"mkdir -p out && echo from-bash > out/b.txt"}`); r.IsError {
		t.Fatalf("bash write: %s", r.Output)
	}
	if r := run("read_file", `{"path":"out/b.txt"}`); r.IsError || !strings.Contains(r.Output, "from-bash") {
		t.Errorf("read_file cannot see what bash wrote: %+v", r)
	}
	if r := run("bash", `{"command":"ls -d scratch"}`); r.IsError {
		t.Errorf("the advertised scratch/ does not exist where bash runs: %+v", r)
	}
	if _, err := os.Stat(filepath.Join(serverCwd, "out")); err == nil {
		t.Error("bash wrote into the server's cwd instead of the loop workspace")
	}
}
