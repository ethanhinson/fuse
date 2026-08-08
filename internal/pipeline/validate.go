package pipeline

import (
	"fmt"
	"sort"
	"strings"
)

// Caps bounds a Pipeline's shape. Each field is opt-in: a zero value skips that
// check (matching the config layer's zero-value-is-unset idiom).
type Caps struct {
	// MaxSteps caps the total number of steps.
	MaxSteps int
	// MaxFanout caps any single step's fanout.
	MaxFanout int
	// MaxDepth caps the DAG's longest dependency chain. Interpretation: the length
	// of the longest depends_on path measured in STEPS (a chain a->b->c->d, where
	// each depends on the prior, has depth 4). Goto edges are NOT counted toward
	// depth — depth models the static data-dependency spine, while goto is dynamic
	// runtime routing.
	MaxDepth int
	// RequirePool requires Pipeline.Workflow to be non-empty.
	RequirePool bool
	// MaxAttempts bounds Synthesize's re-ask loop (see synthesize.go). It is unused
	// by Validate; it lives here so a single Caps value carries every synthesis
	// bound. Zero => Synthesize applies its documented default.
	MaxAttempts int
}

// validOps is the set of condition operators the engine can evaluate.
var validOps = map[string]bool{
	"exists": true, "eq": true, "ne": true,
	"gt": true, "lt": true, "contains": true, "matches": true,
}

// Validate checks a Pipeline for structural soundness and, where set, Caps
// bounds. Checks: unique step names; every depends_on/goto/default resolves to a
// real step; only known condition operators; no cycles over the combined
// depends_on + goto edge set (Tarjan SCC); and each set Caps bound. Errors name
// the offending step(s).
func Validate(p *Pipeline, caps Caps) error {
	if p == nil {
		return fmt.Errorf("pipeline: nil pipeline")
	}

	// Unique names + name set.
	names := make(map[string]bool, len(p.Steps))
	for _, s := range p.Steps {
		if s.Name == "" {
			return fmt.Errorf("pipeline: a step has an empty name")
		}
		if names[s.Name] {
			return fmt.Errorf("pipeline: duplicate step name %q", s.Name)
		}
		names[s.Name] = true
	}

	// Reference resolution + operator validity.
	for _, s := range p.Steps {
		for _, d := range s.DependsOn {
			if !names[d] {
				return fmt.Errorf("pipeline: step %q depends_on unknown step %q", s.Name, d)
			}
		}
		for _, c := range s.Conditions {
			if !validOps[c.Op] {
				return fmt.Errorf("pipeline: step %q condition uses unknown op %q", s.Name, c.Op)
			}
			if c.Goto != "" && !names[c.Goto] {
				return fmt.Errorf("pipeline: step %q condition goto unknown step %q", s.Name, c.Goto)
			}
		}
		if s.Default != "" && !names[s.Default] {
			return fmt.Errorf("pipeline: step %q default routes to unknown step %q", s.Name, s.Default)
		}
	}

	// Cycle detection over combined depends_on + goto/default edges.
	if cyc := findCycle(p); len(cyc) > 0 {
		return fmt.Errorf("pipeline: routing cycle detected among steps: %s", strings.Join(cyc, " -> "))
	}

	// Caps.
	if caps.MaxSteps > 0 && len(p.Steps) > caps.MaxSteps {
		return fmt.Errorf("pipeline: %d steps exceeds MaxSteps %d", len(p.Steps), caps.MaxSteps)
	}
	if caps.MaxFanout > 0 {
		for _, s := range p.Steps {
			if s.Fanout > caps.MaxFanout {
				return fmt.Errorf("pipeline: step %q fanout %d exceeds MaxFanout %d", s.Name, s.Fanout, caps.MaxFanout)
			}
		}
	}
	if caps.MaxDepth > 0 {
		if d := longestDependsChain(p); d > caps.MaxDepth {
			return fmt.Errorf("pipeline: dependency chain length %d exceeds MaxDepth %d", d, caps.MaxDepth)
		}
	}
	if caps.RequirePool && p.Workflow == "" {
		return fmt.Errorf("pipeline: workflow pool required but none set")
	}
	return nil
}

// edges builds the combined adjacency (depends_on drawn as dep->step so it points
// in execution order, plus goto/default step->target) used for cycle detection.
func edges(p *Pipeline) map[string][]string {
	adj := map[string][]string{}
	for _, s := range p.Steps {
		if _, ok := adj[s.Name]; !ok {
			adj[s.Name] = nil
		}
		for _, d := range s.DependsOn {
			adj[d] = append(adj[d], s.Name)
		}
		for _, c := range s.Conditions {
			if c.Goto != "" {
				adj[s.Name] = append(adj[s.Name], c.Goto)
			}
		}
		if s.Default != "" {
			adj[s.Name] = append(adj[s.Name], s.Default)
		}
	}
	return adj
}

// findCycle runs Tarjan's SCC algorithm over the combined edge set and returns
// the members of the first strongly-connected component of size > 1, or a
// self-looping node, as the offending cycle. Empty slice means acyclic.
func findCycle(p *Pipeline) []string {
	adj := edges(p)

	// Stable node ordering for deterministic output.
	nodes := make([]string, 0, len(adj))
	for n := range adj {
		nodes = append(nodes, n)
	}
	sort.Strings(nodes)

	index := map[string]int{}
	low := map[string]int{}
	onStack := map[string]bool{}
	var stack []string
	next := 0
	var result []string

	var strongconnect func(v string)
	strongconnect = func(v string) {
		index[v] = next
		low[v] = next
		next++
		stack = append(stack, v)
		onStack[v] = true

		for _, w := range adj[v] {
			if _, seen := index[w]; !seen {
				strongconnect(w)
				if low[w] < low[v] {
					low[v] = low[w]
				}
			} else if onStack[w] {
				if index[w] < low[v] {
					low[v] = index[w]
				}
			}
		}

		if low[v] == index[v] {
			var comp []string
			for {
				w := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				onStack[w] = false
				comp = append(comp, w)
				if w == v {
					break
				}
			}
			// An SCC of size > 1 is a cycle. A singleton is a cycle only if it has a
			// self-edge.
			if len(comp) > 1 {
				if result == nil {
					sort.Strings(comp)
					result = comp
				}
			} else if hasSelfEdge(adj, comp[0]) {
				if result == nil {
					result = comp
				}
			}
		}
	}

	for _, n := range nodes {
		if _, seen := index[n]; !seen {
			strongconnect(n)
		}
	}
	return result
}

func hasSelfEdge(adj map[string][]string, v string) bool {
	for _, w := range adj[v] {
		if w == v {
			return true
		}
	}
	return false
}

// longestDependsChain returns the length (in steps) of the longest depends_on
// path. Only depends_on edges are considered (goto is dynamic routing, not a
// static depth). The graph is a DAG here (cycle check ran first), so a simple
// memoized DFS suffices.
func longestDependsChain(p *Pipeline) int {
	deps := map[string][]string{}
	for _, s := range p.Steps {
		deps[s.Name] = s.DependsOn
	}
	memo := map[string]int{}
	var depth func(name string) int
	depth = func(name string) int {
		if v, ok := memo[name]; ok {
			return v
		}
		best := 0
		for _, d := range deps[name] {
			if c := depth(d); c > best {
				best = c
			}
		}
		memo[name] = best + 1
		return memo[name]
	}
	max := 0
	for _, s := range p.Steps {
		if d := depth(s.Name); d > max {
			max = d
		}
	}
	return max
}
