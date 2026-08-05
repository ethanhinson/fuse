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
//	extra block (e.g. skill listing from shell mode)
func ComposeSystemPrompt(persona, prefix, extra string) string {
	base := PersonaPrompt(persona)
	if prefix != "" {
		base = prefix + "\n" + base
	}
	if extra == "" {
		return base
	}
	return base + "\n\n" + extra
}
