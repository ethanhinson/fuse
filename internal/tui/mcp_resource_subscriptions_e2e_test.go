package tui

// End-to-end dogfood test (change 0021): a REAL fuse mcp client (mcp.Manager)
// subscribes to a REAL fuse mcp server's fuse://tools resource; a config
// live-reload on the server pushes a notifications/resources/updated; the client
// read-pump routes it through the notification router → OnResource observer →
// MCPResourceUpdatedMsg on the model channel; and the live TUI renders a stale
// indicator. Screenshots are captured at (a) the steady subscribed state and
// (b) the post-push stale state as durable visual evidence.
//
// The server is this test binary re-executed as a minimal-but-real fuse
// mcp-server (Go's helper-process idiom) — hermetic, no external process, and it
// exercises the actual Server.dispatch resources/* path plus the real
// PushResourceUpdated wire frame. The reload trigger is a real file write the
// server watches with fsnotify, mirroring the production config-watch.

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/exp/teatest"
	"github.com/fsnotify/fsnotify"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/mcp"
	"github.com/ethanhinson/fuse/internal/permissions"
	"github.com/ethanhinson/fuse/internal/tools"
)

func TestTUI_MCPResourceSubscriptionDogfoodE2E(t *testing.T) {
	// The trigger file the re-execed server watches; touching it drives the
	// live-reload → registry change → fuse://tools push.
	triggerFile := filepath.Join(t.TempDir(), "reload.trigger")
	if err := os.WriteFile(triggerFile, []byte("v0"), 0o644); err != nil {
		t.Fatalf("seed trigger file: %v", err)
	}

	reg := tools.NewRegistry()
	mgr, err := mcp.NewManager([]config.MCPServerConfig{{
		Name:      "fuse",
		Transport: "stdio",
		Command:   []string{os.Args[0], "-test.run=TestFuseMCPServerHelper"},
		Env: map[string]string{
			"FUSE_MCP_DOGFOOD":      "1",
			"FUSE_MCP_TRIGGER_FILE": triggerFile,
		},
	}}, reg)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer mgr.Close()

	// The dogfood server advertises resources.subscribe — the client's Subscribe
	// gate (D4) must pass against fuse's own server.
	var caps mcp.ServerStatus
	for _, s := range mgr.Status() {
		if s.Name == "fuse" {
			caps = s
		}
	}
	hasSub := false
	for _, c := range caps.Capabilities {
		if c == "resources.subscribe" {
			hasSub = true
		}
	}
	if !hasSub {
		t.Fatalf("dogfood server must advertise resources.subscribe, got caps: %v", caps.Capabilities)
	}

	// List + read the fuse://tools resource through the real client path.
	resList, err := mgr.ListResources(context.Background(), "fuse")
	if err != nil {
		t.Fatalf("ListResources: %v", err)
	}
	foundTools := false
	for _, r := range resList {
		if r.URI == mcp.ToolsResourceURI {
			foundTools = true
		}
	}
	if !foundTools {
		t.Fatalf("resources/list must include %s, got %+v", mcp.ToolsResourceURI, resList)
	}
	if _, err := mgr.ReadResource(context.Background(), "fuse", mcp.ToolsResourceURI); err != nil {
		t.Fatalf("ReadResource: %v", err)
	}

	// Build a real ShellModel and wire OnResource → channel exactly as shell.go.
	cmp := assistantOnce("subscribed")
	build := func(_ string, r agent.Renderer, _ permissions.ApprovalFunc) (*agent.Agent, error) {
		return agent.New(cmp, noopToolExec{}, r, "test/model", "", 25, 0), nil
	}
	m := NewShellModel("alpha", false, "", testRegistry(), nil, build, permissions.NewSessionMode(permissions.ModeSmart), true)
	ch := m.Channel()
	mgr.OnResource(func(e mcp.ResourceUpdatedEvent) {
		select {
		case ch <- MCPResourceUpdatedMsg{Server: e.Server, URI: e.URI}:
		default:
		}
	})

	bridgeCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tm := teatest.NewTestModel(t, m, teatest.WithInitialTermSize(96, 24))
	StartBridges(bridgeCtx, tm.GetProgram(), m.Channel(), nil, nil)
	t.Cleanup(func() { tm.Quit() })

	// Subscribe through the REAL client → server resources/subscribe path.
	if err := mgr.Subscribe(context.Background(), "fuse", mcp.ToolsResourceURI); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// Type a prompt so the transcript has content, then let the turn settle to a
	// steady subscribed state before the push.
	for _, r := range "subscribed to fuse://tools" {
		tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	// Let the program render the steady frame.
	time.Sleep(200 * time.Millisecond)

	// (a) Steady subscribed-state screenshot (no stale indicator yet).
	steady := captureModelFrameLive(t, tm, "mcp-resource-subscribed")
	if bytes.Contains([]byte(steady), []byte("stale")) {
		t.Errorf("steady frame should not show a stale indicator yet:\n%s", steady)
	}

	// Trigger the server's live-reload: write the trigger file. The server's
	// fsnotify watch fires → it changes its tool registry → pushes fuse://tools.
	if err := os.WriteFile(triggerFile, []byte("v1"), 0o644); err != nil {
		t.Fatalf("write trigger: %v", err)
	}

	// The client routes the push and the TUI must render the stale indicator.
	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		s := stripANSI(b)
		return bytes.Contains(s, []byte("stale")) && bytes.Contains(s, []byte(mcp.ToolsResourceURI))
	}, teatest.WithDuration(15*time.Second), teatest.WithCheckInterval(20*time.Millisecond))

	// The stale flag is set on the client too (D2: flagged, not auto-read).
	if !mgr.IsStale("fuse", mcp.ToolsResourceURI) {
		t.Error("client should have flagged fuse://tools stale after the push")
	}

	// (b) Post-push stale-state screenshot.
	tm.Send(tea.KeyMsg{Type: tea.KeyCtrlC})
	stale := captureFrame(t, tm, "mcp-resource-stale")
	if !bytes.Contains([]byte(stale), []byte("stale")) || !bytes.Contains([]byte(stale), []byte(mcp.ToolsResourceURI)) {
		t.Errorf("post-push frame missing stale indicator for %s:\n%s", mcp.ToolsResourceURI, stale)
	}
}

// captureModelFrameLive captures a screenshot of a still-running program by
// snapshotting the current model frame WITHOUT quitting (used for the mid-run
// steady-state capture). It renders the model's View() the same way captureFrame
// does but reads the live model rather than the final one.
func captureModelFrameLive(t *testing.T, tm *teatest.TestModel, name string) string {
	t.Helper()
	// The steady frame is captured from the output stream (the live program has
	// not quit yet, so FinalModel would block). We render what's on screen.
	var last []byte
	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		last = append([]byte(nil), b...)
		return len(stripANSI(b)) > 0
	}, teatest.WithDuration(5*time.Second), teatest.WithCheckInterval(20*time.Millisecond))
	// Persist the .ansi/.txt artifacts from the live output stream.
	dir := os.Getenv("FUSE_SCREENSHOT_DIR")
	if dir == "" {
		dir = t.TempDir()
	}
	_ = os.MkdirAll(dir, 0o755)
	ansiPath := filepath.Join(dir, name+".ansi")
	_ = os.WriteFile(ansiPath, last, 0o644)
	stripped := stripANSI(last)
	_ = os.WriteFile(filepath.Join(dir, name+".txt"), stripped, 0o644)
	if png := renderFramePNG(t, ansiPath, filepath.Join(dir, name+".png")); png != "" {
		t.Logf("screenshot PNG: %s", png)
	}
	return string(stripped)
}

// TestFuseMCPServerHelper is not a real test: re-execed with FUSE_MCP_DOGFOOD=1
// it becomes a REAL fuse mcp server (mcp.NewServer over the built-in tool
// registry) with a config-watch that, on a trigger-file change, mutates its tool
// registry and pushes a fuse://tools resources/updated to subscribed clients —
// the exact dogfood path cmd/fuse/mcp_server.go wires in production. Under a
// normal `go test` run (env unset) it no-ops.
func TestFuseMCPServerHelper(t *testing.T) {
	if os.Getenv("FUSE_MCP_DOGFOOD") != "1" {
		return
	}
	reg := tools.NewRegistry()
	// A minimal starting catalog.
	reg.Register(dogfoodTool{name: "seed"})
	gate := permissions.New(config.PermissionsConfig{Mode: "off"}, reg, permissions.AlwaysApprove)
	srv := mcp.NewServer(os.Stdin, os.Stdout, reg, gate)

	// Config-watch: a change to the trigger file rebuilds the catalog (adds a
	// tool) and pushes fuse://tools — mirroring watchMCPToolRegistry.
	if trig := os.Getenv("FUSE_MCP_TRIGGER_FILE"); trig != "" {
		w, err := fsnotify.NewWatcher()
		if err == nil {
			_ = w.Add(trig)
			go func() {
				n := 0
				for {
					select {
					case ev, ok := <-w.Events:
						if !ok {
							return
						}
						if ev.Op&(fsnotify.Write|fsnotify.Create) == 0 {
							continue
						}
						n++
						reg.Register(dogfoodTool{name: "added_by_reload"})
						srv.PushResourceUpdated(mcp.ToolsResourceURI)
					case <-w.Errors:
					}
				}
			}()
		}
	}
	_ = srv.Serve(context.Background())
	os.Exit(0)
}

// dogfoodTool is a trivial tool for the dogfood server's registry.
type dogfoodTool struct{ name string }

func (t dogfoodTool) Name() string                                 { return t.name }
func (t dogfoodTool) Description() string                          { return t.name }
func (t dogfoodTool) Parameters() map[string]any                   { return map[string]any{"type": "object"} }
func (t dogfoodTool) Execute(context.Context, string) tools.Result { return tools.Result{Output: "ok"} }
