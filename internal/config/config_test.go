package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestObservabilitySignalsValidateIndependently(t *testing.T) {
	t.Run("disabled requires no backend", func(t *testing.T) {
		c := Config{Observability: ObservabilityConfig{
			Metrics:     MetricsObservabilityConfig{Path: "not-a-path", Access: "wrong"},
			Traces:      TracesObservabilityConfig{Endpoint: "not-an-endpoint", Protocol: "wrong"},
			Logging:     LoggingObservabilityConfig{Output: "wrong", Level: "wrong"},
			Cardinality: CardinalityObservabilityConfig{Tenant: CardinalityDimensionConfig{Budget: -1, Pinned: []string{"x"}}},
		}}
		if err := c.Validate(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("enabled trace requires complete adapter", func(t *testing.T) {
		c := Config{Observability: ObservabilityConfig{Traces: TracesObservabilityConfig{Enabled: true}}}
		if err := c.Validate(); err == nil {
			t.Fatal("expected invalid enabled trace adapter")
		}
	})
	t.Run("complete independent signals", func(t *testing.T) {
		c := Config{Observability: ObservabilityConfig{
			Metrics:     MetricsObservabilityConfig{Enabled: true, Path: "/metrics", Bind: "127.0.0.1:9090", Access: "authenticated", HistogramBuckets: []float64{.01, .1, 1}},
			Traces:      TracesObservabilityConfig{Enabled: true, Endpoint: "collector:4317", Protocol: "grpc", Headers: map[string]string{"authorization": "secret"}, QueueSize: 16, BatchSize: 8, ExportTimeout: "2s", BatchTimeout: "1s", SampleRatio: .25},
			Logging:     LoggingObservabilityConfig{Enabled: true, Output: "file", File: "/tmp/fuse.log", Level: "info", MaxOverrideTTL: "15m"},
			Cardinality: CardinalityObservabilityConfig{HashVersion: "sha256-64-v1", Salt: "deploy", Tenant: CardinalityDimensionConfig{Budget: 2, Pinned: []string{"acme"}, Catalog: []string{"acme", "globex"}}, Model: CardinalityDimensionConfig{Budget: 1, Catalog: []string{"m"}}, Tool: CardinalityDimensionConfig{Budget: 1, Catalog: []string{"read"}}},
		}}
		if err := c.Validate(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestObservabilityRejectsInvalidAccessBatchAndLogging(t *testing.T) {
	cases := []ObservabilityConfig{
		{Metrics: MetricsObservabilityConfig{Enabled: true, Path: "/metrics", Access: "open-ish"}},
		{Traces: TracesObservabilityConfig{Enabled: true, Endpoint: "x", Protocol: "grpc", QueueSize: 1, BatchSize: 2}},
		{Logging: LoggingObservabilityConfig{Enabled: true, Output: "file", Level: "info", MaxOverrideTTL: "1m"}},
	}
	for i, o := range cases {
		if err := (Config{Observability: o}).Validate(); err == nil {
			t.Errorf("case %d unexpectedly valid", i)
		}
	}
}

func TestObservabilityRejectsPartialOrChangedMetricLabelDeclarations(t *testing.T) {
	c := Config{Observability: ObservabilityConfig{Metrics: MetricsObservabilityConfig{Enabled: true, Path: "/metrics", Access: "authenticated", Labels: map[string][]string{"fuse_loop_active": {"tenant_id", "loop_id"}}}}}
	if err := c.Validate(); err == nil {
		t.Fatal("changed/partial label catalog accepted")
	}
}

func TestObservabilityLoadsOnlyFromTrustedConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	body := []byte("observability:\n  instance_id: node-a\n  traces:\n    enabled: true\n    endpoint: collector:4317\n    protocol: grpc\n    insecure: true\n    headers: {authorization: secret}\n    queue_size: 32\n    batch_size: 8\n    export_timeout: 2s\n    batch_timeout: 1s\n    sample_ratio: 0.5\n")
	if err := os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	trusted := Default()
	if err := mergeFile(&trusted, path, true, nil); err != nil {
		t.Fatal(err)
	}
	if !trusted.Observability.Traces.Enabled || trusted.Observability.Traces.Headers["authorization"] != "secret" || trusted.Observability.InstanceID != "node-a" {
		t.Fatalf("trusted observability not loaded: %+v", trusted.Observability)
	}
	untrusted := Default()
	if err := mergeFile(&untrusted, path, false, nil); err != nil {
		t.Fatal(err)
	}
	if untrusted.Observability.Traces.Enabled {
		t.Fatal("untrusted telemetry egress configuration was accepted")
	}
}

func TestTraceSampleRatioDefaultsOnlyWhenOmitted(t *testing.T) {
	if got := Default().Observability.Traces.SampleRatio; got != 1 {
		t.Fatalf("default sample ratio = %v, want 1", got)
	}

	path := filepath.Join(t.TempDir(), "config.yml")
	base := "observability:\n  traces:\n    enabled: true\n    endpoint: collector:4317\n    protocol: grpc\n"
	if err := os.WriteFile(path, []byte(base), 0600); err != nil {
		t.Fatal(err)
	}
	omitted := Default()
	if err := mergeFile(&omitted, path, true, nil); err != nil {
		t.Fatal(err)
	}
	if got := omitted.Observability.Traces.SampleRatio; got != 1 {
		t.Fatalf("omitted sample ratio = %v, want 1", got)
	}

	if err := os.WriteFile(path, []byte(base+"    sample_ratio: 0\n"), 0600); err != nil {
		t.Fatal(err)
	}
	explicitZero := Default()
	if err := mergeFile(&explicitZero, path, true, nil); err != nil {
		t.Fatal(err)
	}
	if got := explicitZero.Observability.Traces.SampleRatio; got != 0 {
		t.Fatalf("explicit zero sample ratio = %v, want 0", got)
	}
}
