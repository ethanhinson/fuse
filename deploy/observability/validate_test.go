package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateRejectsBrokenCollectorRoute(t *testing.T) {
	root := copyArtifacts(t)
	replaceArtifact(t, root, "otel-collector.yml", "exporters: [otlp/tempo]", "exporters: []")
	if err := validate(root); err == nil {
		t.Fatal("validate accepted a traces pipeline with no Tempo exporter")
	}
}

func TestValidateRejectsBrokenGrafanaDatasource(t *testing.T) {
	root := copyArtifacts(t)
	replaceArtifact(t, root, "grafana/provisioning/datasources/datasources.yml", "http://prometheus:9090", "http://wrong:9090")
	if err := validate(root); err == nil {
		t.Fatal("validate accepted a Grafana datasource disconnected from Prometheus")
	}
}

func TestValidateRejectsInvalidDashboardPromQL(t *testing.T) {
	root := copyArtifacts(t)
	replaceArtifact(t, root, "grafana/dashboards/fuse-loop.json", "sum(rate(fuse_loop_operations_total[5m]))", "sum(rate(fuse_loop_operations_total[5m])")
	if err := validate(root); err == nil {
		t.Fatal("validate accepted malformed dashboard PromQL")
	}
}

func TestValidateRejectsAlertMetricMismatch(t *testing.T) {
	root := copyArtifacts(t)
	replaceArtifact(t, root, "alerts.yml", "fuse_observability_dropped_total", "fuse_not_observability_dropped_total")
	if err := validate(root); err == nil {
		t.Fatal("validate accepted an alert disconnected from its metric")
	}
}

// The next four tests exist because a non-empty-string assertion is not
// validation: a panel or alert that queries a series nothing in this repo can
// ever produce must fail the gate, not pass it.

func TestValidateRejectsPanelQueryingUnregisteredMetric(t *testing.T) {
	root := copyArtifacts(t)
	replaceArtifact(t, root, "grafana/dashboards/fuse-sandbox.json",
		`sum(fuse_sandbox_active) by (handler, runtime)`,
		`sum(fuse_sandbox_active) by (handler, runtime) + sum(fuse_sandbox_ghost)`)
	if err := validate(root); err == nil {
		t.Fatal("validate accepted a panel querying a metric the recorder never registers")
	}
}

func TestValidateRejectsAlertQueryingUnregisteredMetric(t *testing.T) {
	root := copyArtifacts(t)
	replaceArtifact(t, root, "alerts.yml",
		`sum(rate(fuse_sandbox_unhealthy_total[5m])) by (handler, reason) > 0`,
		`sum(rate(fuse_sandbox_unhealthy_total[5m])) by (handler, reason) > 0 or fuse_sandbox_ghost > 0`)
	if err := validate(root); err == nil {
		t.Fatal("validate accepted an alert querying a metric the recorder never registers")
	}
}

// A trace-backed panel is exactly the shape that slipped through before: no
// fuse.sandbox.* span is producible, and no logs datasource is provisioned, so
// a non-Prometheus panel must be rejected until such a series actually exists.
func TestValidateRejectsTraceBackedPanel(t *testing.T) {
	root := copyArtifacts(t)
	replaceArtifact(t, root, "grafana/dashboards/fuse-sandbox.json",
		`{"title":"Active sandboxes by handler/runtime","type":"timeseries","targets":[`,
		`{"title":"Active sandboxes by handler/runtime","type":"timeseries","datasource":{"type":"tempo","uid":"tempo"},"targets":[`)
	if err := validate(root); err == nil {
		t.Fatal("validate accepted a Tempo panel querying spans nothing emits")
	}
}

func TestValidateRejectsUnvalidatedPanel(t *testing.T) {
	root := copyArtifacts(t)
	replaceArtifact(t, root, "grafana/dashboards/fuse-sandbox.json", `"panels":[`, `"panels":[
{"title":"Something nobody validates","type":"timeseries","targets":[{"expr":"sum(fuse_sandbox_active)"}]},`)
	if err := validate(root); err == nil {
		t.Fatal("validate accepted a dashboard panel that no expectation covers")
	}
}

func TestValidateAcceptsReferenceArtifacts(t *testing.T) {
	if err := validate("."); err != nil {
		t.Fatalf("validate reference artifacts: %v", err)
	}
}

func replaceArtifact(t *testing.T, root, name, old, new string) {
	t.Helper()
	path := filepath.Join(root, name)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	updated := strings.Replace(string(b), old, new, 1)
	if updated == string(b) {
		t.Fatalf("%s does not contain %q", name, old)
	}
	if err := os.WriteFile(path, []byte(updated), 0o600); err != nil {
		t.Fatal(err)
	}
}

func copyArtifacts(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := filepath.Walk(".", func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || filepath.Ext(path) == ".go" {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		dest := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		return os.WriteFile(dest, b, 0o600)
	}); err != nil {
		t.Fatal(err)
	}
	return root
}

// The compose stack's own prometheus.yml (docket change 0076) is validated by a
// second, narrower entry point. Both negatives below are injected, not natural:
// the shipped file names no fuse_* series at all, and targets fuse:9090.

func TestValidateComposeAcceptsReferenceArtifacts(t *testing.T) {
	if err := validateCompose("../compose", "fuse:9090"); err != nil {
		t.Fatalf("validateCompose reference artifacts: %v", err)
	}
}

func TestValidateComposeRejectsUnregisteredMetric(t *testing.T) {
	root := copyComposeArtifacts(t)
	replaceArtifact(t, root, "prometheus.yml", "  - /etc/prometheus/alerts.yml",
		"  - /etc/prometheus/alerts.yml\n# keep: fuse_sandbox_ghost")
	if err := validateCompose(root, "fuse:9090"); err == nil {
		t.Fatal("validateCompose accepted a config naming a metric the recorder never registers")
	}
}

func TestValidateComposeRejectsHostScrapeTarget(t *testing.T) {
	root := copyComposeArtifacts(t)
	replaceArtifact(t, root, "prometheus.yml", `targets: ["fuse:9090"]`, `targets: ["host.docker.internal:9090"]`)
	if err := validateCompose(root, "fuse:9090"); err == nil {
		t.Fatal("validateCompose accepted the standalone stack's host target for a compose service")
	}
}

func TestValidateComposeRejectsMissingAlertRules(t *testing.T) {
	root := copyComposeArtifacts(t)
	replaceArtifact(t, root, "prometheus.yml", "  - /etc/prometheus/alerts.yml", "  - /etc/prometheus/other.yml")
	if err := validateCompose(root, "fuse:9090"); err == nil {
		t.Fatal("validateCompose accepted a config that does not load the mounted alert rules")
	}
}

func TestValidateComposeRejectsTopLevelOverrideOfIncludedService(t *testing.T) {
	root := copyComposeArtifacts(t)
	// The pre-fix shape: overriding the included prometheus service under this
	// file's own services: — what Compose v2.38 rejects as "conflicts with
	// imported resource".
	replaceArtifact(t, root, "docker-compose.yml", "services:\n  fuse: &fuse",
		"services:\n  prometheus:\n    volumes:\n      - ./prometheus.yml:/etc/prometheus/prometheus.yml:ro\n  fuse: &fuse")
	if err := validateCompose(root, "fuse:9090"); err == nil {
		t.Fatal("validateCompose accepted a top-level override of an included service")
	}
}

func TestValidateComposeRejectsOverrideOutsideIncludePathList(t *testing.T) {
	root := copyComposeArtifacts(t)
	replaceArtifact(t, root, "docker-compose.yml", "      - ./prometheus-override.yml\n", "")
	if err := validateCompose(root, "fuse:9090"); err == nil {
		t.Fatal("validateCompose accepted an include that does not merge the prometheus override")
	}
}

func TestValidateComposeRejectsOverrideMountRelativeToWrongDir(t *testing.T) {
	root := copyComposeArtifacts(t)
	replaceArtifact(t, root, "prometheus-override.yml", "../compose/prometheus.yml:", "./prometheus.yml:")
	if err := validateCompose(root, "fuse:9090"); err == nil {
		t.Fatal("validateCompose accepted an override mount that resolves against the wrong directory")
	}
}

// copyComposeArtifacts mirrors copyArtifacts for the compose stack's directory,
// which sits beside this package rather than inside it.
func copyComposeArtifacts(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	entries, err := os.ReadDir("../compose")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join("../compose", entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, entry.Name()), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}
