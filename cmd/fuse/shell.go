package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/integrations/docket"
	"github.com/ethanhinson/fuse/internal/integrations/openspec"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/permissions"
	"github.com/ethanhinson/fuse/internal/secrets"
	"github.com/ethanhinson/fuse/internal/session"
	"github.com/ethanhinson/fuse/internal/skills"
	"github.com/ethanhinson/fuse/internal/tools"
	"github.com/ethanhinson/fuse/internal/tui"
)

// runShell loads skills, builds provider, and runs the interactive bubbletea
// TUI. It parses a minimal set of pre-flags (--model NAME, --verbose) and
// then hands control to the ShellModel. One-shot mode (cmd/fuse/main.go) is
// unaffected.
func runShell(args []string, cfg config.Config, reg *model.Registry, stdout, stderr io.Writer) int {
	alias := reg.Default
	verbose := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--model":
			if i+1 < len(args) {
				alias = args[i+1]
				i++
			}
		case "--verbose":
			verbose = true
		}
	}

	skillDirs := skills.DefaultDirs()

	set, err := skills.Load(skillDirs)
	if err != nil {
		fmt.Fprintf(stderr, "skills error: %v\n", err)
		return 1
	}
	skillBlock := set.SystemPromptBlock()

	toolReg, err := buildSessionRegistryNoMCP(cfg, set.Lookup)
	if err != nil {
		fmt.Fprintf(stderr, "session registry error: %v\n", err)
		return 1
	}

	builtins := tui.NewBuiltinProvider()

	skillProv, err := tui.NewSkillProvider(skillDirs)
	if err != nil {
		log.Printf("skill provider: %v", err)
		skillProv = nil
	}

	mcpProv, err := tui.NewMCPProvider(config.Path(), cfg, toolReg)
	if err != nil {
		log.Printf("mcp provider: %v", err)
		mcpProv = nil
	}

	var slashReg *tui.SlashRegistry
	if mcpProv != nil && skillProv != nil {
		slashReg = tui.NewSlashRegistry(builtins, skillProv, mcpProv)
	} else if mcpProv != nil {
		slashReg = tui.NewSlashRegistry(builtins, mcpProv)
	} else if skillProv != nil {
		slashReg = tui.NewSlashRegistry(builtins, skillProv)
	} else {
		slashReg = tui.NewSlashRegistry(builtins)
	}
	defer slashReg.Close()

	// Inject aggressive spawn_agent guidance (shared with one-shot mode).
	skillBlock += spawnAgentBlock

	traceFile := ""
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "--trace" {
			traceFile = args[i+1]
			break
		}
	}

	build := func(a string, r agent.Renderer, approve permissions.ApprovalFunc) (*agent.Agent, error) {
		return buildAgentWithRendererAndTrace(cfg, reg, a, r, verbose, skillBlock, toolReg, approve, traceFile)
	}

	glamourStyle := os.Getenv("GLAMOUR_STYLE")
	if glamourStyle == "" {
		glamourStyle = "dark"
	}

	// Session log: sweep stale files and open a fresh log.
	logDir := session.DefaultLogDir()
	go session.SweepOld(logDir, 7*24*time.Hour)
	sessLog, serr := session.NewLogger(logDir)
	if serr != nil {
		log.Printf("session log: %v", serr)
		sessLog = nil
	} else {
		defer sessLog.Close()
	}

	// Agent tree for subagent tracking.
	tree := agent.NewAgentTree(alias, alias)
	rootNode := tree.Node(tree.RootID())

	// Secrets store.
	var secretsStore secrets.SecretsStore
	switch cfg.Secrets.Store {
	case "sops":
		s, serr2 := secrets.NewSopsSecretsStore(cfg.Secrets.SopsFile, "")
		if serr2 != nil {
			log.Printf("secrets: sops store: %v; falling back to env", serr2)
			secretsStore = &secrets.EnvSecretsStore{}
		} else {
			secretsStore = s
		}
	default:
		secretsStore = &secrets.EnvSecretsStore{}
	}
	tree.SetSecrets(secretsStore)

	// Remote executor and intent plugin from config.
	if cfg.RemoteExecutor.URL != "" {
		exec := &agent.SSERemoteExecutor{
			BaseURL:     cfg.RemoteExecutor.URL,
			TokenSecret: cfg.RemoteExecutor.TokenSecret,
			HTTPClient:  http.DefaultClient,
			PublicKey:   cfg.RemoteExecutor.PublicKey,
		}
		tree.RegisterRemote("", exec)
	}
	if cfg.RemoteExecutor.IntentPlugin.Kind != "" {
		tree.RegisterIntent("", buildIntentPlugin(cfg.RemoteExecutor.IntentPlugin))
	}

	// ch is declared here so the ChildBuilder closure can capture it.
	// It is assigned from m.Channel() after NewShellModel returns.
	var ch chan tea.Msg

	// SpawnFunc factory — self-referential so child agents get their own spawner.
	var makeSpawnFunc func(parentNode *agent.AgentNode, parentDepth int) tools.SpawnFunc
	makeSpawnFunc = func(parentNode *agent.AgentNode, parentDepth int) tools.SpawnFunc {
		spawner := agent.NewSpawner(
			agent.WithTree(tree),
			agent.WithNode(parentNode),
			agent.WithSpawnDepth(parentDepth),
			agent.WithChildBuilder(func(ctx context.Context, opts agent.SpawnOpts, childNode *agent.AgentNode, childTree *agent.AgentTree) (string, error) {
				// Child-specific tool registry (clone or subset).
				var childToolReg *tools.Registry
				if len(opts.Tools) > 0 {
					childToolReg, _ = toolReg.Subset(opts.Tools)
				} else {
					childToolReg = toolReg.Clone()
				}
				// Replace spawn_agent with one wired to the child's spawner.
				childToolReg.Register(tools.NewSpawnAgentTool(makeSpawnFunc(childNode, childNode.Depth)))

				r := tui.NewNodeRenderer(childNode, childTree)
				approve := tui.NewTeaApprovalFunc(ch) // ch assigned before first Run

				modelAlias := opts.ModelID
				if modelAlias == "" {
					modelAlias = alias
				}

				var a *agent.Agent
				var aerr error
				if opts.SystemPrompt != "" {
					a, aerr = buildChildAgent(cfg, reg, modelAlias, r, opts.SystemPrompt, childToolReg, approve)
				} else {
					a, aerr = buildAgentWithRenderer(cfg, reg, modelAlias, r, verbose, skillBlock, childToolReg, approve)
				}
				if aerr != nil {
					return "", aerr
				}

				history := []model.Message{{Role: "user", Content: opts.Task}}
				msgs, rerr := a.Run(ctx, history)

				if sessLog != nil {
					kind := "done"
					if rerr != nil {
						kind = "error"
					}
					_ = sessLog.Write(session.LogEntry{
						TS:       time.Now(),
						NodeID:   childNode.ID,
						ParentID: childNode.ParentID,
						Label:    childNode.Label,
						Depth:    childNode.Depth,
						Kind:     kind,
					})
				}

				if rerr != nil {
					return "", rerr
				}
				return lastAssistantText(msgs), nil
			}),
		)

		return func(ctx context.Context, label, task, systemPrompt, modelID, remoteID, intentPlugin string, toolsList []string, remote bool) (string, error) {
			opts := agent.SpawnOpts{
				Label:          label,
				Task:           task,
				SystemPrompt:   systemPrompt,
				ModelID:        modelID,
				Remote:         remote,
				RemoteID:       remoteID,
				IntentPluginID: intentPlugin,
				Tools:          toolsList,
			}
			handle, herr := spawner.Spawn(ctx, opts)
			if herr != nil {
				return "", herr
			}
			done := handle.Wait()
			return done.Result, done.Err
		}
	}

	// Register spawn_agent in the tool registry before any agent runs.
	toolReg.Register(tools.NewSpawnAgentTool(makeSpawnFunc(rootNode, 0)))

	m := tui.NewShellModel(alias, verbose, glamourStyle, reg, slashReg, build)
	ch = m.Channel() // assign ch; closure captures the variable, not a snapshot
	m = m.WithTree(tree)

	// Start the 250ms dirty-node flusher.
	flushCtx, cancelFlusher := context.WithCancel(context.Background())
	defer cancelFlusher()
	tree.StartDirtyFlusher(flushCtx)

	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion(), tea.WithOutput(stdout))
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(stderr, "tui error: %v\n", err)
		return 1
	}
	return 0
}

// buildIntentPlugin constructs the IntentPlugin from config.
func buildIntentPlugin(cfg config.IntentPluginConfig) agent.IntentPlugin {
	token := os.ExpandEnv(cfg.GitToken)
	switch cfg.Kind {
	case "docket":
		return &docket.DocketIntentPlugin{
			GitRemoteURL:      cfg.GitRemoteURL,
			GitToken:          token,
			Image:             cfg.Image,
			IntegrationBranch: cfg.IntegrationBranch,
		}
	case "openspec":
		return &openspec.OpenSpecIntentPlugin{
			GitRemoteURL:      cfg.GitRemoteURL,
			GitToken:          token,
			Image:             cfg.Image,
			IntegrationBranch: cfg.IntegrationBranch,
		}
	default:
		return agent.NilIntentPlugin{}
	}
}
