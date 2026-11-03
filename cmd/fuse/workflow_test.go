package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/config"
)

func researchWF() config.WorkflowConfig {
	return config.WorkflowConfig{
		Skill: "research",
		Pool:  config.PoolConfig{Concurrent: 5, Total: 8, MaxDepth: 1},
		Workers: map[string]config.WorkerConfig{
			"facet-researcher": {Tools: []string{"web_search", "web_fetch", "read_file"}},
		},
	}
}

func TestResolveWorkerTools_NoWorkerReturnsRequested(t *testing.T) {
	got, err := resolveWorkerTools(researchWF(), "", []string{"read_file"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "read_file" {
		t.Errorf("got %v, want [read_file] unchanged", got)
	}
}

func TestResolveWorkerTools_KnownWorkerEmptyRequestGetsAllowlist(t *testing.T) {
	got, err := resolveWorkerTools(researchWF(), "facet-researcher", nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"web_search": true, "web_fetch": true, "read_file": true}
	if len(got) != 3 {
		t.Fatalf("got %v, want the 3-tool allowlist", got)
	}
	for _, tl := range got {
		if !want[tl] {
			t.Errorf("unexpected tool %q", tl)
		}
		if tl == "spawn_agent" {
			t.Error("facet-researcher allowlist must never yield spawn_agent")
		}
	}
}

func TestResolveWorkerTools_NarrowingIsAllowed(t *testing.T) {
	got, err := resolveWorkerTools(researchWF(), "facet-researcher", []string{"web_search"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "web_search" {
		t.Errorf("got %v, want [web_search] (narrowed)", got)
	}
}

func TestResolveWorkerTools_CannotWidenBeyondAllowlist(t *testing.T) {
	// bash is not in the worker's allowlist: requesting it must be rejected.
	if _, err := resolveWorkerTools(researchWF(), "facet-researcher", []string{"bash"}); err == nil {
		t.Fatal("expected error when narrowing names a tool outside the allowlist")
	}
}

func TestResolveWorkerTools_UnknownWorkerErrors(t *testing.T) {
	if _, err := resolveWorkerTools(researchWF(), "nonesuch", nil); err == nil {
		t.Fatal("expected error for unknown worker")
	}
}

func TestResolveWorkerTools_EmptyAllowlistErrors(t *testing.T) {
	wf := config.WorkflowConfig{Workers: map[string]config.WorkerConfig{"reader": {}}}
	for _, req := range [][]string{nil, {"read_file"}} {
		got, err := resolveWorkerTools(wf, "reader", req)
		if got != nil {
			t.Errorf("requested %v: got %v, want a nil slice", req, got)
		}
		if err == nil || !strings.Contains(err.Error(), `worker "reader" declares no tools`) {
			t.Errorf("requested %v: err = %v, want declares no tools", req, err)
		}
	}
}

// TestResolveWorkerTools_ToolLessWorker: a tool-less worker resolves to no
// tool list at all (the child builder gives it an empty registry), and any
// requested tool is refused rather than widening it.
func TestResolveWorkerTools_ToolLessWorker(t *testing.T) {
	wf := config.WorkflowConfig{Workers: map[string]config.WorkerConfig{"drafter": {NoTools: true}}}
	got, err := resolveWorkerTools(wf, "drafter", nil)
	if err != nil || got != nil {
		t.Fatalf("resolveWorkerTools(drafter, nil) = (%v, %v), want (nil, nil)", got, err)
	}
	got, err = resolveWorkerTools(wf, "drafter", []string{"read_file"})
	if got != nil {
		t.Errorf("requested [read_file]: got %v, want a nil slice", got)
	}
	if err == nil || !strings.Contains(err.Error(), `worker "drafter" permits no tools`) {
		t.Fatalf("requested [read_file]: err = %v, want permits no tools", err)
	}
}

// TestResolveRoleTools_ToolLessRole: a tool-less role resolves to no tool list
// and is not bound (it gets no blackboard), and any requested tool is refused.
func TestResolveRoleTools_ToolLessRole(t *testing.T) {
	role := config.RoleConfig{NoTools: true}
	got, bound, err := resolveRoleTools("judge", role, nil)
	if err != nil || got != nil || bound {
		t.Fatalf("resolveRoleTools(judge, nil) = (%v, %v, %v), want (nil, false, nil)", got, bound, err)
	}
	got, bound, err = resolveRoleTools("judge", role, []string{"read_file"})
	if got != nil || bound {
		t.Errorf("requested [read_file]: got (%v, %v), want (nil, false)", got, bound)
	}
	if err == nil || !strings.Contains(err.Error(), `role "judge" permits no tools`) {
		t.Fatalf("requested [read_file]: err = %v, want permits no tools", err)
	}
}

func TestBackstopFor_DepthLimit(t *testing.T) {
	tree := agent.NewAgentTree("root", "m")
	rootID := tree.RootID()
	act := &workflowActivation{name: "research", cfg: researchWF(), rootDepth: 0}

	bs := backstopFor(tree, act, rootID)
	if bs == nil {
		t.Fatal("backstop should be non-nil for a pool with total/max_depth set")
	}
	// max_depth=1, rootDepth=0 => a spawn producing depth 2 is refused.
	if err := bs(2); !errors.Is(err, agent.ErrWorkflowQuotaExhausted) {
		t.Errorf("depth 2 should be refused (max_depth 1), got %v", err)
	}
	// depth 1 is at the limit, allowed (the subtree is empty, so total is fine).
	if err := bs(1); err != nil {
		t.Errorf("depth 1 with empty subtree should be allowed, got %v", err)
	}
	// (Total-quota refusal within a turn is covered by the agent package's
	// TestSpawnWorkflowBackstopFires, which drives the real Spawn path.)
}

func TestBackstopFor_NilOutsideWorkflow(t *testing.T) {
	tree := agent.NewAgentTree("root", "m")
	if backstopFor(tree, nil, tree.RootID()) != nil {
		t.Error("no activation => nil backstop")
	}
}

func TestWorkerNames(t *testing.T) {
	act := workflowActivation{name: "research", cfg: researchWF(), rootDepth: 0}
	names := act.workerNames()
	if len(names) != 1 || names[0] != "facet-researcher" {
		t.Errorf("workerNames = %v, want [facet-researcher]", names)
	}
	// A workflow with no workers yields nil (freeform).
	empty := workflowActivation{cfg: config.WorkflowConfig{Skill: "x"}}
	if empty.workerNames() != nil {
		t.Errorf("expected nil worker names for a workerless workflow, got %v", empty.workerNames())
	}
}
