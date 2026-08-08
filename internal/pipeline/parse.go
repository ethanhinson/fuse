package pipeline

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Parse decodes a Pipeline from YAML or JSON bytes. JSON is a subset of YAML, so
// a single YAML decode handles both; the input is sniffed only to give JSON a
// dedicated decoder path (stricter number handling) when it clearly looks like
// JSON. The `on_error` scalar (fail | skip | retry(N)) is parsed separately so a
// malformed value can name the offending step.
func Parse(data []byte) (*Pipeline, error) {
	if looksLikeJSON(data) {
		return parseJSON(data)
	}
	return parseYAML(data)
}

func looksLikeJSON(data []byte) bool {
	t := bytes.TrimSpace(data)
	return len(t) > 0 && (t[0] == '{' || t[0] == '[')
}

// wireStep mirrors Step but takes on_error as a raw string so we can parse it
// with step context. All other fields decode directly.
type wireStep struct {
	Name       string      `yaml:"name" json:"name"`
	Worker     string      `yaml:"worker" json:"worker"`
	Prompt     string      `yaml:"prompt" json:"prompt"`
	Inputs     []string    `yaml:"inputs" json:"inputs"`
	Outputs    []string    `yaml:"outputs" json:"outputs"`
	DependsOn  []string    `yaml:"depends_on" json:"depends_on"`
	Fanout     int         `yaml:"fanout" json:"fanout"`
	Expects    rawExpects  `yaml:"expects" json:"expects"`
	OnError    string      `yaml:"on_error" json:"on_error"`
	Conditions []Condition `yaml:"conditions" json:"conditions"`
	Default    string      `yaml:"default" json:"default"`
}

type wirePipeline struct {
	Name     string     `yaml:"name" json:"name"`
	Workflow string     `yaml:"workflow" json:"workflow"`
	Steps    []wireStep `yaml:"steps" json:"steps"`
}

func parseYAML(data []byte) (*Pipeline, error) {
	var w wirePipeline
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(false)
	if err := dec.Decode(&w); err != nil {
		return nil, fmt.Errorf("pipeline: yaml decode: %w", err)
	}
	return fromWire(w)
}

func parseJSON(data []byte) (*Pipeline, error) {
	var w wirePipeline
	if err := json.Unmarshal(data, &w); err != nil {
		return nil, fmt.Errorf("pipeline: json decode: %w", err)
	}
	return fromWire(w)
}

func fromWire(w wirePipeline) (*Pipeline, error) {
	p := &Pipeline{Name: w.Name, Workflow: w.Workflow}
	p.Steps = make([]Step, 0, len(w.Steps))
	for _, ws := range w.Steps {
		policy, err := parseOnError(ws.OnError, ws.Name)
		if err != nil {
			return nil, err
		}
		p.Steps = append(p.Steps, Step{
			Name:       ws.Name,
			Worker:     ws.Worker,
			Prompt:     ws.Prompt,
			Inputs:     ws.Inputs,
			Outputs:    ws.Outputs,
			DependsOn:  ws.DependsOn,
			Fanout:     ws.Fanout,
			Expects:    normalizeExpects(json.RawMessage(ws.Expects)),
			OnError:    policy,
			Conditions: ws.Conditions,
			Default:    ws.Default,
		})
	}
	return p, nil
}

// rawExpects captures the `expects` JSON Schema from either format as raw JSON
// bytes. YAML decodes to a generic value then re-marshals to JSON (yaml.v3
// cannot decode a map directly into json.RawMessage); JSON stores the raw slice.
type rawExpects json.RawMessage

func (r *rawExpects) UnmarshalYAML(unmarshal func(any) error) error {
	var v any
	if err := unmarshal(&v); err != nil {
		return err
	}
	if v == nil {
		*r = nil
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	*r = rawExpects(b)
	return nil
}

func (r *rawExpects) UnmarshalJSON(data []byte) error {
	*r = rawExpects(append([]byte(nil), data...))
	return nil
}

// normalizeExpects drops an empty/"null" raw message so absent schemas stay nil.
func normalizeExpects(raw json.RawMessage) json.RawMessage {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || string(t) == "null" {
		return nil
	}
	return raw
}

// parseOnError converts the on_error scalar into an ErrorPolicy. An empty scalar
// defaults to fail. Malformed values (including retry(0) and non-positive/
// non-numeric retry counts) return an error naming the step.
func parseOnError(scalar, step string) (ErrorPolicy, error) {
	s := strings.TrimSpace(scalar)
	switch s {
	case "", "fail":
		return ErrorPolicy{Kind: ErrorFail}, nil
	case "skip":
		return ErrorPolicy{Kind: ErrorSkip}, nil
	}
	if inner, ok := strings.CutPrefix(s, "retry("); ok {
		if arg, ok := strings.CutSuffix(inner, ")"); ok {
			n, err := strconv.Atoi(strings.TrimSpace(arg))
			if err == nil && n >= 1 {
				return ErrorPolicy{Kind: ErrorRetry, Retries: n}, nil
			}
		}
		return ErrorPolicy{}, fmt.Errorf("pipeline: step %q: malformed on_error %q (retry needs a positive integer, e.g. retry(3))", step, scalar)
	}
	return ErrorPolicy{}, fmt.Errorf("pipeline: step %q: unknown on_error %q (want fail, skip, or retry(N))", step, scalar)
}
