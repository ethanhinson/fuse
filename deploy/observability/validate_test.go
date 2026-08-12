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
