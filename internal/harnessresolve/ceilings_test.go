package harnessresolve

import (
	"context"
	"errors"
	"math"
	"strconv"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/ethanhinson/fuse/harness"
	harnessv1 "github.com/ethanhinson/fuse/harness/v1"
	"github.com/ethanhinson/fuse/internal/capabilities"
	"github.com/ethanhinson/fuse/internal/config"
)

// descriptorTightenPaths independently walks HarnessSpec for tighten_only
// fields, as dotted templates with map and list values entered keyless (the
// walk of harness/options_test.go walkSpec, with a per-path recursion guard so
// a message type reused at two paths is visited at both).
func descriptorTightenPaths() []string {
	var out []string
	onPath := map[protoreflect.FullName]bool{}
	var walk func(prefix string, md protoreflect.MessageDescriptor)
	walk = func(prefix string, md protoreflect.MessageDescriptor) {
		if onPath[md.FullName()] {
			return
		}
		onPath[md.FullName()] = true
		defer delete(onPath, md.FullName())
		fields := md.Fields()
		for i := 0; i < fields.Len(); i++ {
			fd := fields.Get(i)
			path := prefix + string(fd.Name())
			if opts := fd.Options(); opts != nil && proto.HasExtension(opts, harnessv1.E_Ui) &&
				proto.GetExtension(opts, harnessv1.E_Ui).(*harnessv1.FieldUI).GetTightenOnly() {
				out = append(out, path)
			}
			switch {
			case fd.IsMap():
				if m := fd.MapValue().Message(); m != nil {
					walk(path+".", m)
				}
			case fd.Message() != nil:
				walk(path+".", fd.Message())
			}
		}
	}
	walk("", (&harnessv1.HarnessSpec{}).ProtoReflect().Descriptor())
	return out
}

// ceilingBase is an operator config with a mix of set and unset limits and one
// operator-defined workflow.
func ceilingBase() config.Config {
	c := config.Default()
	c.Agents.QueueBound = 2.5
	c.Agents.ToolTimeoutSeconds = 120
	c.Throughput.RequestsPerMinute = 6000
	c.Pipeline.Synthesis.MaxSteps = 20
	// Default() sets max_attempts; clear it so one pipeline cap is unset.
	c.Pipeline.Synthesis.MaxAttempts = 0
	c.Workflows = map[string]config.WorkflowConfig{"triage": {Skill: "notes", Pool: config.PoolConfig{Concurrent: 4, Tokens: 50_000}}}
	return c
}

// templateOf: workflows["triage"].pool.concurrent -> workflows.pool.concurrent.
func templateOf(p string) string {
	if i := strings.Index(p, "["); i >= 0 {
		return p[:i] + p[strings.Index(p, "]")+1:]
	}
	return p
}

// The spec's coverage test: no tighten-only field may lack a ceiling.
func TestEveryTightenOnlyFieldHasACeiling(t *testing.T) {
	have := map[string]bool{}
	for _, c := range (Resolver{Base: ceilingBase()}).Ceilings() {
		have[templateOf(c.FieldPath)] = true
		if c.Direction != capabilities.DirectionMax {
			t.Errorf("%s direction %v", c.FieldPath, c.Direction)
		}
	}
	paths := descriptorTightenPaths()
	if len(paths) == 0 {
		t.Fatal("no tighten_only fields found")
	}
	for _, p := range paths {
		if !have[p] {
			t.Errorf("tighten_only field %s has no ceiling", p)
		}
	}
	for p := range have {
		found := false
		for _, d := range paths {
			found = found || d == p
		}
		if !found {
			t.Errorf("ceiling %s is not a tighten_only field", p)
		}
	}
}

// Advertised path == refusal path: both read one accessor.
func TestCeilingPathsMatchRefusalPaths(t *testing.T) {
	r := Resolver{Base: ceilingBase()}
	for _, c := range r.Ceilings() {
		if path, _ := r.tightenField(configKeyFor(c.FieldPath)); path != c.FieldPath {
			t.Errorf("ceiling %s refuses at %s", c.FieldPath, path)
		}
	}
}

func TestCeilingsCarryTheOperatorValues(t *testing.T) {
	got := map[string]capabilities.Ceiling{}
	for _, c := range (Resolver{Base: ceilingBase()}).Ceilings() {
		if _, dup := got[c.FieldPath]; dup {
			t.Errorf("ceiling %s listed twice", c.FieldPath)
		}
		got[c.FieldPath] = c
	}
	set := map[string]float64{
		"agents.queue_bound":                  2.5,
		"agents.tool_timeout_seconds":         120,
		"throughput.requests_per_minute":      6000,
		"pipeline.synthesis.max_steps":        20,
		`workflows["triage"].pool.concurrent`: 4,
		`workflows["triage"].pool.tokens`:     50000,
	}
	for path, want := range set {
		c, ok := got[path]
		if !ok {
			t.Errorf("no ceiling for %s", path)
			continue
		}
		if !c.OperatorSet || c.Value != want {
			t.Errorf("%s = set %v value %v, want set true value %v", path, c.OperatorSet, c.Value, want)
		}
	}
	for _, path := range []string{"throughput.session_tokens", "pipeline.synthesis.max_attempts", `workflows["triage"].pool.total`} {
		c, ok := got[path]
		if !ok {
			t.Errorf("no ceiling for %s", path)
			continue
		}
		if c.OperatorSet || c.Value != 0 {
			t.Errorf("%s = set %v value %v, want set false value 0", path, c.OperatorSet, c.Value)
		}
	}
}

func TestCeilingsWithNoWorkflowsListNoWorkflowEntries(t *testing.T) {
	base := config.Default()
	// Default() ships a built-in research workflow; an operator with none
	// defined must advertise no workflow ceilings.
	base.Workflows = nil
	cs := (Resolver{Base: base}).Ceilings()
	if len(cs) == 0 {
		t.Fatal("no ceilings")
	}
	for _, c := range cs {
		if strings.HasPrefix(c.FieldPath, "workflows") {
			t.Errorf("unexpected workflow ceiling %s", c.FieldPath)
		}
	}
}

// Every operator-defined workflow has each pool number advertised, in sorted
// workflow order, so the listing is stable across calls.
func TestCeilingsListEveryOperatorWorkflowSorted(t *testing.T) {
	base := ceilingBase()
	base.Workflows["alpha"] = config.WorkflowConfig{Pool: config.PoolConfig{Total: 8}}
	byField := map[string][]string{}
	for _, c := range (Resolver{Base: base}).Ceilings() {
		if rest, ok := strings.CutPrefix(c.FieldPath, "workflows["); ok {
			end := strings.Index(rest, "]")
			name, _ := strconv.Unquote(rest[:end])
			field := rest[end+1:]
			byField[field] = append(byField[field], name)
		}
	}
	if len(byField) != 4 {
		t.Errorf("workflow pool fields = %v, want 4", byField)
	}
	for field, names := range byField {
		if strings.Join(names, ",") != "alpha,triage" {
			t.Errorf("workflows %s listed as %v, want [alpha triage]", field, names)
		}
	}
}

// setSpecField sets the numeric spec field at a ceiling's FieldPath.
func setSpecField(t *testing.T, spec *harnessv1.HarnessSpec, path string, v float64) {
	t.Helper()
	m := spec.ProtoReflect()
	parts := strings.Split(path, ".")
	for i := 0; i < len(parts); i++ {
		part := parts[i]
		if open := strings.Index(part, "["); open >= 0 {
			// workflows["x"] - a quoted key may not contain a dot (spec key pattern).
			key, err := strconv.Unquote(part[open+1 : len(part)-1])
			if err != nil {
				t.Fatalf("%s: %v", path, err)
			}
			fd := m.Descriptor().Fields().ByName(protoreflect.Name(part[:open]))
			m = m.Mutable(fd).Map().Mutable(protoreflect.ValueOfString(key).MapKey()).Message()
			continue
		}
		fd := m.Descriptor().Fields().ByName(protoreflect.Name(part))
		if fd == nil {
			t.Fatalf("%s: no field %s", path, part)
		}
		if i < len(parts)-1 {
			m = m.Mutable(fd).Message()
			continue
		}
		switch fd.Kind() {
		case protoreflect.DoubleKind:
			m.Set(fd, protoreflect.ValueOfFloat64(v))
		case protoreflect.Uint32Kind:
			m.Set(fd, protoreflect.ValueOfUint32(uint32(v)))
		default:
			t.Fatalf("%s: unexpected kind %v", path, fd.Kind())
		}
	}
}

// tightenViolations resolves spec over r and returns its tighten_only
// violations.
func tightenViolations(t *testing.T, r Resolver, spec *harnessv1.HarnessSpec) []harness.Violation {
	t.Helper()
	_, err := r.Resolve(context.Background(), "acme", spec)
	var ve *harness.ValidationError
	if err != nil && !errors.As(err, &ve) {
		t.Fatalf("resolve: %v", err)
	}
	var out []harness.Violation
	if ve != nil {
		for _, v := range ve.Violations {
			if v.RuleID == harness.RuleTightenOnly {
				out = append(out, v)
			}
		}
	}
	return out
}

// Advertised ceiling == enforced ceiling. For every advertised ceiling, a spec
// that loosens it is refused at exactly that path, naming the advertised value;
// a spec that tightens a set ceiling is accepted.
func TestCeilingsMatchStartLoopEnforcement(t *testing.T) {
	r := Resolver{Base: ceilingBase()}
	for _, c := range r.Ceilings() {
		loosen := 1.0
		if c.OperatorSet {
			loosen = c.Value + 1
		}
		spec := &harnessv1.HarnessSpec{}
		setSpecField(t, spec, c.FieldPath, loosen)
		vs := tightenViolations(t, r, spec)
		if len(vs) != 1 || vs[0].FieldPath != c.FieldPath {
			t.Errorf("loosening %s to %v: tighten violations %+v", c.FieldPath, loosen, vs)
			continue
		}
		want := "may only tighten a limit this server sets, and it sets none here"
		if c.OperatorSet {
			want = "may only tighten this server's limit of " + strconv.FormatFloat(c.Value, 'f', -1, 64)
		}
		if vs[0].Message != want {
			t.Errorf("%s message %q, want %q", c.FieldPath, vs[0].Message, want)
		}
		if !c.OperatorSet {
			continue
		}
		tighten := math.Floor(c.Value / 2)
		if tighten <= 0 {
			continue
		}
		spec = &harnessv1.HarnessSpec{}
		setSpecField(t, spec, c.FieldPath, tighten)
		if vs := tightenViolations(t, r, spec); len(vs) != 0 {
			t.Errorf("tightening %s to %v refused: %+v", c.FieldPath, tighten, vs)
		}
	}
}

// A large operator limit renders in plain digits, never exponent form.
func TestTightenMessageRendersLargeLimitsPlainly(t *testing.T) {
	base := ceilingBase()
	base.Throughput.SessionTokens = 2_000_000
	_, limit := (Resolver{Base: base}).tightenField("throughput.session_tokens")
	if limit != "2000000" {
		t.Errorf("limit = %q, want 2000000", limit)
	}
}
