package runtime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	harnessv1 "github.com/ethanhinson/fuse/harness/v1"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/event/fsstore"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/tools"
	"github.com/ethanhinson/fuse/internal/workspace"
)

// regCapture records the per-loop registry a runtime built, so a test can call
// the tools the loop's model would call.
type regCapture struct {
	mu  sync.Mutex
	reg *tools.Registry
}

func (c *regCapture) newRegistry(base ...tools.Tool) func() *tools.Registry {
	return func() *tools.Registry {
		r := tools.NewRegistry()
		for _, tl := range base {
			r.Register(tl)
		}
		c.mu.Lock()
		c.reg = r
		c.mu.Unlock()
		return r
	}
}

func (c *regCapture) exec(t *testing.T, name string, args map[string]any) tools.Result {
	t.Helper()
	c.mu.Lock()
	r := c.reg
	c.mu.Unlock()
	if r == nil {
		t.Fatal("registry was never built")
	}
	b, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return r.Execute(context.Background(), name, string(b))
}

// fileTools is what a real binding's base registry holds: the raw os.* forms.
func fileTools() []tools.Tool {
	return []tools.Tool{tools.NewReadFile(), tools.NewWriteFile(), tools.NewEditFile(), tools.NewListDirectory(), tools.NewGrep()}
}

// parkOn starts an interactive loop on a runtime and waits for it to park.
func parkOn(t *testing.T, deps Deps, tenant event.TenantID) (Runtime, LoopHandle) {
	t.Helper()
	fake := newGatedCompleter(model.CompletionResp{Content: "ok"})
	var bc bindingCapture
	deps.MaxConcurrent = 1
	deps.BuildAgent = bc.buildAgent(fake)
	rt := New(deps)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	h, err := rt.StartLoop(ctx, LoopConfig{Task: "hi", ModelID: "cloud/x", Interactive: true, Tenant: tenant})
	if err != nil {
		t.Fatalf("StartLoop: %v", err)
	}
	ev, unsub, err := rt.Observe(ctx, tenant, h.ID())
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	fake.admit(0)
	waitForKind(t, ev, event.KindLoopParked, 2*time.Second)
	unsub()
	return rt, h
}

// resumeOn resumes loopID on a fresh runtime and returns the resume error.
func resumeOn(t *testing.T, deps Deps, tenant event.TenantID, loopID string) error {
	t.Helper()
	fake := newGatedCompleter(model.CompletionResp{Content: "ok"})
	var bc bindingCapture
	deps.MaxConcurrent = 1
	deps.IdleTTL = time.Hour
	deps.BuildAgent = bc.buildAgent(fake)
	rt := New(deps)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	_, err := rt.Resume(ctx, tenant, loopID)
	return err
}

// waitReaped waits for an idle interactive loop to give up ownership.
func waitReaped(t *testing.T, h LoopHandle) {
	t.Helper()
	select {
	case <-doneOf(h):
	case <-time.After(3 * time.Second):
		t.Fatal("interactive loop was not reaped after going idle")
	}
}

// TestWorkspace_EventImmediatelyFollowsHarness tightens criterion 4: the change
// loosened the existing hist[1] assertions to accept either kind, and its own
// test only counted the events, so nothing pinned the position.
func TestWorkspace_EventImmediatelyFollowsHarness(t *testing.T) {
	store := fsstore.NewDurableFSStore(t.TempDir())
	fh := &fakeHarness{completer: &scriptedCompleter{}}
	rt := New(Deps{MaxConcurrent: 1, DurableStore: store, Registry: store, Harness: fh,
		Workspaces: &workspace.LocalProvider{Parent: t.TempDir()}})
	hist := runToCompletion(t, rt, LoopConfig{Task: "hi", Tenant: "acme", Harness: testSpec()})
	if len(hist) < 2 || hist[0].Kind != event.KindLoopHarness || hist[1].Kind != event.KindLoopWorkspace {
		t.Fatalf("stream opens %v, want loop.harness then loop.workspace", kindsOf(hist))
	}
}

// TestWorkspace_EventCarriesNoHostPath: the record is identity, never a location.
func TestWorkspace_EventCarriesNoHostPath(t *testing.T) {
	parent := t.TempDir()
	store := fsstore.NewDurableFSStore(t.TempDir())
	hits := 0
	rt := New(Deps{MaxConcurrent: 1, DurableStore: store, Registry: store,
		Workspaces: &workspace.LocalProvider{Parent: parent},
		BuildAgent: processBuild(t, &scriptedCompleter{}, &hits)})
	hist := runToCompletion(t, rt, LoopConfig{Task: "hi", Tenant: "acme"})
	recs := workspaceEvents(hist)
	if len(recs) != 1 {
		t.Fatalf("loop.workspace events = %d, want 1", len(recs))
	}
	if strings.Contains(string(recs[0].Payload), parent) {
		t.Errorf("event leaks the host path: %s", recs[0].Payload)
	}
}

// TestWorkspace_FilesSurviveResumeOnAnotherRuntime is the two-runtime test from
// spec item 8, through the tools the loop actually holds. The change's resume
// test only counted events; it never looked at a file.
func TestWorkspace_FilesSurviveResumeOnAnotherRuntime(t *testing.T) {
	parent := t.TempDir()
	store := fsstore.NewDurableFSStore(t.TempDir())
	const tenant event.TenantID = "acme"

	var ca regCapture
	_, h := parkOn(t, Deps{DurableStore: store, Registry: store, IdleTTL: 120 * time.Millisecond,
		Workspaces: &workspace.LocalProvider{Parent: parent}, NewToolRegistry: ca.newRegistry(fileTools()...)}, tenant)
	if r := ca.exec(t, "write_file", map[string]any{"path": "notes/a.md", "content": "from A\n"}); r.IsError {
		t.Fatalf("write on A: %s", r.Output)
	}
	waitReaped(t, h)

	var cb regCapture
	if err := resumeOn(t, Deps{DurableStore: store, Registry: store,
		Workspaces: &workspace.LocalProvider{Parent: parent}, NewToolRegistry: cb.newRegistry(fileTools()...)}, tenant, h.ID()); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if r := cb.exec(t, "read_file", map[string]any{"path": "notes/a.md"}); r.IsError || !strings.Contains(r.Output, "from A") {
		t.Errorf("read on B: %+v", r)
	}
	if r := cb.exec(t, "list_directory", map[string]any{"path": "."}); !strings.Contains(r.Output, "scratch/") {
		t.Errorf("list on B: %+v", r)
	}
}

// TestWorkspace_LoopsCannotReachEachOther: two loops of one tenant, through the
// tools. Tenant isolation is covered by the conformance suite; loop isolation
// inside a tenant is what the (tenant, loop) key adds and was untested.
func TestWorkspace_LoopsCannotReachEachOther(t *testing.T) {
	parent := t.TempDir()
	store := fsstore.NewDurableFSStore(t.TempDir())
	const tenant event.TenantID = "acme"
	var a, b regCapture
	_, ha := parkOn(t, Deps{DurableStore: store, Registry: store, IdleTTL: time.Hour,
		Workspaces: &workspace.LocalProvider{Parent: parent}, NewToolRegistry: a.newRegistry(fileTools()...)}, tenant)
	parkOn(t, Deps{DurableStore: store, Registry: store, IdleTTL: time.Hour,
		Workspaces: &workspace.LocalProvider{Parent: parent}, NewToolRegistry: b.newRegistry(fileTools()...)}, tenant)
	if r := a.exec(t, "write_file", map[string]any{"path": "mine.txt", "content": "A-only\n"}); r.IsError {
		t.Fatal(r.Output)
	}
	for _, p := range []string{"mine.txt", "../" + ha.ID() + "/mine.txt", filepath.Join(parent, string(tenant), ha.ID(), "mine.txt")} {
		if r := b.exec(t, "read_file", map[string]any{"path": p}); !r.IsError || strings.Contains(r.Output, "A-only") {
			t.Errorf("loop B read %q: %+v", p, r)
		}
	}
}

// TestWorkspace_ProviderMismatchRefusesResume pins the refusal in openWorkspace.
func TestWorkspace_ProviderMismatchRefusesResume(t *testing.T) {
	store := fsstore.NewDurableFSStore(t.TempDir())
	const tenant event.TenantID = "acme"
	var ca regCapture
	_, h := parkOn(t, Deps{DurableStore: store, Registry: store, IdleTTL: 120 * time.Millisecond,
		Workspaces: workspace.NewMemProvider(), NewToolRegistry: ca.newRegistry()}, tenant)
	waitReaped(t, h)
	var cb regCapture
	err := resumeOn(t, Deps{DurableStore: store, Registry: store,
		Workspaces: &workspace.LocalProvider{Parent: t.TempDir()}, NewToolRegistry: cb.newRegistry()}, tenant, h.ID())
	if err == nil {
		t.Error("resume across providers succeeded; it must refuse")
	}
}

// TestWorkspace_ResumeOfPreWorkspaceLoopIsConfined: a loop parked before this
// change has no loop.workspace event. Resuming it on a hosted server must not
// hand the model raw os.* file tools under AlwaysApprove; it may open a
// workspace or refuse, but it may not fail open.
func TestWorkspace_ResumeOfPreWorkspaceLoopIsConfined(t *testing.T) {
	store := fsstore.NewDurableFSStore(t.TempDir())
	const tenant event.TenantID = "acme"
	secret := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(secret, []byte("topsecret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var ca regCapture
	_, h := parkOn(t, Deps{DurableStore: store, Registry: store, IdleTTL: 120 * time.Millisecond,
		NewToolRegistry: ca.newRegistry(fileTools()...)}, tenant)
	waitReaped(t, h)

	var cb regCapture
	err := resumeOn(t, Deps{DurableStore: store, Registry: store,
		Workspaces: &workspace.LocalProvider{Parent: t.TempDir()}, NewToolRegistry: cb.newRegistry(fileTools()...)}, tenant, h.ID())
	if err != nil {
		return // refusing is an acceptable fail-closed answer
	}
	if r := cb.exec(t, "read_file", map[string]any{"path": secret}); !r.IsError {
		t.Errorf("resumed loop has unconfined file tools; read %q: %q", secret, r.Output)
	}
}

// TestWorkspace_ResumeWithoutProviderRefuses: the stream says this loop was
// confined, so a runtime with no provider cannot quietly drop the confinement.
func TestWorkspace_ResumeWithoutProviderRefuses(t *testing.T) {
	store := fsstore.NewDurableFSStore(t.TempDir())
	const tenant event.TenantID = "acme"
	var ca regCapture
	_, h := parkOn(t, Deps{DurableStore: store, Registry: store, IdleTTL: 120 * time.Millisecond,
		Workspaces: &workspace.LocalProvider{Parent: t.TempDir()}, NewToolRegistry: ca.newRegistry(fileTools()...)}, tenant)
	waitReaped(t, h)
	var cb regCapture
	if err := resumeOn(t, Deps{DurableStore: store, Registry: store, NewToolRegistry: cb.newRegistry(fileTools()...)}, tenant, h.ID()); err == nil {
		t.Error("a recorded workspace was resumed with no provider; it must refuse")
	}
}

// TestWorkspace_RebindOnlyReplacesExistingTools: the rebind is documented as an
// overwrite, but Register adds. A registry that withheld a file tool must not
// get it back from the runtime.
func TestWorkspace_RebindOnlyReplacesExistingTools(t *testing.T) {
	store := fsstore.NewDurableFSStore(t.TempDir())
	var c regCapture
	hits := 0
	rt := New(Deps{MaxConcurrent: 1, DurableStore: store, Registry: store,
		Workspaces:      &workspace.LocalProvider{Parent: t.TempDir()},
		NewToolRegistry: c.newRegistry(tools.NewReadFile()),
		BuildAgent:      processBuild(t, &scriptedCompleter{}, &hits)})
	h, err := rt.StartLoop(context.Background(), LoopConfig{Task: "hi", Tenant: "acme"})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = h.Wait()
	for _, name := range []string{"write_file", "edit_file", "list_directory", "grep"} {
		if c.reg.Has(name) {
			t.Errorf("runtime added withheld tool %q", name)
		}
	}
	if !c.reg.Has("read_file") {
		t.Error("read_file was held and must stay")
	}
}

// TestWorkspace_ToolLessLoopOpensNoWorkspace: a no_tools loop has nothing that
// could use a workspace, so neither a directory nor an identity failure should
// come of it.
func TestWorkspace_ToolLessLoopOpensNoWorkspace(t *testing.T) {
	parent := t.TempDir()
	store := fsstore.NewDurableFSStore(t.TempDir())
	fh := &fakeHarness{completer: &scriptedCompleter{}}
	rt := New(Deps{MaxConcurrent: 1, DurableStore: store, Registry: store, Harness: fh,
		Workspaces: &workspace.LocalProvider{Parent: parent}})
	turns := uint32(1)
	spec := &harnessv1.HarnessSpec{Models: &harnessv1.Models{DefaultAlias: "glm"}, MaxTurns: &turns, NoTools: true}
	h, err := rt.StartLoop(context.Background(), LoopConfig{Task: "hi", Tenant: "Org:Mixed", Harness: spec})
	if err != nil {
		t.Fatalf("tool-less loop refused over a workspace it cannot use: %v", err)
	}
	_, _ = h.Wait()
	if entries, _ := os.ReadDir(parent); len(entries) != 0 {
		t.Errorf("tool-less loop created workspace state: %v", entries)
	}
}

// TestWorkspace_TenantIDsThatRanBeforeStillStart: before this change a tenant
// outside the path-segment allowlist lost bash but its loop ran. Refusing
// StartLoop for every such tenant is a wider outage than the change scoped.
// Distinct identities must also stay distinct on a case-insensitive filesystem.
func TestWorkspace_TenantIDsThatRanBeforeStillStart(t *testing.T) {
	store := fsstore.NewDurableFSStore(t.TempDir())
	const mixed, lower event.TenantID = "Acme", "acme"
	var a, b regCapture
	parent := t.TempDir()
	parkOn(t, Deps{DurableStore: store, Registry: store, IdleTTL: time.Hour,
		Workspaces: &workspace.LocalProvider{Parent: parent}, NewToolRegistry: a.newRegistry(fileTools()...)}, mixed)
	parkOn(t, Deps{DurableStore: store, Registry: store, IdleTTL: time.Hour,
		Workspaces: &workspace.LocalProvider{Parent: parent}, NewToolRegistry: b.newRegistry(fileTools()...)}, lower)
	if r := a.exec(t, "write_file", map[string]any{"path": "x.txt", "content": "mixed\n"}); r.IsError {
		t.Fatal(r.Output)
	}
	if r := b.exec(t, "read_file", map[string]any{"path": "x.txt"}); !r.IsError {
		t.Errorf("tenants %q and %q share a workspace: %q", mixed, lower, r.Output)
	}
}

// TestWorkspace_CodeindexAnswersAboutTheWorkspace is spec item 7: the tool runs
// in the workspace or is not registered, because the model must never see a tool
// that answers about the server's own checkout.
func TestWorkspace_CodeindexAnswersAboutTheWorkspace(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "codeindex")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\npwd -P\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEINDEX_BIN", bin)
	serverCwd, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(serverCwd)

	store := fsstore.NewDurableFSStore(t.TempDir())
	var c regCapture
	parkOn(t, Deps{DurableStore: store, Registry: store, IdleTTL: time.Hour,
		Workspaces: &workspace.LocalProvider{Parent: t.TempDir()}, NewToolRegistry: c.newRegistry(tools.CodeindexTools()...)}, "acme")
	if !c.reg.Has("codeindex_callers") {
		return // not registering it is an allowed answer
	}
	r := c.exec(t, "codeindex_callers", map[string]any{"symbol": "X"})
	if strings.TrimSpace(r.Output) == serverCwd {
		t.Errorf("codeindex ran in the server's cwd %q, not the loop workspace", serverCwd)
	}
}
