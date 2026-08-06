package main

import (
	"testing"

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
