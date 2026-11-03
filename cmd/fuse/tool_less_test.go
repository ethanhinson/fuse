package main

import (
	"context"
	"reflect"
	"strings"
	"testing"

	harnessv1 "github.com/ethanhinson/fuse/harness/v1"
	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/config"
)

// Tool-less children (change 0116) at the gateway seam: a tool-less worker is
// offered no tools at every child-builder site, beside a narrowed worker that
// still gets its tools plus the blackboard, and a tool-less role refuses a
// spawn that asks for tools before any child exists.

// toolLessWorkerWorkflow is a workflow with a narrowed reader worker and a
// tool-less drafter worker.
const toolLessWorkerWorkflow = `workflows:
  wf:
    pool: {total: 4}
    workers:
      reader: {tools: [read_file]}
      drafter: {no_tools: true}
`

// toolLessWorkerYAML is the operator config of the worker parity rows.
const toolLessWorkerYAML = hostedOperatorYAML + toolLessWorkerWorkflow

// toolLessWorkerSpec is a harness spec defining the same workflow.
func toolLessWorkerSpec() *harnessv1.HarnessSpec {
	return &harnessv1.HarnessSpec{Workflows: map[string]*harnessv1.Workflow{
		"wf": {Pool: &harnessv1.Pool{Total: 4}, Workers: map[string]*harnessv1.Worker{
			"reader":  {Tools: []string{"read_file"}},
			"drafter": {NoTools: true},
		}},
	}}
}

// toolLessWorkerSpawns root the workflow per spawn and bind a worker in it.
var toolLessWorkerSpawns = []map[string]any{
	{"label": "drf", "task": "DRF-TASK", "workflow": "wf", "worker": "drafter"},
	{"label": "rdr", "task": "RDR-TASK", "workflow": "wf", "worker": "reader"},
}

// researchWorkerSpawns bind a worker in the research probe's own workflow,
// whose root is the probe's root, so they name no workflow.
var researchWorkerSpawns = []map[string]any{
	{"label": "drf", "task": "DRF-TASK", "worker": "drafter"},
	{"label": "rdr", "task": "RDR-TASK", "worker": "reader"},
}

// withResearchWorkers sets the research workflow's workers to the tool-less
// drafter and the narrowed reader.
func withResearchWorkers(cfg *config.Config) {
	rw := cfg.Workflows["research"]
	rw.Workers = map[string]config.WorkerConfig{
		"drafter": {NoTools: true},
		"reader":  {Tools: []string{"read_file"}},
	}
	cfg.Workflows["research"] = rw
}

// workerParityRows mirror roleParityRows site for site.
func workerParityRows() []parityRow {
	route := spawnRoute(toolLessWorkerSpawns)
	local := func(build parityBuild) func(t *testing.T) (*routedGateway, *agent.AgentTree) {
		return func(t *testing.T) (*routedGateway, *agent.AgentTree) {
			return parityLocal(t, toolLessWorkerYAML, route, nil, build, toolLessWorkerSpawns)
		}
	}
	return []parityRow{
		{"one-shot", toolLessWorkerYAML, local(oneShotParityBuild)},
		{"shell", toolLessWorkerYAML, local(shellParityBuild)},
		{"research-probe", hostedOperatorYAML, func(t *testing.T) (*routedGateway, *agent.AgentTree) {
			return parityLocal(t, hostedOperatorYAML, spawnRoute(researchWorkerSpawns), withResearchWorkers,
				researchProbeParityBuild("research"), researchWorkerSpawns)
		}},
		{"loop-server/operator", toolLessWorkerYAML, func(t *testing.T) (*routedGateway, *agent.AgentTree) {
			r := runHostedSpecWith(t, toolLessWorkerYAML, &harnessv1.HarnessSpec{}, route)
			return r.gw, r.tree
		}},
		{"loop-server/spec", hostedOperatorYAML, func(t *testing.T) (*routedGateway, *agent.AgentTree) {
			r := runHostedSpec(t, toolLessWorkerSpec(), route)
			return r.gw, r.tree
		}},
	}
}

// TestToolLessWorkerParityAcrossChildBuilders drives a tool-less and a
// narrowed worker spawn through every child-builder site.
func TestToolLessWorkerParityAcrossChildBuilders(t *testing.T) {
	for _, row := range workerParityRows() {
		t.Run(row.name, func(t *testing.T) {
			gw, tree := row.run(t)
			assertToolLessChild(t, gw, "DRF-TASK", toolLessPersonaFor(t, row.operatorYAML, "fast"))
			if got := sortedToolNames(turnRequest(t, gw, "RDR-TASK", 0)); !reflect.DeepEqual(got, readerSchema) {
				t.Errorf("RDR-TASK tools = %v, want %v", got, readerSchema)
			}
			for _, label := range []string{"drf", "rdr"} {
				nodeLabelled(t, tree, label)
			}
		})
	}
}

// TestToolLessWorkerParityTableCoversEveryChildBuilder keeps the worker table
// the size of the role table, which TestRoleParityTableCoversEveryChildBuilder
// pins to the child-builder sites: a new site fails here too.
func TestToolLessWorkerParityTableCoversEveryChildBuilder(t *testing.T) {
	roles, workers := roleParityRows(), workerParityRows()
	if len(workers) != len(roles) {
		t.Fatalf("worker parity table has %d rows, want the role parity table's %d", len(workers), len(roles))
	}
	for i := range roles {
		if workers[i].name != roles[i].name {
			t.Errorf("worker parity row %d = %q, want %q (the role parity row)", i, workers[i].name, roles[i].name)
		}
	}
}

// TestToolLessRoleRefusesToolsAcrossBindings: a spawn naming a tool-less role
// and a tool set is refused before any node exists, on a local binding and on
// a hosted spec. Both go through workflowScope.checkRequest.
func TestToolLessRoleRefusesToolsAcrossBindings(t *testing.T) {
	const refusal = `role "drafter" permits no tools`
	bad := map[string]any{"label": "bad", "task": "BAD-TASK", "role": "drafter", "tools": []string{"read_file"}}

	t.Run("one-shot", func(t *testing.T) {
		gw, tree, toolReg := parityLocalSetup(t, roleOperatorYAML, roleChildRoute, nil, oneShotParityBuild)
		before := len(tree.SnapshotAll())
		res := toolReg.Execute(context.Background(), "spawn_agent", argsJSON(bad))
		if !res.IsError || !strings.Contains(res.Output, refusal) {
			t.Fatalf("spawn_agent = %+v, want an error containing %q", res, refusal)
		}
		if n := len(tree.SnapshotAll()); n != before {
			t.Errorf("refused spawn created %d nodes, want 0", n-before)
		}
		if n := len(gw.callsFor("BAD-TASK")); n != 0 {
			t.Errorf("BAD-TASK requests = %d, want 0", n)
		}
	})

	t.Run("loop-server/spec", func(t *testing.T) {
		r := runHostedSpec(t, roleParitySpec(), spawnRoute([]map[string]any{bad}))
		if got := countContaining(toolResults(turnRequest(t, r.gw, "ROOT-TASK", 1)), refusal); got != 1 {
			t.Fatalf("root turn-1 tool results carrying %q = %d, want 1", refusal, got)
		}
		if n := len(r.gw.callsFor("BAD-TASK")); n != 0 {
			t.Errorf("BAD-TASK requests = %d, want 0", n)
		}
		for _, v := range r.tree.SnapshotAll() {
			if v.Label == "bad" {
				t.Errorf("refused spawn created node %q", v.ID)
			}
		}
	})
}
