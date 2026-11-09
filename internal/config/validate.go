package config

import (
	"fmt"
	"net"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ethanhinson/fuse/internal/model"
)

// validModes is the set of permission modes ParseMode understands. It is checked
// explicitly because ParseMode silently maps any unknown token to "smart", which
// turns a typo (mode: smrat) into a silent, wrong posture instead of an error.
var validModes = map[string]bool{
	"off":        true,
	"prompt-all": true,
	"smart":      true,
	"auto":       true,
}

// Problem is one structural validation finding against a resolved Config
// (change 0084). Path is the dotted YAML key path of the offending key (e.g.
// "context.summarization.threshold") so a caller that renders config as a form
// can place the error next to the field; Message describes the problem relative
// to that key. Err, when set, is the complete error the CLI has always printed
// for this rule (kept byte-identical, wrapping preserved); when nil the full
// text is "config: <Path> <Message>".
type Problem struct {
	Path    string
	Message string
	Err     error
}

// Error renders the problem as the CLI's startup error text.
func (p Problem) Error() string {
	if p.Err != nil {
		return p.Err.Error()
	}
	return "config: " + p.Path + " " + p.Message
}

// Unwrap exposes the wrapped legacy error, if any.
func (p Problem) Unwrap() error { return p.Err }

// Validate performs structural validation of a resolved Config and returns the
// first problem found, or nil. It deliberately does NOT resolve model aliases —
// that requires the model.Registry, which lives above this leaf package, so
// model-reference checks are done by the caller (see internal/configcheck) where
// both the config and the registry are in scope. Validate covers the checks that
// need only the config itself: enum membership and numeric sanity.
//
// The loader already clamps some numerics (e.g. negative MaxSpawns → 16); this
// method catches the values the loader passes through untouched and the enums it
// silently defaults, so a malformed config fails loudly at startup rather than
// behaving surprisingly at runtime.
//
// The rules themselves live in Problems (change 0084), which reports every
// failing rule with its key path; Validate keeps the startup contract of
// returning the first one, with the exact text it has always had.
func (c Config) Validate() error {
	ps := c.Problems()
	if len(ps) == 0 {
		return nil
	}
	if ps[0].Err != nil {
		return ps[0].Err
	}
	return ps[0]
}

// Problems runs every structural validation rule and returns all failures, in
// rule order (so Problems()[0] is what Validate reports). It is the single home
// of these rules; the CLI startup gate (Validate) reads it.
func (c Config) Problems() []Problem {
	var ps []Problem
	add := func(path, format string, args ...any) {
		ps = append(ps, Problem{Path: path, Message: fmt.Sprintf(format, args...)})
	}

	if m := c.Permissions.Mode; m != "" && !validModes[m] {
		add("permissions.mode", "%q is invalid (want off, prompt-all, smart, or auto)", m)
	}

	// Note: agents.tool_timeout_seconds is intentionally NOT checked here. The
	// loader's tighten-only merge (ADR-0006) only applies a value when it is > 0
	// and silently drops a negative, so a negative can never reach the resolved
	// Config - a check for it would be dead code implying a protection that does
	// not exist. MaxTurns/MaxTokens ARE passed through unclamped, so they are
	// checked below.
	if c.MaxTokens < 0 {
		add("max_tokens", "must be >= 0, got %d", c.MaxTokens)
	}
	if n := utf8.RuneCountInString(c.Instructions); n > MaxInstructionsChars {
		add("instructions", "must be at most %d characters, got %d", MaxInstructionsChars, n)
	}
	if c.NoProgressCalls != nil && *c.NoProgressCalls < 0 {
		add("no_progress_calls", "must be >= 0 (0 = off), got %d", *c.NoProgressCalls)
	}
	if c.MaxRunTokens < 0 {
		add("max_run_tokens", "must be >= 0 (0 = no cap), got %d", c.MaxRunTokens)
	}
	for alias, m := range c.Models.Entries {
		switch m.ReasoningEffort {
		case "", "none", "minimal", "low", "medium", "high", "xhigh":
		default:
			add("models."+alias+".reasoning_effort", "must be none, minimal, low, medium, high or xhigh, got %q", m.ReasoningEffort)
		}
		if m.Dialect != "" && !model.IsDialect(m.Dialect) {
			add("models."+alias+".dialect", "must be one of %s (or empty to detect it from the id), got %q", strings.Join(model.DialectNames(), ", "), m.Dialect)
		}
		for _, k := range m.DialectOptions.unknown {
			add("models."+alias+".dialect_options."+k, "is not a dialect option (want %s)", wantList(dialectOptionKeys))
		}
		if f := m.DialectOptions.ReasoningField; f != "" && !slices.Contains(model.ReasoningFields(), f) {
			add("models."+alias+".dialect_options.reasoning_field", "must be %s (or empty to replay reasoning in the field it arrived in), got %q", wantList(model.ReasoningFields()), f)
		}
	}
	if c.MaxTurns != nil && *c.MaxTurns < 0 {
		add("max_turns", "must be >= 0 (0 = unlimited), got %d", *c.MaxTurns)
	}

	// Summarization threshold is a context-window fraction; only 0 (unset) or a
	// value in (0,1] is meaningful. A value >1 or <0 would never (or always)
	// trigger, silently defeating Tier-2 summarization.
	if t := c.Context.Summarization.Threshold; t < 0 || t > 1 {
		add("context.summarization.threshold", "must be in [0,1], got %g", t)
	}
	// Relevance borderline band must be ordered and within [0,1] when set. The
	// problem is attributed to the bound(s) at fault: the one out of range, or
	// BOTH when the band is merely out of order (either may be the one the
	// author set; the other may be a default). Validate reports the first, whose
	// text is the same either way.
	rel := c.Context.Relevance
	if rel.BorderlineLo < 0 || rel.BorderlineHi > 1 || (rel.BorderlineHi > 0 && rel.BorderlineLo > rel.BorderlineHi) {
		var paths []string
		switch {
		case rel.BorderlineLo < 0 && rel.BorderlineHi > 1:
			paths = []string{"context.relevance.borderline_lo", "context.relevance.borderline_hi"}
		case rel.BorderlineLo < 0:
			paths = []string{"context.relevance.borderline_lo"}
		case rel.BorderlineHi > 1:
			paths = []string{"context.relevance.borderline_hi"}
		default:
			paths = []string{"context.relevance.borderline_lo", "context.relevance.borderline_hi"}
		}
		msg := fmt.Sprintf("borderline band [%g,%g] is invalid (want 0<=lo<=hi<=1)", rel.BorderlineLo, rel.BorderlineHi)
		for _, path := range paths {
			ps = append(ps, Problem{Path: path, Message: msg, Err: fmt.Errorf("config: context.relevance %s", msg)})
		}
	}
	if rel.RecencyFloorPct < 0 || rel.RecencyFloorPct > 100 {
		add("context.relevance.recency_floor_pct", "must be in [0,100], got %d", rel.RecencyFloorPct)
	}

	// The observability check stops at its first problem.
	if err := validateObservability(c.Observability); err != nil {
		ps = append(ps, Problem{Path: "observability", Message: strings.TrimPrefix(err.Error(), "config: "), Err: err})
	}

	// Roles: role tool names and model aliases are
	// not resolved here (tool sets are dynamic).
	if n := len(c.Roles); n > MaxRoles {
		add("roles", "must define at most %d roles, got %d", MaxRoles, n)
	}
	for _, name := range sortedKeys(c.Roles) {
		r := c.Roles[name]
		if !roleNameRE.MatchString(name) || len(name) > MaxRoleNameChars {
			// A name that is empty or holds a dot cannot be addressed as one
			// path part, so it is reported on the roles field itself. The
			// role's other fields are not checked until its name is valid.
			path := "roles." + name
			if name == "" || strings.Contains(name, ".") {
				path = "roles"
			}
			add(path, "role name %q must match %s and be at most %d characters", name, RoleNamePattern, MaxRoleNameChars)
			continue
		}
		for i, tool := range r.Tools {
			if tool == "" {
				add(fmt.Sprintf("roles.%s.tools[%d]", name, i), "must not be empty: name a tool, or omit tools to inherit the main agent's")
			}
		}
		if r.NoTools && len(r.Tools) > 0 {
			add("roles."+name+".no_tools", "names tools and no_tools; choose one")
		}
		if strings.TrimSpace(r.Description) == "" {
			add("roles."+name+".description", "is required: what the main agent reads when deciding whom to hand work to")
		} else if n := utf8.RuneCountInString(r.Description); n > MaxRoleDescriptionChars {
			add("roles."+name+".description", "must be at most %d characters, got %d", MaxRoleDescriptionChars, n)
		}
		if n := utf8.RuneCountInString(r.Instructions); n > MaxInstructionsChars {
			add("roles."+name+".instructions", "must be at most %d characters, got %d", MaxInstructionsChars, n)
		}
	}
	return ps
}

// roleNameRE is RoleNamePattern compiled.
var roleNameRE = regexp.MustCompile(RoleNamePattern)

// sortedKeys returns m's keys in sorted order.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func validateObservability(o ObservabilityConfig) error {
	if o.Traces.Enabled {
		if strings.TrimSpace(o.Traces.Endpoint) == "" {
			return fmt.Errorf("config: enabled observability.traces requires endpoint")
		}
		if _, _, err := net.SplitHostPort(o.Traces.Endpoint); err != nil {
			return fmt.Errorf("config: observability.traces.endpoint must be host:port: %w", err)
		}
		if o.Traces.Protocol != "grpc" && o.Traces.Protocol != "http/protobuf" {
			return fmt.Errorf("config: observability.traces.protocol must be grpc or http/protobuf")
		}
		if o.Traces.QueueSize < 0 || o.Traces.BatchSize < 0 || (o.Traces.QueueSize > 0 && o.Traces.BatchSize > o.Traces.QueueSize) {
			return fmt.Errorf("config: observability.traces batching and queue sizes are invalid")
		}
		for name, raw := range map[string]string{"export_timeout": o.Traces.ExportTimeout, "batch_timeout": o.Traces.BatchTimeout} {
			if raw != "" {
				d, err := time.ParseDuration(raw)
				if err != nil || d <= 0 {
					return fmt.Errorf("config: observability.traces.%s must be a positive duration", name)
				}
			}
		}
		if o.Traces.SampleRatio < 0 || o.Traces.SampleRatio > 1 {
			return fmt.Errorf("config: observability.traces.sample_ratio must be in [0,1]")
		}
	}
	return nil
}

// wantList joins names as "a, b or c".
func wantList(names []string) string {
	if len(names) < 2 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + " or " + names[len(names)-1]
}
