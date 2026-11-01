package config

import (
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/ethanhinson/fuse/internal/loopauth"
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
// The loader already clamps some numerics (e.g. negative MaxSpawns → 64); this
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
// of these rules: the CLI startup gate (Validate) and the harness-spec validator
// (github.com/ethanhinson/fuse/harness, change 0084) both read it, so the two
// can never disagree about what a valid config is.
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

	// loop_server.lease_ttl (change 0049) is a Go duration string when set; a
	// typo ("30" without a unit, "sec") must fail loudly at startup rather than
	// silently falling back to the runtime default and giving a lease TTL nobody
	// asked for. Empty = unset (the runtime's built-in default) and is fine.
	if ttl := c.LoopServer.LeaseTTL; ttl != "" {
		d, err := time.ParseDuration(ttl)
		if err != nil {
			ps = append(ps, Problem{
				Path:    "loop_server.lease_ttl",
				Message: fmt.Sprintf("%q is not a valid duration (e.g. \"30s\", \"2m\")", ttl),
				Err:     fmt.Errorf("config: loop_server.lease_ttl %q is not a valid duration (e.g. \"30s\", \"2m\"): %w", ttl, err),
			})
		} else if d < 0 {
			add("loop_server.lease_ttl", "must be >= 0, got %s", d)
		}
	}

	switch c.LoopServer.SpecPolicy.Models {
	case "", SpecModelsAny, SpecModelsRegistryOnly:
	default:
		add("loop_server.spec_policy.models", "must be %q or %q, got %q", SpecModelsAny, SpecModelsRegistryOnly, c.LoopServer.SpecPolicy.Models)
	}

	for _, f := range []struct {
		field string
		hosts []string
	}{
		{"loop_server.spec_policy.mcp_hosts", c.LoopServer.SpecPolicy.MCPHosts},
		{"loop_server.spec_policy.allow_http_hosts", c.LoopServer.SpecPolicy.AllowHTTPHosts},
	} {
		for _, h := range f.hosts {
			if h == "*" || strings.ContainsAny(h, "/:") {
				add(f.field, "%q must be a hostname or *.suffix pattern, without a scheme, port or path", h)
			}
		}
	}

	// The issuer and observability checks are operator-only surfaces and stop at
	// their first problem; each is reported under its block's path.
	if err := validateIssuers(c.LoopServer.Issuers); err != nil {
		ps = append(ps, Problem{Path: "loop_server.issuers", Message: strings.TrimPrefix(err.Error(), "config: "), Err: err})
	}
	if err := validateSecrets(c.LoopServer.Secrets); err != nil {
		ps = append(ps, Problem{Path: "loop_server.secrets", Message: strings.TrimPrefix(err.Error(), "config: "), Err: err})
	}
	ps = append(ps, limitProblems(c.LoopServer.Limits)...)
	if err := validateObservability(c.Observability); err != nil {
		ps = append(ps, Problem{Path: "observability", Message: strings.TrimPrefix(err.Error(), "config: "), Err: err})
	}

	// A workflow worker's allowlist is its whole tool set: it inherits nothing
	// from its parent, so an empty allowlist is a mistake, not "everything".
	for _, wf := range sortedKeys(c.Workflows) {
		workers := c.Workflows[wf].Workers
		for _, w := range sortedKeys(workers) {
			if len(workers[w].Tools) == 0 {
				add("workflows."+wf+".workers."+w+".tools", "must name at least one tool (a worker inherits nothing; omit spawn_agent to keep it from nesting)")
			}
		}
	}

	return ps
}

// limitProblems checks loop_server.limits (change 0103): no negative axis, and
// every override key is a tenant id a principal can actually carry.
func limitProblems(l LoopLimitsConfig) []Problem {
	var ps []Problem
	axes := func(prefix string, v LoopLimitConfig) {
		if v.MaxLiveLoops < 0 {
			ps = append(ps, Problem{Path: prefix + ".max_live_loops", Message: fmt.Sprintf("must be >= 0, got %d", v.MaxLiveLoops)})
		}
		if v.StartsPerMinute < 0 {
			ps = append(ps, Problem{Path: prefix + ".starts_per_minute", Message: fmt.Sprintf("must be >= 0, got %d", v.StartsPerMinute)})
		}
	}
	axes("loop_server.limits.default", l.Default)
	for _, k := range sortedKeys(l.Tenants) {
		if k != "_default" && !loopauth.ValidTenantID(k) {
			ps = append(ps, Problem{
				Path:    "loop_server.limits.tenants",
				Message: fmt.Sprintf("tenant %q must be _default or lowercase letters, digits, '-' or '_' (starting alphanumeric)", k),
			})
			continue
		}
		axes("loop_server.limits.tenants."+k, l.Tenants[k])
	}
	return ps
}

// sortedKeys returns m's keys in sorted order.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// validateIssuers checks loop_server.issuers. Each problem names the issuer by
// position and name so the operator can find the entry. A bad issuer refuses
// startup instead of being skipped: skipping it would turn every token that
// issuer mints into a mysterious Unauthenticated at runtime.
func validateIssuers(issuers []IssuerConfig) error {
	seen := map[string]int{}
	for i, is := range issuers {
		where := fmt.Sprintf("loop_server.issuers[%d]", i)
		if is.Issuer != "" {
			where = fmt.Sprintf("loop_server.issuers[%d] (%q)", i, is.Issuer)
		}
		if strings.TrimSpace(is.Issuer) == "" {
			return fmt.Errorf("config: %s: issuer is required (it must equal the token's iss claim)", where)
		}
		if prev, dup := seen[is.Issuer]; dup {
			return fmt.Errorf("config: %s: issuer %q is already declared by loop_server.issuers[%d]", where, is.Issuer, prev)
		}
		seen[is.Issuer] = i
		sources := 0
		if is.SigningKey != "" {
			sources++
		}
		if len(is.JWKS) > 0 {
			sources++
		}
		if is.JWKSURL != "" {
			sources++
		}
		if sources == 0 {
			return fmt.Errorf("config: %s: signing_key is required (or jwks / jwks_url for asymmetric keys)", where)
		}
		if sources > 1 {
			return fmt.Errorf("config: %s: exactly one of signing_key, jwks, jwks_url may be set", where)
		}
		for _, j := range is.JWKS {
			if _, err := loopauth.ParseJWK(loopauth.JWK{Kid: j.Kid, Kty: j.Kty, Crv: j.Crv, X: j.X, Y: j.Y}); err != nil {
				return fmt.Errorf("config: %s: jwks: %v", where, err)
			}
		}
		if is.JWKSURL != "" {
			u, err := url.Parse(is.JWKSURL)
			if err != nil || u.Host == "" {
				return fmt.Errorf("config: %s: jwks_url %q is not a valid URL", where, is.JWKSURL)
			}
			loopback := u.Hostname() == "localhost" || net.ParseIP(u.Hostname()).IsLoopback()
			if u.Scheme != "https" && !(u.Scheme == "http" && loopback) {
				return fmt.Errorf("config: %s: jwks_url must be https (http is allowed only for localhost/loopback)", where)
			}
		}
		if d, err := is.JWKSRefreshDuration(); err != nil || d < 0 {
			return fmt.Errorf("config: %s: jwks_refresh %q is not a valid duration", where, is.JWKSRefresh)
		}
		if is.TenantPrefix != "" && !loopauth.ValidTenantID(is.TenantPrefix) {
			return fmt.Errorf("config: %s: tenant_prefix %q must be lowercase letters, digits, '-' or '_'", where, is.TenantPrefix)
		}
		if is.SigningKey != "" && len(is.SigningKey) < MinIssuerSigningKeyBytes {
			return fmt.Errorf("config: %s: signing_key must be at least %d bytes for HS256, got %d", where, MinIssuerSigningKeyBytes, len(is.SigningKey))
		}
		if strings.TrimSpace(is.Audience) == "" {
			return fmt.Errorf("config: %s: audience is required (it must equal the token's aud claim)", where)
		}
		d, err := is.MaxTTLDuration()
		if err != nil {
			return fmt.Errorf("config: %s: max_ttl %q is not a valid duration (e.g. \"15m\", \"1h\"): %w", where, is.MaxTTL, err)
		}
		if d <= 0 {
			return fmt.Errorf("config: %s: max_ttl must be positive, got %s", where, d)
		}
	}
	return nil
}

var secretRefRE = regexp.MustCompile(SecretRefPattern)

// validateSecrets checks loop_server.secrets (change 0085). Like the issuers it
// stops at the first problem and names the entry by position and name.
func validateSecrets(secrets []SecretSourceConfig) error {
	seen := map[string]int{}
	for i, s := range secrets {
		where := fmt.Sprintf("loop_server.secrets[%d]", i)
		if s.Name != "" {
			where = fmt.Sprintf("loop_server.secrets[%d] (%q)", i, s.Name)
		}
		if s.Name == "" {
			return fmt.Errorf("config: %s: name is required", where)
		}
		if !secretRefRE.MatchString(s.Name) {
			return fmt.Errorf("config: %s: name must match %s", where, SecretRefPattern)
		}
		if prev, dup := seen[s.Name]; dup {
			return fmt.Errorf("config: %s: name %q is already declared by loop_server.secrets[%d]", where, s.Name, prev)
		}
		seen[s.Name] = i
		if (s.Env == "") == (s.File == "") {
			return fmt.Errorf("config: %s: exactly one of env or file is required", where)
		}
		if s.File != "" && !filepath.IsAbs(s.File) {
			return fmt.Errorf("config: %s: file must be an absolute path", where)
		}
		for _, h := range s.Hosts {
			if h == "" || strings.ContainsAny(h, ":/ ") {
				return fmt.Errorf("config: %s: hosts entry %q must be a bare hostname (no scheme, port or path)", where, h)
			}
		}
	}
	return nil
}

func validateObservability(o ObservabilityConfig) error {
	if o.Metrics.Enabled {
		if o.Metrics.Path == "" || !strings.HasPrefix(o.Metrics.Path, "/") {
			return fmt.Errorf("config: observability.metrics.path must be an absolute HTTP path")
		}
		if o.Metrics.Access != "authenticated" && o.Metrics.Access != "public" {
			return fmt.Errorf("config: observability.metrics.access must be authenticated or public")
		}
		if o.Metrics.Bind != "" {
			if _, _, err := net.SplitHostPort(o.Metrics.Bind); err != nil {
				return fmt.Errorf("config: observability.metrics.bind must be host:port: %w", err)
			}
		}
		last := float64(0)
		for _, bucket := range o.Metrics.HistogramBuckets {
			if bucket <= last {
				return fmt.Errorf("config: observability.metrics.histogram_buckets must be positive and strictly increasing")
			}
			last = bucket
		}
		declared := map[string][]string{
			"fuse_loop_operations_total": {"tenant_id", "operation", "outcome"}, "fuse_loop_operation_duration_seconds": {"tenant_id", "operation", "outcome"}, "fuse_loop_active": {"tenant_id", "operation"},
			"fuse_model_calls_total": {"tenant_id", "model", "outcome"}, "fuse_model_call_duration_seconds": {"tenant_id", "model", "outcome"}, "fuse_model_call_attempts": {"tenant_id", "model", "outcome"},
			"fuse_tool_calls_total": {"tenant_id", "tool", "outcome"}, "fuse_tool_call_duration_seconds": {"tenant_id", "tool", "outcome"}, "fuse_spawn_operations_total": {"tenant_id", "outcome"}, "fuse_spawn_operation_duration_seconds": {"tenant_id", "outcome"},
			"fuse_event_projection_total": {"event_kind", "outcome"}, "fuse_event_projection_duration_seconds": {"outcome"}, "fuse_observability_export_errors_total": {"signal", "reason"}, "fuse_observability_dropped_total": {"signal", "reason"}, "fuse_observability_log_reopens_total": {"outcome"}, "fuse_observability_log_overrides": {"scope"},
			"fuse_metrics_label_overflow_total": {"dimension", "metric"}, "fuse_metrics_label_admitted_values": {"dimension"}, "fuse_metrics_label_budget": {"dimension"},
		}
		if len(o.Metrics.Labels) > 0 {
			if len(o.Metrics.Labels) != len(declared) {
				return fmt.Errorf("config: observability.metrics.labels must declare the exact metric catalog")
			}
			for metric, want := range declared {
				got, ok := o.Metrics.Labels[metric]
				if !ok || strings.Join(got, "\x00") != strings.Join(want, "\x00") {
					return fmt.Errorf("config: observability.metrics.labels for %s do not match the exact declaration", metric)
				}
			}
		}
		for name, d := range map[string]CardinalityDimensionConfig{"tenant": o.Cardinality.Tenant, "model": o.Cardinality.Model, "tool": o.Cardinality.Tool} {
			if d.Budget < 1 {
				return fmt.Errorf("config: observability.cardinality.%s.budget must be at least 1 to reserve __overflow__", name)
			}
			if len(d.Pinned) > d.Budget-1 {
				return fmt.Errorf("config: observability.cardinality.%s pins exceed non-overflow capacity", name)
			}
		}
	}
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
	if o.Logging.Enabled {
		if o.Logging.Output != "stdout" && o.Logging.Output != "file" {
			return fmt.Errorf("config: observability.logging.output must be stdout or file")
		}
		if o.Logging.Output == "file" && strings.TrimSpace(o.Logging.File) == "" {
			return fmt.Errorf("config: file logging requires observability.logging.file")
		}
		if o.Logging.Level != "debug" && o.Logging.Level != "info" && o.Logging.Level != "warn" && o.Logging.Level != "error" {
			return fmt.Errorf("config: observability.logging.level must be debug, info, warn, or error")
		}
		d, err := time.ParseDuration(o.Logging.MaxOverrideTTL)
		if err != nil || d <= 0 {
			return fmt.Errorf("config: observability.logging.max_override_ttl must be a positive duration")
		}
	}
	return nil
}
