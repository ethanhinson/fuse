package model

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strings"
)

// Qwen3-Coder (and the Qwen3 family behind LM Studio, Ollama, and several
// OpenRouter providers) emits tool calls in an XML dialect:
//
//	<tool_call>
//	<function=read_file>
//	<parameter=path>
//	task.md
//	</parameter>
//	</function>
//	</tool_call>
//
// A serving stack is supposed to parse that into the OpenAI `tool_calls`
// field. When its parser fails (observed 2026-09-19 on roughly one first turn
// in seven through OpenRouter: the provider consumed the opening <tool_call>
// tag, choked on the rest, and streamed the remainder as plain content) the
// reply reaches us as text with no tool call. The agent loop reads "no tool
// call" as "the model is done", so the run ends after one turn with nothing
// written. recoverXMLToolCalls is the fallback: it lifts such calls out of
// content into ToolCalls so the loop proceeds exactly as if the provider had
// parsed them.
//
// It runs only when the response carried no structured tool call and the
// request advertised tools (the model was in tool-calling mode), and it lifts
// every well-formed <function=NAME>...</function> block. NAME is not checked
// against the advertised tools: a provider's parser would not check either,
// and the registry answers an unknown name with an error result the model can
// correct on its next turn, which is far better than ending the run. Prose
// that merely mentions the syntax has no closing tag and is left alone.

var (
	xmlFunctionRe  = regexp.MustCompile(`(?s)<function=([\w.\-]+)>(.*?)</function>`)
	xmlParameterRe = regexp.MustCompile(`(?s)<parameter=([\w.\-]+)>(.*?)</parameter>`)
)

// recoverXMLToolCalls returns the content with any recovered call blocks
// removed, the recovered calls in document order, and whether anything was
// recovered. It returns the input untouched when tools is empty or content
// holds no well-formed block.
func recoverXMLToolCalls(content string, tools []ToolSchema) (string, []ToolCall, bool) {
	if len(tools) == 0 || !strings.Contains(content, "<function=") {
		return content, nil, false
	}
	byName := make(map[string]ToolSchema, len(tools))
	for _, t := range tools {
		byName[t.Name] = t
	}

	matches := xmlFunctionRe.FindAllStringSubmatchIndex(content, -1)
	var calls []ToolCall
	var stripped strings.Builder
	last := 0
	for _, m := range matches {
		name := content[m[2]:m[3]]
		schema := byName[name] // zero value for an unadvertised name: params stay strings
		args := map[string]any{}
		for _, p := range xmlParameterRe.FindAllStringSubmatch(content[m[4]:m[5]], -1) {
			args[p[1]] = coerceXMLParam(unwrapXMLValue(p[2]), paramType(schema, p[1]))
		}
		raw, err := json.Marshal(args)
		if err != nil {
			continue
		}
		calls = append(calls, ToolCall{ID: recoveredCallID(), Name: name, Arguments: string(raw)})
		stripped.WriteString(content[last:m[0]])
		last = m[1]
	}
	if len(calls) == 0 {
		return content, nil, false
	}
	stripped.WriteString(content[last:])
	return cleanRecoveredContent(stripped.String()), calls, true
}

// unwrapXMLValue undoes the chat template's framing: it wraps each value in
// exactly one newline on each side, so exactly one is removed. Anything
// further (a trailing newline inside file content, say) is the value.
func unwrapXMLValue(v string) string {
	v = strings.TrimPrefix(v, "\n")
	return strings.TrimSuffix(v, "\n")
}

// paramType reads the declared JSON Schema type of one parameter, or "" when
// the schema does not say.
func paramType(schema ToolSchema, param string) string {
	props, _ := schema.Parameters["properties"].(map[string]any)
	spec, _ := props[param].(map[string]any)
	t, _ := spec["type"].(string)
	return t
}

// coerceXMLParam turns the raw text of a parameter into the JSON value its
// schema declares. XML carries no types, so a non-string parameter is parsed
// as JSON when it is valid JSON; otherwise the text is kept as a string and
// the tool reports the bad argument itself, as it would for any malformed
// call.
func coerceXMLParam(text, typ string) any {
	switch typ {
	case "", "string":
		return text
	}
	var v any
	if err := json.Unmarshal([]byte(strings.TrimSpace(text)), &v); err == nil {
		return v
	}
	return text
}

// cleanRecoveredContent removes the wrapper tags a provider may have left
// around the lifted blocks (a dangling </tool_call> is the common leftover)
// and trims the result, so the assistant message carries just the prose.
func cleanRecoveredContent(s string) string {
	s = strings.ReplaceAll(s, "<tool_call>", "")
	s = strings.ReplaceAll(s, "</tool_call>", "")
	return strings.TrimSpace(s)
}

// recoveredCallID mints an id for a lifted call. The gateway never saw the
// call as structured, so there is no upstream id; the conversation replay
// only needs it to be unique so tool results pair with their call.
func recoveredCallID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "call_recovered"
	}
	return "call_recovered_" + hex.EncodeToString(b[:])
}
