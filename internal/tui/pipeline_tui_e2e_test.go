package tui

// End-to-end TUI verification for change 0026 (pipeline composition), mirroring
// the blackboard round-trip e2e (blackboard_tui_e2e_test.go). It proves the
// pipeline_run tool reaches the screen through the REAL ShellModel + bridge: a
// scripted root turn calls pipeline_run with an authored 2-step chain, the tool
// drives the REAL internal/pipeline engine over a REAL agent.Spawner whose
// child steps run as real child Agents, each step's spawn renders as an agent
// node in the live transcript, and the terminal status flows back to the model.
// A visual-confirmation screenshot (.ansi/.txt/.png via captureFrame →
// FUSE_SCREENSHOT_DIR) is captured of the settled transcript.

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/exp/teatest"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/permissions"
	"github.com/ethanhinson/fuse/internal/pipeline"
	"github.com/ethanhinson/fuse/internal/tools"
)

// pipelineChainYAML is a 2-step chain: synth depends on search. Distinctive
// prompts let a scripted child completer answer per step.
const pipelineChainYAML = `
name: research-chain
steps:
  - name: search
    prompt: "PIPELINE-SEARCH gather sources"
    outputs: ["hits"]
  - name: synth
    prompt: "PIPELINE-SYNTH combine the findings"
    depends_on: ["search"]
    outputs: ["report"]
`

// TestTUI_PipelineRunRoundTrip drives a turn whose scripted root model calls the
// real pipeline_run tool with an authored chain. The tool's runFn parses,
// validates, and runs the pipeline over a REAL agent.Spawner; each step spawns a
// real child Agent whose result is written to the blackboard. The step spawns
// surface as agent nodes in the live transcript, and pipeline.status lands on
// the blackboard. Captures the settled transcript screenshot.
func TestTUI_PipelineRunRoundTrip(t *testing.T) {
	tree := agent.NewAgentTree("alpha", "test/model")
	root := tree.Node(tree.RootID())
	bb := agent.NewBlackboard(tree)

	// Child steps run as real child agents driven by their own scripted
	// completer (one reply per step spawn, no tool calls so each child loop
	// ends immediately). The spawner uses a child builder that renders into the
	// tree so the step spawns appear in the live transcript.
	childCmp := &scriptedCompleter{responses: []model.CompletionResp{
		{Content: "search-step-done"},
		{Content: "synth-step-done"},
	}}
	spawner := agent.NewSpawner(
		agent.WithTree(tree),
		agent.WithNode(root),
		agent.WithSpawnDepth(0),
		agent.WithChildBuilder(func(ctx context.Context, opts agent.SpawnOpts, childNode *agent.AgentNode, childTree *agent.AgentTree) (string, error) {
			r := NewNodeRenderer(childNode, childTree)
			child := agent.New(childCmp, tools.NewRegistry(), r, "test/model", opts.SystemPrompt, 2, 0)
			msgs, rerr := child.Run(ctx, []model.Message{{Role: "user", Content: opts.Task}})
			if rerr != nil {
				return "", rerr
			}
			return lastAssistantContent(msgs), nil
		}),
	)

	// runFn: parse + validate (authored path, no synthesis caps) + run.
	runFn := func(ctx context.Context, def []byte) (string, error) {
		p, err := pipeline.Parse(def)
		if err != nil {
			return "", err
		}
		if err := pipeline.Validate(p, pipeline.Caps{}); err != nil {
			return "", err
		}
		st, err := pipeline.Run(ctx, p, spawner, bb)
		if err != nil {
			return "", err
		}
		if st.State == pipeline.StateFailed {
			return "pipeline " + p.Name + ": failed at " + st.FailedStep, nil
		}
		return "pipeline " + p.Name + ": completed", nil
	}

	reg := tools.NewRegistry()
	reg.Register(tools.NewPipelineRunTool(runFn, nil))
	if !reg.Has("pipeline_run") {
		t.Fatal("pipeline_run tool not registered")
	}

	// Turn 1: call pipeline_run with the authored chain. Turn 2: reply + stop.
	cmp := &scriptedCompleter{responses: []model.CompletionResp{
		{ToolCalls: []model.ToolCall{
			{ID: "1", Name: "pipeline_run", Arguments: pipelineRunArgs},
		}},
		{Content: "pipeline-round-trip-complete"},
	}}

	exec := regExec{reg: reg}
	build := func(_ string, r agent.Renderer, _ permissions.ApprovalFunc) (*agent.Agent, error) {
		return agent.New(cmp, exec, r, "test/model", "", 25, 0), nil
	}
	m := NewShellModel("alpha", true, "", testRegistry(), nil, build, permissions.NewSessionMode(permissions.ModeSmart), true)

	bridgeCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tm := teatest.NewTestModel(t, m, teatest.WithInitialTermSize(96, 30))
	StartBridges(bridgeCtx, tm.GetProgram(), m.Channel(), nil, nil)
	t.Cleanup(func() { tm.Quit() })

	for _, r := range "run the pipeline" {
		tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})

	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		return bytes.Contains(stripANSI(b), []byte("pipeline-round-trip-complete"))
	}, teatest.WithDuration(20*time.Second), teatest.WithCheckInterval(20*time.Millisecond))

	// The engine must have written pipeline.status through the live run.
	if _, ok := bb.Get("pipeline.status"); !ok {
		t.Error("blackboard has no pipeline.status after the live pipeline run")
	}
	// The chain's step outputs must be present (proves both steps executed).
	if _, ok := bb.Get("report"); !ok {
		t.Error("blackboard has no report key — synth step did not run")
	}

	// Visual-confirmation screenshot of the settled transcript.
	tm.Send(tea.KeyMsg{Type: tea.KeyCtrlC})
	frame := captureFrame(t, tm, "pipeline-run")
	if !strings.Contains(frame, "pipeline_run") {
		t.Errorf("screenshot frame missing the pipeline_run call\n%s", frame)
	}
	if !strings.Contains(frame, "pipeline-round-trip-complete") {
		t.Errorf("screenshot frame missing the final reply\n%s", frame)
	}
}

// pipelineRunArgs is the JSON tool-call argument object carrying the authored
// chain as the `definition` string.
var pipelineRunArgs = func() string {
	b, err := json.Marshal(map[string]any{"definition": pipelineChainYAML})
	if err != nil {
		panic(err)
	}
	return string(b)
}()

// lastAssistantContent returns the content of the last assistant message, the
// child agent's final result text (local to this test; cmd/fuse has its own).
func lastAssistantContent(msgs []model.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "assistant" && msgs[i].Content != "" {
			return msgs[i].Content
		}
	}
	return ""
}
