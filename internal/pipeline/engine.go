package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/ethanhinson/fuse/internal/agent"
)

// RunState is the terminal state of a pipeline run.
type RunState int

const (
	// StateCompleted means every reachable step finished (skips included).
	StateCompleted RunState = iota
	// StateFailed means a step failed under a fail/exhausted-retry policy.
	StateFailed
)

func (s RunState) String() string {
	if s == StateFailed {
		return "failed"
	}
	return "completed"
}

// Status is the terminal status of a Run. FailedStep names the offending step
// when State is StateFailed (empty otherwise).
type Status struct {
	State      RunState
	FailedStep string
}

// statusKey is the blackboard key the engine always writes the terminal status to.
const statusKey = "pipeline.status"

// writerID/writerLabel stamp blackboard writes the engine makes on behalf of the
// pipeline itself (step outputs and the terminal status).
const (
	engineWriterID    = "pipeline"
	engineWriterLabel = "pipeline"
)

// Run executes pipeline p over spawner sp, sharing state through blackboard bb.
// It is readiness-driven: every step whose depends_on are all complete is
// launched concurrently, each step instance being one sp.Spawn call. Inputs are
// resolved from the blackboard (glob-expanded) and substituted into the prompt as
// {{key}}; outputs are written back (a literal key for a single instance, or
// glob-namespaced keys for a fanout). Conditions/Default route after each step.
// The terminal status is always written to pipeline.status. Run returns a
// non-nil error only when the pipeline failed (State == StateFailed).
//
// The coordinator uses plain goroutines and never itself holds a scheduler slot
// while awaiting a step's child spawns — only the spawned children consume slots
// (regresses slot-cap-yield-while-blocked-on-children).
func Run(ctx context.Context, p *Pipeline, sp *agent.Spawner, bb *agent.Blackboard) (Status, error) {
	byName := make(map[string]Step, len(p.Steps))
	for _, s := range p.Steps {
		byName[s.Name] = s
	}

	var (
		mu         sync.Mutex
		completed  = map[string]bool{} // step names that finished (done or skipped)
		started    = map[string]bool{}
		failed     bool
		failedStep string
	)

	depsSatisfied := func(s Step) bool {
		for _, d := range s.DependsOn {
			if !completed[d] {
				return false
			}
		}
		return true
	}

	var wg sync.WaitGroup

	// runStep executes one step (its instances) and, on completion, records its
	// completion and launches any newly-ready steps. Recursion happens on child
	// goroutines so ready siblings run concurrently.
	var launchReady func()
	runStep := func(s Step) {
		defer wg.Done()
		outputs, stepErr := executeStep(ctx, s, sp, bb)

		mu.Lock()
		defer mu.Unlock()
		if failed {
			return // pipeline already failing; do not fan out further
		}
		if stepErr != nil {
			if s.OnError.Kind == ErrorSkip {
				// Record completion, leave outputs absent, continue.
				completed[s.Name] = true
			} else {
				failed = true
				failedStep = s.Name
				return
			}
		} else {
			// Write outputs only on success.
			for k, v := range outputs {
				bb.Put(k, v, engineWriterID, engineWriterLabel)
			}
			completed[s.Name] = true
			// Evaluate routing for its side effects (documented behavior); the
			// readiness scheduler drives actual execution. route never errors.
			_, _ = route(s, bb)
		}
		launchReady()
	}

	launchReady = func() {
		// Caller holds mu.
		for _, s := range p.Steps {
			if started[s.Name] || completed[s.Name] {
				continue
			}
			if !depsSatisfied(s) {
				continue
			}
			started[s.Name] = true
			wg.Add(1)
			go runStep(s)
		}
	}

	mu.Lock()
	launchReady()
	mu.Unlock()

	wg.Wait()

	mu.Lock()
	st := Status{State: StateCompleted}
	if failed {
		st = Status{State: StateFailed, FailedStep: failedStep}
	}
	mu.Unlock()

	bb.Put(statusKey, st.State.String(), engineWriterID, engineWriterLabel)
	if st.State == StateFailed {
		return st, fmt.Errorf("pipeline %q failed at step %q", p.Name, st.FailedStep)
	}
	return st, nil
}

// executeStep runs one step's instance(s) and returns the output key->value map
// to write on success, or an error respecting the step's retry budget. It does
// NOT write the blackboard (the caller does, under the coordinator lock) so a
// failing step under fail/skip leaves no partial outputs.
func executeStep(ctx context.Context, s Step, sp *agent.Spawner, bb *agent.Blackboard) (map[string]any, error) {
	prompt := substitute(s.Prompt, s.Inputs, bb)

	fanout := s.Fanout
	if fanout < 1 {
		fanout = 1
	}

	// Spawn all instances, retrying each (independently) per the step's policy.
	results := make([]any, fanout)
	errs := make([]error, fanout)
	var wg sync.WaitGroup
	for i := 0; i < fanout; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			results[idx], errs[idx] = spawnWithRetry(ctx, s, prompt, idx, fanout, sp)
		}(i)
	}
	wg.Wait()

	for _, e := range errs {
		if e != nil {
			return nil, e
		}
	}

	out := map[string]any{}
	if fanout > 1 {
		for i := 0; i < fanout; i++ {
			for _, pat := range s.Outputs {
				out[fanoutKey(pat, i)] = results[i]
			}
		}
	} else {
		for _, k := range s.Outputs {
			out[k] = results[0]
		}
	}
	return out, nil
}

// spawnWithRetry performs a single instance's spawn, retrying per s.OnError. For
// retry(N) the spawn is attempted up to 1+N times. idx/total let a fanout
// instance disambiguate its label/prompt.
func spawnWithRetry(ctx context.Context, s Step, prompt string, idx, total int, sp *agent.Spawner) (any, error) {
	label := s.Name
	if total > 1 {
		label = s.Name + "-" + strconv.Itoa(idx)
	}

	maxTries := 1
	if s.OnError.Kind == ErrorRetry {
		maxTries = 1 + s.OnError.Retries
	}

	var lastErr error
	for try := 0; try < maxTries; try++ {
		v, err := spawnOnce(ctx, s, label, prompt, sp)
		if err == nil {
			return v, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return nil, lastErr
}

// spawnOnce spawns a single child and returns its structured value (when it
// matched the step's Expects schema) or the raw result text (on
// ErrNoStructuredResult — a mismatch or no-schema case degrades to text, never a
// step error).
func spawnOnce(ctx context.Context, s Step, label, prompt string, sp *agent.Spawner) (any, error) {
	opts := agent.SpawnOpts{
		Label:  label,
		Task:   prompt,
		Worker: s.Worker,
	}
	if len(s.Expects) > 0 {
		if schema := decodeSchema(s.Expects); schema != nil {
			opts.Expects = schema
		}
	}

	h, err := sp.Spawn(ctx, opts)
	if err != nil {
		return nil, err
	}
	structured, rerr := h.Result()
	if rerr == nil {
		return structured, nil
	}
	// A run error (not the no-structured sentinel) is a real failure.
	done := h.Wait()
	if done.Err != nil {
		return nil, done.Err
	}
	// No structured result: degrade to the raw text.
	return done.Result, nil
}

// substitute expands {{key}} placeholders in the prompt from the blackboard.
// Glob-input keys (containing '*') are expanded across every matching key and the
// values joined; a plain key substitutes its single value. Missing keys leave the
// placeholder blank. Rendering is total (never errors).
func substitute(prompt string, inputs []string, bb *agent.Blackboard) string {
	out := prompt
	for _, in := range inputs {
		placeholder := "{{" + in + "}}"
		out = strings.ReplaceAll(out, placeholder, resolveInput(in, bb))
	}
	return out
}

// resolveInput renders one input key to a string. A glob pattern joins the string
// forms of every matching key's value (in Keys' sorted order); a plain key
// renders its single value.
func resolveInput(key string, bb *agent.Blackboard) string {
	if strings.ContainsAny(key, "*?[") {
		matches := bb.Keys(key)
		parts := make([]string, 0, len(matches))
		for _, k := range matches {
			if e, ok := bb.Get(k); ok {
				parts = append(parts, valueString(e.Value))
			}
		}
		return strings.Join(parts, "\n")
	}
	if e, ok := bb.Get(key); ok {
		return valueString(e.Value)
	}
	return ""
}

// valueString renders a blackboard value for prompt interpolation. Strings pass
// through; other values are formatted with %v.
func valueString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

// decodeSchema decodes a step's raw Expects JSON into the map[string]any shape
// the spawner's validator consumes (agent.SpawnOpts.Expects). A malformed or
// non-object schema yields nil (no structured expectation), degrading the step to
// plain text rather than erroring.
func decodeSchema(raw []byte) map[string]any {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	return m
}

// fanoutKey maps an output pattern and an instance index to a concrete key. A
// pattern containing '*' has the LAST '*' replaced by the index (e.g. "hits/*",
// 2 => "hits/2"); a pattern with no glob gets the index appended under a '/'.
func fanoutKey(pattern string, idx int) string {
	i := strconv.Itoa(idx)
	if strings.Contains(pattern, "*") {
		last := strings.LastIndex(pattern, "*")
		return pattern[:last] + i + pattern[last+1:]
	}
	return pattern + "/" + i
}
