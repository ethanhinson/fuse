package harnessresolve

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	harnessv1 "github.com/ethanhinson/fuse/harness/v1"
	"github.com/ethanhinson/fuse/internal/capabilities"
)

// tightenTemplate is one tighten_only HarnessSpec field. mapField is the map
// it sits under ("" for none); suffix is the dotted path below the map value
// (or the whole path).
type tightenTemplate struct{ mapField, suffix string }

// tightenTemplates reads every tighten_only field from the
// (fuse.harness.v1.ui) descriptor options, never a hand list. A tighten_only
// field under one map level is supported (all the schema has); one under a
// nested map panics so the coverage test fails loudly instead of advertising
// nothing. A message type is entered once per path, not once overall, so a
// type reused at two paths is listed at both, and recursion stops at a type
// already on the path.
func tightenTemplates() []tightenTemplate {
	var out []tightenTemplate
	onPath := map[protoreflect.FullName]bool{}
	// nested names the outer map when md sits under a map inside a map.
	var walk func(mapField, nested, prefix string, md protoreflect.MessageDescriptor)
	walk = func(mapField, nested, prefix string, md protoreflect.MessageDescriptor) {
		if onPath[md.FullName()] {
			return
		}
		onPath[md.FullName()] = true
		defer delete(onPath, md.FullName())
		for i := 0; i < md.Fields().Len(); i++ {
			fd := md.Fields().Get(i)
			path := prefix + string(fd.Name())
			if o := fd.Options(); o != nil && proto.HasExtension(o, harnessv1.E_Ui) &&
				proto.GetExtension(o, harnessv1.E_Ui).(*harnessv1.FieldUI).GetTightenOnly() {
				if nested != "" {
					panic("harnessresolve: tighten_only field " + path + " under nested map " + nested)
				}
				out = append(out, tightenTemplate{mapField, path})
			}
			switch {
			case fd.IsMap() && fd.MapValue().Message() != nil:
				if mapField != "" {
					walk(mapField, mapField, path+".", fd.MapValue().Message())
				} else {
					walk(path, "", "", fd.MapValue().Message())
				}
			case !fd.IsMap() && fd.Message() != nil:
				walk(mapField, nested, path+".", fd.Message())
			}
		}
	}
	walk("", "", "", (&harnessv1.HarnessSpec{}).ProtoReflect().Descriptor())
	return out
}

// Ceilings is the tighten-only floor a spec resolved over r.Base is held to:
// one entry per tighten_only field, and per operator-defined key for a field
// under a map (a key the operator does not define is the client's, so it has
// no ceiling). Values come from operatorLimit, which tightenField words its
// refusal with. A spec may set such a field only to a positive value strictly
// below a set Value; when the operator sets none, the spec may not set it.
func (r Resolver) Ceilings() []capabilities.Ceiling {
	var out []capabilities.Ceiling
	add := func(key, path string) {
		v, set, known := r.operatorLimit(key)
		if !known {
			panic("harnessresolve: no operator value for tighten_only key " + key)
		}
		c := capabilities.Ceiling{FieldPath: path, OperatorSet: set, Direction: capabilities.DirectionMax}
		if set {
			c.Value = v
		}
		out = append(out, c)
	}
	for _, t := range tightenTemplates() {
		if t.mapField == "" {
			add(t.suffix, t.suffix)
			continue
		}
		for _, k := range r.operatorMapKeys(t.mapField) {
			add(t.mapField+"."+k+"."+t.suffix, fmt.Sprintf("%s[%s].%s", t.mapField, strconv.Quote(k), t.suffix))
		}
	}
	return out
}

// operatorMapKeys lists, sorted, the keys the operator defines in a spec map
// that holds tighten_only fields. Only workflows does today; any other map
// panics so a new one cannot be advertised as having no keys.
func (r Resolver) operatorMapKeys(mapField string) []string {
	if mapField != "workflows" {
		panic("harnessresolve: no operator keys for map " + mapField)
	}
	keys := make([]string, 0, len(r.Base.Workflows))
	for k := range r.Base.Workflows {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// operatorLimit reads the operator's value for a tighten-only config key
// (config.OverlaySpec's refusal form). set reports a positive value - 0 is
// unset, and an unset limit is one a spec may not introduce. known is false
// for a key that is not a tighten-only axis. Ceilings and tightenField both
// read through here, so the advertised value is the one a refusal names.
func (r Resolver) operatorLimit(key string) (v float64, set, known bool) {
	num := func(n int) (float64, bool, bool) { return float64(max(n, 0)), n > 0, true }
	b := r.Base
	if rest, ok := strings.CutPrefix(key, "workflows."); ok {
		// workflows.<name>.pool.<field>; a workflow name cannot contain a dot
		// (the spec's key pattern), so the last two segments are pool.<field>.
		i := strings.LastIndex(rest, ".pool.")
		if i < 0 {
			return 0, false, false
		}
		p := b.Workflows[rest[:i]].Pool
		switch rest[i+len(".pool."):] {
		case "concurrent":
			return num(p.Concurrent)
		case "total":
			return num(p.Total)
		case "max_depth":
			return num(p.MaxDepth)
		case "tokens":
			return num(p.Tokens)
		}
		return 0, false, false
	}
	switch key {
	case "agents.queue_bound":
		if b.Agents.QueueBound > 0 {
			return b.Agents.QueueBound, true, true
		}
		return 0, false, true
	case "agents.tool_timeout_seconds":
		return num(b.Agents.ToolTimeoutSeconds)
	case "throughput.requests_per_minute":
		return num(b.Throughput.RequestsPerMinute)
	case "throughput.tokens_per_minute":
		return num(b.Throughput.TokensPerMinute)
	case "throughput.session_tokens":
		return num(b.Throughput.SessionTokens)
	case "pipeline.synthesis.max_steps":
		return num(b.Pipeline.Synthesis.MaxSteps)
	case "pipeline.synthesis.max_fanout":
		return num(b.Pipeline.Synthesis.MaxFanout)
	case "pipeline.synthesis.max_depth":
		return num(b.Pipeline.Synthesis.MaxDepth)
	case "pipeline.synthesis.max_attempts":
		return num(b.Pipeline.Synthesis.MaxAttempts)
	}
	return 0, false, false
}
