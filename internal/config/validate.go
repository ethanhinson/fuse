package config

import (
	"fmt"
	"net"
	"strings"
	"time"
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

// Validate performs structural validation of a resolved Config and returns the
// first problem found, or nil. It deliberately does NOT resolve model aliases —
// that requires the model.Registry, which lives above this leaf package, so
// model-reference checks are done by the caller (see cmd/fuse) where both the
// config and the registry are in scope. Validate covers the checks that need
// only the config itself: enum membership and numeric sanity.
//
// The loader already clamps some numerics (e.g. negative MaxSpawns → 64); this
// method catches the values the loader passes through untouched and the enums it
// silently defaults, so a malformed config fails loudly at startup rather than
// behaving surprisingly at runtime.
func (c Config) Validate() error {
	if m := c.Permissions.Mode; m != "" && !validModes[m] {
		return fmt.Errorf("config: permissions.mode %q is invalid (want off, prompt-all, smart, or auto)", m)
	}

	// Note: agents.tool_timeout_seconds is intentionally NOT checked here. The
	// loader's tighten-only merge (ADR-0006) only applies a value when it is > 0
	// and silently drops a negative, so a negative can never reach the resolved
	// Config — a Validate check for it would be dead code implying a protection
	// that does not exist. MaxTurns/MaxTokens ARE passed through unclamped, so
	// they are checked below.
	if c.MaxTokens < 0 {
		return fmt.Errorf("config: max_tokens must be >= 0, got %d", c.MaxTokens)
	}
	if c.MaxTurns != nil && *c.MaxTurns < 0 {
		return fmt.Errorf("config: max_turns must be >= 0 (0 = unlimited), got %d", *c.MaxTurns)
	}

	// Summarization threshold is a context-window fraction; only 0 (unset) or a
	// value in (0,1] is meaningful. A value >1 or <0 would never (or always)
	// trigger, silently defeating Tier-2 summarization.
	if t := c.Context.Summarization.Threshold; t < 0 || t > 1 {
		return fmt.Errorf("config: context.summarization.threshold must be in [0,1], got %g", t)
	}
	// Relevance borderline band must be ordered and within [0,1] when set.
	rel := c.Context.Relevance
	if rel.BorderlineLo < 0 || rel.BorderlineHi > 1 || (rel.BorderlineHi > 0 && rel.BorderlineLo > rel.BorderlineHi) {
		return fmt.Errorf("config: context.relevance borderline band [%g,%g] is invalid (want 0<=lo<=hi<=1)", rel.BorderlineLo, rel.BorderlineHi)
	}
	if rel.RecencyFloorPct < 0 || rel.RecencyFloorPct > 100 {
		return fmt.Errorf("config: context.relevance.recency_floor_pct must be in [0,100], got %d", rel.RecencyFloorPct)
	}

	// loop_server.lease_ttl (change 0049) is a Go duration string when set; a
	// typo ("30" without a unit, "sec") must fail loudly at startup rather than
	// silently falling back to the runtime default and giving a lease TTL nobody
	// asked for. Empty = unset (the runtime's built-in default) and is fine.
	if ttl := c.LoopServer.LeaseTTL; ttl != "" {
		d, err := time.ParseDuration(ttl)
		if err != nil {
			return fmt.Errorf("config: loop_server.lease_ttl %q is not a valid duration (e.g. \"30s\", \"2m\"): %w", ttl, err)
		}
		if d < 0 {
			return fmt.Errorf("config: loop_server.lease_ttl must be >= 0, got %s", d)
		}
	}

	if err := validateObservability(c.Observability); err != nil {
		return err
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
