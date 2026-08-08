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
// glob-namespaced keys for a fanout).
//
// Conditional routing affects execution (change 0026, FIX 2). A step with any
// conditions or a non-empty default is a ROUTER; on success route() picks its
// chosen successor (first matching condition's goto, else default, else none).
// A step named as a router's goto/default target is BRANCH-GATED: it runs only
// when a router releases it (chose it), is SKIPPED (outputs absent) once every
// router targeting it has decided without choosing it, and — because a skipped
// step's outputs are absent — carries its depends_on downstream into the skip.
// Steps that are no router's target keep pure depends_on readiness, so a
// pipeline with no conditions/default behaves exactly as a plain DAG. Routing
// evaluation is total (route never errors: a type-mismatch or missing-key
// condition simply does not match and falls through to default/none).
//
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

	// runCtx is a cancellable child of ctx used for the step execution path. On the
	// first fail under on_error:fail we cancel it so in-flight sibling goroutines
	// observe cancellation and stop early rather than running to completion on the
	// shared parent ctx (wasted spawns/tokens). wg.Wait still awaits clean shutdown.
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()

	// Routing wiring (change 0026). A ROUTER is a step with any conditions or a
	// non-empty default; its ROUTING TARGETS are the union of every condition Goto
	// and its Default. A step named as a routing target of at least one router is
	// ROUTED (branch-gated): it does not launch on depends_on readiness alone —
	// it waits for its router(s) to decide, launching only when RELEASED by a
	// router that chose it. A routed step whose every router has decided WITHOUT
	// releasing it is SKIPPED. Non-routed steps keep pure readiness scheduling, so
	// a pipeline without any conditions/default behaves exactly as before.
	routers := map[string][]string{} // routed step -> router step names targeting it
	for _, s := range p.Steps {
		for _, tgt := range routingTargets(s) {
			routers[tgt] = append(routers[tgt], s.Name)
		}
	}
	isRouted := func(name string) bool { _, ok := routers[name]; return ok }

	var (
		mu         sync.Mutex
		done       = map[string]bool{} // steps that ran to completion (outputs written)
		skipped    = map[string]bool{} // steps settled without running (branch not taken)
		released   = map[string]bool{} // routed steps a router chose (eligible to run)
		routerDone = map[string]int{}  // routed step -> count of its routers that decided
		started    = map[string]bool{}
		failed     bool
		failedStep string
	)

	// settled reports whether a step has reached a terminal state (ran or skipped).
	settled := func(name string) bool { return done[name] || skipped[name] }

	// depsAllDone reports whether every depends_on ran to completion (a skipped
	// dependency is NOT "done" — its outputs are absent — so it does not satisfy a
	// dependent, which propagates the skip in launchReady).
	depsAllDone := func(s Step) bool {
		for _, d := range s.DependsOn {
			if !done[d] {
				return false
			}
		}
		return true
	}

	// depHasSkip reports whether any depends_on was skipped, which propagates the
	// skip to this step (a branch not taken carries its downstream with it).
	depHasSkip := func(s Step) bool {
		for _, d := range s.DependsOn {
			if skipped[d] {
				return true
			}
		}
		return false
	}

	// routersAllDecided reports whether every router targeting name has decided
	// (settled). Only then can a routed, unreleased step be conclusively skipped.
	routersAllDecided := func(name string) bool {
		return routerDone[name] >= len(routers[name])
	}

	var wg sync.WaitGroup
	var launchReady func()

	// markSkipped settles name as skipped (no outputs) and records the skip against
	// any routed target it routes to, so a router that never ran still counts as a
	// decision for its targets. Caller holds mu.
	var markSkipped func(name string)
	markSkipped = func(name string) {
		if settled(name) || started[name] {
			return
		}
		skipped[name] = true
		// A skipped router still "decides" its targets (by not releasing them).
		for _, tgt := range routingTargets(byName[name]) {
			routerDone[tgt]++
		}
	}

	// runStep executes one step (its instances) and, on completion, records its
	// completion, applies routing, and launches any newly-ready steps. Recursion
	// happens on child goroutines so ready siblings run concurrently.
	runStep := func(s Step) {
		defer wg.Done()
		outputs, stepErr := executeStep(runCtx, s, sp, bb)

		mu.Lock()
		defer mu.Unlock()
		if failed {
			return // pipeline already failing; do not fan out further
		}
		if stepErr != nil {
			if s.OnError.Kind == ErrorSkip {
				// Record completion, leave outputs absent, continue. A skip still
				// decides this step's routing targets (it took no branch).
				done[s.Name] = true
				for _, tgt := range routingTargets(s) {
					routerDone[tgt]++
				}
			} else {
				failed = true
				failedStep = s.Name
				// Cancel in-flight siblings so they stop early instead of running to
				// completion on the shared ctx (FIX 3): the failure is terminal, so
				// their work is wasted. wg.Wait below still awaits their shutdown.
				cancelRun()
				return
			}
		} else {
			// Write outputs only on success.
			for k, v := range outputs {
				bb.Put(k, v, engineWriterID, engineWriterLabel)
			}
			done[s.Name] = true
			// Routing: pick the chosen target (first matching condition, else
			// default, else none). Release the chosen target; every other routing
			// target of this router counts as decided-not-chosen. route never errors.
			chosen, ok := route(s, bb)
			for _, tgt := range routingTargets(s) {
				routerDone[tgt]++
			}
			if ok && chosen != "" {
				released[chosen] = true
			}
		}
		launchReady()
	}

	launchReady = func() {
		// Caller holds mu. A fixpoint pass: launching/skipping a step can settle
		// another, so iterate until a full sweep makes no change.
		for {
			changed := false
			for _, s := range p.Steps {
				if settled(s.Name) || started[s.Name] {
					continue
				}
				// A step downstream of a skipped (not-taken) branch is itself skipped.
				if depHasSkip(s) {
					markSkipped(s.Name)
					changed = true
					continue
				}
				if !depsAllDone(s) {
					continue
				}
				// A routed step is gated on its routers' decision: it runs only if a
				// router released it; if every router has decided without releasing it,
				// it is skipped; otherwise it keeps waiting.
				if isRouted(s.Name) && !released[s.Name] {
					if routersAllDecided(s.Name) {
						markSkipped(s.Name)
						changed = true
					}
					continue
				}
				started[s.Name] = true
				changed = true
				wg.Add(1)
				go runStep(s)
			}
			if !changed {
				return
			}
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
