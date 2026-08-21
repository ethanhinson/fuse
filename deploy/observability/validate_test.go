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
