package pipeline

import (
	"reflect"
	"regexp"
	"strings"

	"github.com/ethanhinson/fuse/internal/agent"
)

// evalCondition reports whether condition c holds against the blackboard. It is
// total: any type mismatch, missing key, invalid regexp, or unknown operator
// yields false. It never errors or panics — routing must be deterministic for any
// input the runtime can produce.
func evalCondition(c Condition, bb *agent.Blackboard) bool {
	entry, ok := bb.Get(c.Key)

	switch c.Op {
	case "exists":
		return ok
	case "eq":
		return ok && jsonEqual(entry.Value, c.Value)
	case "ne":
		return ok && !jsonEqual(entry.Value, c.Value)
	case "gt", "lt":
		if !ok {
			return false
		}
		lhs, lok := toNumber(entry.Value)
		rhs, rok := toNumber(c.Value)
		if !lok || !rok {
			return false
		}
		if c.Op == "gt" {
			return lhs > rhs
		}
		return lhs < rhs
	case "contains":
		return ok && contains(entry.Value, c.Value)
	case "matches":
		if !ok {
			return false
		}
		pat, pok := c.Value.(string)
		if !pok {
			return false
		}
		re, err := regexp.Compile(pat)
		if err != nil {
			return false
		}
		return re.MatchString(toStringLoose(entry.Value))
	default:
		return false
	}
}

// routingTargets returns the set of steps this step can route to: every
// condition Goto (non-empty) plus Default (when set), de-duplicated in first-seen
// order. It is the static edge set the engine uses to identify branch-gated
// (routed) steps; empty for a step with no conditions and no default. See route
// for the runtime decision among these targets.
func routingTargets(s Step) []string {
	var out []string
	seen := map[string]bool{}
	add := func(name string) {
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		out = append(out, name)
	}
	for _, c := range s.Conditions {
		add(c.Goto)
	}
	add(s.Default)
	return out
}

// route returns the next step to run after this one completes. The first
// matching condition's Goto wins; otherwise Default; otherwise ("", false)
// meaning "no explicit route, rely on readiness scheduling".
func route(s Step, bb *agent.Blackboard) (string, bool) {
	for _, c := range s.Conditions {
		if evalCondition(c, bb) {
			return c.Goto, true
		}
	}
	if s.Default != "" {
		return s.Default, true
	}
	return "", false
}

// jsonEqual compares two decoded-JSON values for equality, coercing across
// numeric kinds so an authored int and a float64-decoded number compare equal.
func jsonEqual(a, b any) bool {
	an, aok := toNumber(a)
	bn, bok := toNumber(b)
	if aok && bok {
		return an == bn
	}
	if aok != bok {
		return false // one numeric, one not
	}
	return reflect.DeepEqual(a, b)
}

// toNumber coerces a value to float64 if it is any numeric kind. Bools and
// numeric-looking strings are NOT coerced (a type mismatch is false, per spec).
func toNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int8:
		return float64(n), true
	case int16:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint:
		return float64(n), true
	case uint8:
		return float64(n), true
	case uint16:
		return float64(n), true
	case uint32:
		return float64(n), true
	case uint64:
		return float64(n), true
	default:
		return 0, false
	}
}

// contains implements the `contains` operator: substring for a string value, or
// membership for an array value (with numeric coercion on element compare).
func contains(value, want any) bool {
	switch v := value.(type) {
	case string:
		w, ok := want.(string)
		return ok && strings.Contains(v, w)
	case []any:
		for _, el := range v {
			if jsonEqual(el, want) {
				return true
			}
		}
		return false
	default:
		return false
	}
}

// toStringLoose renders a value for regexp matching. Only string values match
// meaningfully; anything else yields "" (which a pattern anchored on content
// won't match), keeping matches total.
func toStringLoose(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
