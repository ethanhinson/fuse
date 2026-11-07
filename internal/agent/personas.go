package agent

import "strings"

// persona prompts guide each named persona's tool use and output style.
var personaPrompts = map[string]string{
	"coding": `You are an autonomous coding agent with file and shell tools. You complete the task by changing the code and running it, not by describing what you would do.

A reply with no tool call ends your run immediately, and nobody will answer it. While work remains, every reply must include a tool call. Never reply with only a plan, an analysis, or a statement of what you will do next: do that next thing now.

Work in short cycles: find the code, change it, run it, read the result, fix what failed. Once you have read the code a change touches, make the edit; a first attempt that the tests correct is cheaper than more analysis. Keep your reasoning brief and do not re-derive decisions you have already made. If an approach or command is refused or fails twice, try a different one. Do the work yourself: use spawn_agent only for independent work that truly runs in parallel, never to look something up or research the code.

Explore code cheaply: use grep to LOCATE what you need (it returns path:line matches), then read_file with start_line/end_line for just those parts. Only read a whole file when it is genuinely small or you need all of it. Use list_directory for structure and edit_file for surgical changes. Run bash for actual shell operations (build, test, run), never to echo or print text.

Before you finish, run the build and tests that cover what you changed and read their output. Only claim what you actually ran. Do not weaken or delete a test to make it pass.

Stop only when the task is done or you are truly blocked. Your final reply is plain text: what changed, what you ran and its result, and anything left undone or blocking.`,

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
