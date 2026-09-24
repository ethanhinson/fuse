package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/skills"
	"github.com/ethanhinson/fuse/internal/tools"
)

// installSkillPreflight wires agent.SkillPreflight: one constrained model call
// before the first turn that decides which listed skill, if any, the request
// is for. The model is offered a single select_skill tool whose `skill`
// argument is an enum of the listed names plus "none", with tool choice
// required, so it cannot answer with prose or skip the decision. Only a
// "high" confidence pick loads the skill. Installed only when the skill tool is registered
// and enabled and at least one skill is listed; `skill_select: off` disables
// it. See docs/results/2026-09-24-skill-trigger-probes.md for why the
// system-prompt directive alone is not enough on open-weight models.
func installSkillPreflight(a *agent.Agent, cfg config.Config, modelID string, traceW io.Writer, gate model.RateGate, toolReg *tools.Registry) {
	if cfg.SkillSelect == "off" || toolReg == nil || !toolReg.Has("skill") || toolDisabled(cfg, "skill") {
		return
	}
	set, err := skills.LoadWithEmbedded(skills.DefaultDirs())
	if err != nil || set == nil || len(set.All()) == 0 {
		return
	}
	adapter := gatewayAdapter(cfg, gate)
	if traceW != nil {
		adapter = adapter.WithTraceLabel(traceW, "skill-select")
	}
	a.SkillPreflight = skillSelector(adapter, modelID, set.All())
}

// skillSelector returns the preflight function for a given skill list.
func skillSelector(m agent.Completer, modelID string, list []skills.Skill) func(context.Context, string) (string, error) {
	names := make([]string, 0, len(list)+1)
	var listing strings.Builder
	listing.WriteString("Available skills:\n")
	for _, sk := range list {
		names = append(names, sk.Name)
		fmt.Fprintf(&listing, "- %s: %s\n", sk.Name, sk.Description)
	}
	enum := append(append([]string{}, names...), "none")
	tool := model.ToolSchema{
		Name: "select_skill",
		Description: "Decide whether the user's request is the kind of task one of the listed skills is written for. " +
			"A skill matches only when the request is clearly that kind of task, not merely related to it or in the same domain. " +
			"If no skill clearly matches, or you are unsure, choose \"none\".",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"skill":      map[string]any{"type": "string", "enum": enum},
				"confidence": map[string]any{"type": "string", "enum": []string{"high", "medium", "low"}, "description": "How clearly the request is that skill's kind of task."},
				"why":        map[string]any{"type": "string", "description": "One sentence: which words of the request match the skill's description, or why none does."},
			},
			"required": []string{"skill", "confidence", "why"},
		},
	}
	system := "You route requests to skills. " + listing.String()
	return func(ctx context.Context, request string) (string, error) {
		resp, err := m.Complete(ctx, model.CompletionReq{
			Model:      modelID,
			Messages:   []model.Message{{Role: "system", Content: system}, {Role: "user", Content: request}},
			Tools:      []model.ToolSchema{tool},
			MaxTokens:  2048,
			ToolChoice: "required",
		})
		if err != nil {
			return "", err
		}
		for _, tc := range resp.ToolCalls {
			if tc.Name != "select_skill" {
				continue
			}
			var pick struct {
				Skill      string `json:"skill"`
				Confidence string `json:"confidence"`
			}
			if err := json.Unmarshal([]byte(tc.Arguments), &pick); err != nil {
				return "", fmt.Errorf("select_skill arguments: %w", err)
			}
			// Only a confident match loads a skill. Probed 2026-09-24: true matches
			// came back "high" on every model (24/24); the near-miss request (a Go
			// unit-test task against a competitive-programming skill) came back
			// none/low or ledger/medium on DeepSeek, so "medium" is where the false
			// positives live. MiniMax still said high on that near miss; the fix
			// there is the skill description saying what the skill is NOT for.
			if pick.Skill == "" || pick.Skill == "none" || pick.Confidence != "high" {
				return "", nil
			}
			for _, n := range names {
				if n == pick.Skill {
					return n, nil
				}
			}
			return "", nil // not a listed name; never load something that was not offered
		}
		return "", nil // no tool call: treat as none rather than fail the run
	}
}
