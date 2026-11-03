package agent

import "strings"

// persona prompts guide each named persona's tool use and output style.
var personaPrompts = map[string]string{
	"coding": `You are a coding agent. You have access to file and shell tools.
Explore code cheaply: use grep to LOCATE what you need (it returns path:line matches),
then read_file with start_line/end_line for just those parts. Only read a whole file when
it is genuinely small or you need all of it — whole-file dumps bloat your context fast.
Use list_directory for structure, edit_file for surgical changes.
Run bash only for actual shell operations (build, test, run) — never to echo or print text you could write directly.
Output your analysis, findings, and explanations as plain text responses.`,

	"research": `You are a research agent. You have access to file and shell tools.
Use tools to gather information. Synthesize and output your findings as plain text.
Do not use bash to echo your analysis — write it directly as your response.`,

	"reasoning": `You are a reasoning agent. Think step-by-step and output your conclusions as plain text.
Use tools to gather information when needed, but never use bash just to display text.`,

	"general": `You are a general-purpose agent with access to file and shell tools.
Use tools for their intended purpose. Output responses as plain text.
Do not use bash to echo text — write it directly as your response.`,
}

// PersonaPrompt returns the system prompt for a named persona, falling back to
// the general prompt for unknown names.
func PersonaPrompt(persona string) string {
	if p, ok := personaPrompts[strings.ToLower(persona)]; ok {
		return p
	}
	return personaPrompts["general"]
}

// ComposeSystemPrompt builds the final system prompt. The order is:
//
//	prefix (model-specific directive, e.g. "/no_think")
//	persona base prompt
//	instructions (the harness's task brief; main agent only)
//	extra block (e.g. skill listing from shell mode)
//
// Blocks are separated by a blank line. Blank instructions add nothing, so the
// result is then identical to a prompt composed without them.
func ComposeSystemPrompt(persona, prefix, instructions, extra string) string {
	base := PersonaPrompt(persona)
	if prefix != "" {
		base = prefix + "\n" + base
	}
	if ins := strings.TrimSpace(instructions); ins != "" {
		base += "\n\n" + ins
	}
	if extra == "" {
		return base
	}
	return base + "\n\n" + extra
}

// toolLessPersonaPrompts is each persona's one-line form for an agent offered
// no tools at all: it states that and asks for plain text.
var toolLessPersonaPrompts = map[string]string{
	"coding":    "You are a coding agent. You have no tools: answer directly in plain text.",
	"research":  "You are a research agent. You have no tools: answer directly in plain text.",
	"reasoning": "You are a reasoning agent. You have no tools: think step-by-step and answer directly in plain text.",
	"general":   "You are a general-purpose agent. You have no tools: answer directly in plain text.",
}

// ToolLessPersonaPrompt returns the one-line persona prompt for a tool-less
// agent, falling back to the general one for unknown names.
func ToolLessPersonaPrompt(persona string) string {
	if p, ok := toolLessPersonaPrompts[strings.ToLower(persona)]; ok {
		return p
	}
	return toolLessPersonaPrompts["general"]
}

// ComposeToolLessSystemPrompt is ComposeSystemPrompt for an agent offered no
// tools: the tool-less persona line in place of the persona prompt, and no
// extra block, since every extra block describes a tool.
func ComposeToolLessSystemPrompt(persona, prefix, instructions string) string {
	base := ToolLessPersonaPrompt(persona)
	if prefix != "" {
		base = prefix + "\n" + base
	}
	if strings.TrimSpace(instructions) != "" {
		base += "\n\n" + instructions
	}
	return base
}
