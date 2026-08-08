// Package pipeline implements deterministic DAG execution over the subagent
// runtime (change 0026). A Pipeline is a static, declarative graph of Steps; the
// engine (Run) drives it readiness-first over a real agent.Spawner and shares
// state through an agent.Blackboard. Pipelines are authored as YAML or JSON, or
// synthesized from a natural-language goal (Synthesize).
package pipeline

import (
	"encoding/json"
	"strconv"
)

// Pipeline is a named, static DAG of Steps. Workflow optionally binds the run to
// a workflow pool; empty means the global/session pool.
type Pipeline struct {
	Name     string `yaml:"name" json:"name"`
	Workflow string `yaml:"workflow,omitempty" json:"workflow,omitempty"`
	Steps    []Step `yaml:"steps" json:"steps"`
}

// Step is one node in the pipeline DAG. It spawns one (or Fanout) child agents,
// resolving Inputs from the blackboard into the Prompt and writing results to
// Outputs, then routes to the next step via Conditions/Default.
type Step struct {
	Name    string   `yaml:"name" json:"name"`
	Worker  string   `yaml:"worker,omitempty" json:"worker,omitempty"`
	Prompt  string   `yaml:"prompt,omitempty" json:"prompt,omitempty"`
	Inputs  []string `yaml:"inputs,omitempty" json:"inputs,omitempty"`
	Outputs []string `yaml:"outputs,omitempty" json:"outputs,omitempty"`
	// DependsOn lists step names that must complete before this step is ready.
	DependsOn []string `yaml:"depends_on,omitempty" json:"depends_on,omitempty"`
	// Fanout N spawns N parallel instances (0/1 => a single instance). With
	// glob-namespaced outputs (e.g. "hits/*") each instance writes hits/0..N-1.
	Fanout int `yaml:"fanout,omitempty" json:"fanout,omitempty"`
	// Expects is an optional JSON Schema (change 0024) the step's result should
	// conform to; a match lands the structured value, a mismatch degrades to text.
	Expects json.RawMessage `yaml:"expects,omitempty" json:"expects,omitempty"`
	// OnError selects the failure policy for this step (default fail).
	OnError ErrorPolicy `yaml:"on_error,omitempty" json:"on_error,omitempty"`
	// Conditions route to the next step; the first match's Goto wins. A step with
	// conditions (or a default) is a router: its chosen target runs and its other
	// targets are skipped (change 0026 — routing affects execution, see engine.Run).
	Conditions []Condition `yaml:"conditions,omitempty" json:"conditions,omitempty"`
	// Default is the next step when no condition matches (empty => no branch is
	// taken; a target reachable only via this router is then skipped).
	Default string `yaml:"default,omitempty" json:"default,omitempty"`
}

// Condition is one routing rule: if the blackboard Key relates to Value under Op,
// route to Goto. It is authored under the `if:` key in YAML/JSON.
type Condition struct {
	Key   string `yaml:"-" json:"-"`
	Op    string `yaml:"-" json:"-"`
	Value any    `yaml:"-" json:"-"`
	Goto  string `yaml:"goto" json:"goto"`
}

// condIf is the nested `if:{key,op,value}` shape a Condition serializes to.
type condIf struct {
	Key   string `yaml:"key" json:"key"`
	Op    string `yaml:"op" json:"op"`
	Value any    `yaml:"value" json:"value"`
}

// condWire is the on-the-wire shape of a Condition: an `if` object plus `goto`.
type condWire struct {
	If   condIf `yaml:"if" json:"if"`
	Goto string `yaml:"goto" json:"goto"`
}

// MarshalYAML renders a Condition as {if:{key,op,value}, goto}.
func (c Condition) MarshalYAML() (any, error) {
	return condWire{If: condIf{Key: c.Key, Op: c.Op, Value: c.Value}, Goto: c.Goto}, nil
}

// UnmarshalYAML reads the {if:{key,op,value}, goto} shape into a Condition.
func (c *Condition) UnmarshalYAML(unmarshal func(any) error) error {
	var w condWire
	if err := unmarshal(&w); err != nil {
		return err
	}
	c.Key, c.Op, c.Value, c.Goto = w.If.Key, w.If.Op, w.If.Value, w.Goto
	return nil
}

// MarshalJSON renders a Condition as {"if":{key,op,value}, "goto"}.
func (c Condition) MarshalJSON() ([]byte, error) {
	return json.Marshal(condWire{If: condIf{Key: c.Key, Op: c.Op, Value: c.Value}, Goto: c.Goto})
}

// UnmarshalJSON reads the {"if":{...}, "goto"} shape into a Condition.
func (c *Condition) UnmarshalJSON(data []byte) error {
	var w condWire
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	c.Key, c.Op, c.Value, c.Goto = w.If.Key, w.If.Op, w.If.Value, w.Goto
	return nil
}

// ErrorKind enumerates the failure policies for a Step.
type ErrorKind int

const (
	// ErrorFail stops the pipeline and marks it failed (the default).
	ErrorFail ErrorKind = iota
	// ErrorSkip records the failure, leaves outputs absent, and continues.
	ErrorSkip
	// ErrorRetry re-spawns the step up to Retries times before failing.
	ErrorRetry
)

// ErrorPolicy models the `on_error` scalar: fail | skip | retry(N). Retries is
// meaningful only for ErrorRetry (N >= 1).
type ErrorPolicy struct {
	Kind    ErrorKind
	Retries int
}

// MarshalYAML renders an ErrorPolicy back to its scalar form.
func (p ErrorPolicy) MarshalYAML() (any, error) { return p.scalar(), nil }

// MarshalJSON renders an ErrorPolicy back to its scalar string form.
func (p ErrorPolicy) MarshalJSON() ([]byte, error) { return json.Marshal(p.scalar()) }

func (p ErrorPolicy) scalar() string {
	switch p.Kind {
	case ErrorSkip:
		return "skip"
	case ErrorRetry:
		return "retry(" + strconv.Itoa(p.Retries) + ")"
	default:
		return "fail"
	}
}
