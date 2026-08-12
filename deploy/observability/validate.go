// validate checks that the development observability stack remains wired to the
// Fuse metric and tracing surfaces. Run with: go run ./deploy/observability/validate.go
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

var yamlArtifacts = []string{
	"docker-compose.yml", "prometheus.yml", "otel-collector.yml", "tempo.yml", "alerts.yml",
	"grafana/provisioning/datasources/datasources.yml", "grafana/provisioning/dashboards/dashboards.yml",
}

func main() {
	root := "deploy/observability"
	for _, name := range yamlArtifacts {
		parseYAML(filepath.Join(root, name))
	}
	parseJSON(filepath.Join(root, "grafana/dashboards/fuse-loop.json"))
	assertContains(filepath.Join(root, "docker-compose.yml"), "prometheus:", "grafana:", "otel-collector:", "tempo:")
	assertContains(filepath.Join(root, "prometheus.yml"), "host.docker.internal:9090", "alerts.yml")
	assertContains(filepath.Join(root, "otel-collector.yml"), "otlp:", "endpoint: 0.0.0.0:4317", "endpoint: 0.0.0.0:4318", "tempo:4317")
	assertContains(filepath.Join(root, "tempo.yml"), "otlp:", "endpoint: 0.0.0.0:4317", "endpoint: 0.0.0.0:4318")
	assertContains(filepath.Join(root, "grafana/provisioning/datasources/datasources.yml"), "Prometheus", "Tempo", "http://prometheus:9090", "http://tempo:3200")
	assertContains(filepath.Join(root, "grafana/provisioning/dashboards/dashboards.yml"), "/var/lib/grafana/dashboards")
	assertContains(filepath.Join(root, "grafana/dashboards/fuse-loop.json"), `"uid":"fuse-loop"`)
	assertContains(filepath.Join(root, "alerts.yml"), "FuseHighErrorRate", "FuseHighLatency", "FuseOverflowAttributionIncomplete", "FuseExporterErrors", "FuseDroppedObservations")
}

func parseYAML(path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		fail(err)
	}
	var value any
	if err := yaml.Unmarshal(b, &value); err != nil {
		fail(fmt.Errorf("parse %s: %w", path, err))
	}
}

func parseJSON(path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		fail(err)
	}
	var value any
	if err := json.Unmarshal(b, &value); err != nil {
		fail(fmt.Errorf("parse %s: %w", path, err))
	}
}

func assertContains(path string, values ...string) {
	b, err := os.ReadFile(path)
	if err != nil {
		fail(err)
	}
	for _, value := range values {
		if !strings.Contains(string(b), value) {
			fail(fmt.Errorf("%s missing %q", path, value))
		}
	}
}

func fail(err error) { fmt.Fprintln(os.Stderr, "observability artifact validation:", err); os.Exit(1) }
