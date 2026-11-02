package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ethanhinson/fuse/internal/agent"
)

// SynthRole is one role a synthesized pipeline may assign a step to: its name
// and the description the synthesizer chooses by.
type SynthRole struct {
	Name        string
	Description string
}

// SynthContext carries the palette a synthesized pipeline may draw from: the
// available worker types, roles and tool names. All are advisory context
// injected into the synthesis prompt; a step naming a role outside Roles is
// rejected and re-asked.
type SynthContext struct {
	Workers []string
	Roles   []SynthRole
	Tools   []string
}

// defaultSynthAttempts is the re-ask budget used when Caps.MaxAttempts is unset
// (0). It bounds the parse/validate feedback loop.
const defaultSynthAttempts = 3

// Synthesize turns a natural-language goal into a validated Pipeline by driving a
// synthesis agent through sp. It attaches the Pipeline JSON schema as the spawn's
// Expects, parses the reply (structured value or text), and Validate()s it under
// caps. On a parse or validation rejection it re-spawns with the error appended,
// up to caps.MaxAttempts (defaulting to defaultSynthAttempts when unset). It fails
// loudly — returning an error, never an invalid or over-cap Pipeline — once the
// attempt budget is exhausted.
func Synthesize(ctx context.Context, goal string, sc SynthContext, sp *agent.Spawner, caps Caps) (*Pipeline, error) {
	attempts := caps.MaxAttempts
	if attempts <= 0 {
		attempts = defaultSynthAttempts
	}

	roleNames := synthRoleNames(sc.Roles)
	schema := pipelineJSONSchema(roleNames)
	baseSystem := synthesisSystemPrompt(sc)

	var lastErr error
	for try := 0; try < attempts; try++ {
		task := goal
		if lastErr != nil {
			task = goal + "\n\nYour previous attempt was rejected: " + lastErr.Error() +
				"\nReturn a corrected pipeline that fixes this."
		}

		opts := agent.SpawnOpts{
			Label:        "synthesize",
			Task:         task,
			SystemPrompt: baseSystem,
			Expects:      schema,
		}
		h, err := sp.Spawn(ctx, opts)
		if err != nil {
			return nil, fmt.Errorf("pipeline: synthesis spawn failed: %w", err)
		}

		raw, text := synthResult(h)

		p, perr := parseSynth(raw, text)
		if perr != nil {
			lastErr = perr
			continue
		}
		if rerr := CheckRoles(p, roleNames); rerr != nil {
			lastErr = rerr
			continue
		}
		if verr := Validate(p, caps); verr != nil {
			lastErr = verr
			continue
		}
		return p, nil
	}
	return nil, fmt.Errorf("pipeline: synthesis failed after %d attempt(s): %w", attempts, lastErr)
}

// synthRoleNames returns the palette's role names in palette order.
func synthRoleNames(roles []SynthRole) []string {
	if len(roles) == 0 {
		return nil
	}
	out := make([]string, len(roles))
	for i, r := range roles {
		out[i] = r.Name
	}
	return out
}

// synthResult extracts the child's structured value (when it matched the schema)
// and the raw result text. Either may be used to parse the pipeline.
func synthResult(h agent.AgentHandle) (structured any, text string) {
	s, err := h.Result()
	if err == nil {
		structured = s
	}
	text = h.Wait().Result
	return structured, text
}

// parseSynth parses a Pipeline from the child's structured value if present,
// otherwise from its raw text. The structured value (a decoded JSON object) is
// re-marshaled to JSON and parsed through the same path as text so validation is
// identical either way.
func parseSynth(structured any, text string) (*Pipeline, error) {
	if structured != nil {
		if b, err := json.Marshal(structured); err == nil {
			if p, perr := Parse(b); perr == nil {
				return p, nil
			}
		}
	}
	return Parse([]byte(text))
}

// synthesisSystemPrompt builds the fixed instruction block: the JSON schema
// description plus the available workers and tools.
func synthesisSystemPrompt(sc SynthContext) string {
	var b strings.Builder
	b.WriteString("You are a pipeline synthesizer. Produce a single JSON object describing a ")
	b.WriteString("deterministic DAG pipeline that accomplishes the user's goal.\n\n")
	b.WriteString("The pipeline JSON schema:\n")
	b.WriteString(pipelineSchemaDescription(len(sc.Roles) > 0))
	b.WriteString("\n")
	if len(sc.Workers) > 0 {
		b.WriteString("Available worker types: ")
		b.WriteString(strings.Join(sc.Workers, ", "))
		b.WriteString("\n")
	}
	if len(sc.Roles) > 0 {
		b.WriteString("Available roles (set a step's role to one of these instead of worker):\n")
		for _, r := range sc.Roles {
			b.WriteString("- " + r.Name + " — " + strings.Join(strings.Fields(r.Description), " ") + "\n")
		}
	}
	if len(sc.Tools) > 0 {
		b.WriteString("Available tools: ")
		b.WriteString(strings.Join(sc.Tools, ", "))
		b.WriteString("\n")
	}
	b.WriteString("\nRespond with ONLY the JSON object, no prose.")
	return b.String()
}

// pipelineSchemaDescription is a compact human/LLM-readable description of the
// pipeline shape, embedded in the synthesis system prompt. withRoles adds the
// step's role field after its worker field; without it the text is unchanged.
func pipelineSchemaDescription(withRoles bool) string {
	if !withRoles {
		return pipelineSchemaText
	}
	return strings.Replace(pipelineSchemaText, schemaWorkerLine, schemaWorkerLine+schemaRoleLine, 1)
}

const (
	schemaWorkerLine = `      "worker": string (one of the available worker types),` + "\n"
	schemaRoleLine   = `      "role": string (one of the available roles; never together with worker),` + "\n"
)

// pipelineSchemaText is the role-free pipeline shape description.
const pipelineSchemaText = `{
  "name": string,
  "workflow": string (optional workflow pool),
  "steps": [
    {
      "name": string (unique),
      "worker": string (one of the available worker types),
      "prompt": string (may contain {{key}} placeholders filled from inputs),
      "inputs": [string] (blackboard keys, globs allowed),
      "outputs": [string] (blackboard keys the result is written to),
      "depends_on": [string] (names of steps that must finish first),
      "fanout": int (N parallel instances; omit or 1 for a single instance),
      "on_error": "fail" | "skip" | "retry(N)",
      "conditions": [ {"if": {"key": string, "op": "exists|eq|ne|gt|lt|contains|matches", "value": any}, "goto": string} ],
      "default": string (next step when no condition matches)
    }
  ]
}
The graph must be acyclic over depends_on and goto edges; every depends_on/goto/default must name a real step.`

// pipelineJSONSchema returns a JSON Schema (as a decoded map) for the Pipeline
// shape, used as the spawn's Expects so the child is asked for conforming JSON.
// A non-empty roles adds the step's role field; without roles the schema is
// unchanged.
func pipelineJSONSchema(roles []string) map[string]any {
	stepProps := map[string]any{
		"name":       map[string]any{"type": "string"},
		"worker":     map[string]any{"type": "string"},
		"prompt":     map[string]any{"type": "string"},
		"inputs":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		"outputs":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		"depends_on": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		"fanout":     map[string]any{"type": "integer"},
		"on_error":   map[string]any{"type": "string"},
		"default":    map[string]any{"type": "string"},
	}
	if len(roles) > 0 {
		stepProps["role"] = map[string]any{"type": "string"}
	}
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name":     map[string]any{"type": "string"},
			"workflow": map[string]any{"type": "string"},
			"steps": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type":       "object",
					"properties": stepProps,
					"required":   []any{"name"},
				},
			},
		},
		"required": []any{"name", "steps"},
	}
}
