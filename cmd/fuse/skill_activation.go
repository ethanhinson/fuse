package main

import (
	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/skills"
)

// installSkillActivation wires the deterministic skill-routing layer: skills
// that declare `activation:` triggers (always, phrases, paths) get their body
// attached when a trigger fires on the request or on a tool call's paths. It
// needs no skill tool and makes no model call; `skill_activation: off`
// disables it. Skills without triggers keep today's model-invoked path.
func installSkillActivation(a *agent.Agent, cfg config.Config) {
	if cfg.SkillActivation == "off" {
		return
	}
	set, err := skills.LoadWithEmbedded(skills.DefaultDirs())
	if err != nil || set == nil {
		return
	}
	act := skills.NewActivator(set)
	if act == nil {
		return
	}
	a.SkillActivator = activatorAdapter{act}
}

// activatorAdapter maps skills.Match onto agent.SkillAttachment so the agent
// package never imports skills.
type activatorAdapter struct{ act *skills.Activator }

func (ad activatorAdapter) OnRequest(request string) []agent.SkillAttachment {
	return toAttachments(ad.act.OnRequest(request))
}

func (ad activatorAdapter) OnToolCall(name, arguments string) []agent.SkillAttachment {
	return toAttachments(ad.act.OnToolCall(name, arguments))
}

func toAttachments(ms []skills.Match) []agent.SkillAttachment {
	out := make([]agent.SkillAttachment, 0, len(ms))
	for _, m := range ms {
		out = append(out, agent.SkillAttachment{Name: m.Skill.Name, Reason: m.Reason, Body: m.Skill.Body})
	}
	return out
}
