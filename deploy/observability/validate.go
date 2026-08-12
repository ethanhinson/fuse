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

type composeConfig struct {
	Services map[string]composeService `yaml:"services"`
}

type composeService struct {
	Volumes []string `yaml:"volumes"`
}

type prometheusConfig struct {
	RuleFiles     []string `yaml:"rule_files"`
	ScrapeConfigs []struct {
		JobName       string `yaml:"job_name"`
		MetricsPath   string `yaml:"metrics_path"`
		StaticConfigs []struct {
			Targets []string `yaml:"targets"`
		} `yaml:"static_configs"`
	} `yaml:"scrape_configs"`
}

type endpointConfig struct {
	Endpoint string `yaml:"endpoint"`
}

type collectorConfig struct {
	Receivers map[string]struct {
		Protocols map[string]endpointConfig `yaml:"protocols"`
	} `yaml:"receivers"`
	Exporters map[string]endpointConfig `yaml:"exporters"`
	Service   struct {
		Pipelines map[string]struct {
			Receivers []string `yaml:"receivers"`
			Exporters []string `yaml:"exporters"`
		} `yaml:"pipelines"`
	} `yaml:"service"`
}

type tempoConfig struct {
	Server struct {
		HTTPListenPort int `yaml:"http_listen_port"`
	} `yaml:"server"`
	Distributor struct {
		Receivers map[string]struct {
			Protocols map[string]endpointConfig `yaml:"protocols"`
		} `yaml:"receivers"`
	} `yaml:"distributor"`
}

type datasourceConfig struct {
	Datasources []struct {
		Name      string `yaml:"name"`
		UID       string `yaml:"uid"`
		Type      string `yaml:"type"`
		URL       string `yaml:"url"`
		IsDefault bool   `yaml:"isDefault"`
	} `yaml:"datasources"`
}

type dashboardProvisioning struct {
	Providers []struct {
		Name    string `yaml:"name"`
		Type    string `yaml:"type"`
		Options struct {
			Path string `yaml:"path"`
		} `yaml:"options"`
	} `yaml:"providers"`
}

type alertConfig struct {
	Groups []struct {
		Rules []struct {
			Alert string `yaml:"alert"`
			Expr  string `yaml:"expr"`
		} `yaml:"rules"`
	} `yaml:"groups"`
}

type dashboard struct {
	UID    string `json:"uid"`
	Panels []struct {
		Title   string `json:"title"`
		Targets []struct {
			Expr string `json:"expr"`
		} `json:"targets"`
	} `json:"panels"`
}

func main() {
	if err := validate("deploy/observability"); err != nil {
		fail(err)
	}
}

func validate(root string) error {
	var compose composeConfig
	if err := readYAML(filepath.Join(root, "docker-compose.yml"), &compose); err != nil {
		return err
	}
	for _, service := range []string{"prometheus", "grafana", "otel-collector", "tempo"} {
		if _, ok := compose.Services[service]; !ok {
			return fmt.Errorf("compose missing %s service", service)
		}
	}
	requireMount := func(service, source, target string) error {
		for _, volume := range compose.Services[service].Volumes {
			if strings.HasPrefix(volume, source+":") && strings.Contains(volume, ":"+target) {
				return nil
			}
		}
		return fmt.Errorf("compose %s service does not mount %s at %s", service, source, target)
	}
	if err := requireMount("prometheus", "./prometheus.yml", "/etc/prometheus/prometheus.yml"); err != nil {
		return err
	}
	if err := requireMount("prometheus", "./alerts.yml", "/etc/prometheus/alerts.yml"); err != nil {
		return err
	}
	if err := requireMount("otel-collector", "./otel-collector.yml", "/etc/otelcol/config.yml"); err != nil {
		return err
	}
	if err := requireMount("tempo", "./tempo.yml", "/etc/tempo.yml"); err != nil {
		return err
	}
	if err := requireMount("grafana", "./grafana/provisioning", "/etc/grafana/provisioning"); err != nil {
		return err
	}
	if err := requireMount("grafana", "./grafana/dashboards", "/var/lib/grafana/dashboards"); err != nil {
		return err
	}

	var prom prometheusConfig
	if err := readYAML(filepath.Join(root, "prometheus.yml"), &prom); err != nil {
		return err
	}
	if !contains(prom.RuleFiles, "/etc/prometheus/alerts.yml") {
		return fmt.Errorf("prometheus does not load the mounted alert rules")
	}
	if !hasFuseScrapeTarget(prom) {
		return fmt.Errorf("prometheus missing Fuse /metrics scrape target")
	}

	var collector collectorConfig
	if err := readYAML(filepath.Join(root, "otel-collector.yml"), &collector); err != nil {
		return err
	}
	if err := requireOTLPReceiver(collector.Receivers["otlp"].Protocols, "collector"); err != nil {
		return err
	}
	traces, ok := collector.Service.Pipelines["traces"]
	if !ok || !contains(traces.Receivers, "otlp") || !contains(traces.Exporters, "otlp/tempo") {
		return fmt.Errorf("collector traces pipeline must route otlp to otlp/tempo")
	}
	if collector.Exporters["otlp/tempo"].Endpoint != "tempo:4317" {
		return fmt.Errorf("collector otlp/tempo exporter must target tempo:4317")
	}

	var tempo tempoConfig
	if err := readYAML(filepath.Join(root, "tempo.yml"), &tempo); err != nil {
		return err
	}
	if tempo.Server.HTTPListenPort != 3200 {
		return fmt.Errorf("tempo HTTP API must listen on 3200")
	}
	if err := requireOTLPReceiver(tempo.Distributor.Receivers["otlp"].Protocols, "tempo"); err != nil {
		return err
	}

	var datasources datasourceConfig
	if err := readYAML(filepath.Join(root, "grafana/provisioning/datasources/datasources.yml"), &datasources); err != nil {
		return err
	}
	if !hasDatasource(datasources, "prometheus", "prometheus", "http://prometheus:9090", true) {
		return fmt.Errorf("grafana must provision the default Prometheus datasource")
	}
	if !hasDatasource(datasources, "tempo", "tempo", "http://tempo:3200", false) {
		return fmt.Errorf("grafana must provision the Tempo datasource")
	}
	var provisioning dashboardProvisioning
	if err := readYAML(filepath.Join(root, "grafana/provisioning/dashboards/dashboards.yml"), &provisioning); err != nil {
		return err
	}
	if !hasDashboardProvider(provisioning) {
		return fmt.Errorf("grafana must provision dashboards from /var/lib/grafana/dashboards")
	}

	var board dashboard
	if err := readJSON(filepath.Join(root, "grafana/dashboards/fuse-loop.json"), &board); err != nil {
		return err
	}
	if board.UID != "fuse-loop" {
		return fmt.Errorf("dashboard UID must be fuse-loop")
	}
	if err := validateDashboard(board); err != nil {
		return err
	}

	var alerts alertConfig
	if err := readYAML(filepath.Join(root, "alerts.yml"), &alerts); err != nil {
		return err
	}
	return validateAlerts(alerts)
}

func requireOTLPReceiver(protocols map[string]endpointConfig, owner string) error {
	if protocols["grpc"].Endpoint != "0.0.0.0:4317" || protocols["http"].Endpoint != "0.0.0.0:4318" {
		return fmt.Errorf("%s OTLP receiver must expose grpc :4317 and http :4318", owner)
	}
	return nil
}

func hasFuseScrapeTarget(config prometheusConfig) bool {
	for _, scrape := range config.ScrapeConfigs {
		if scrape.JobName != "fuse" || scrape.MetricsPath != "/metrics" {
			continue
		}
		for _, static := range scrape.StaticConfigs {
			if contains(static.Targets, "host.docker.internal:9090") {
				return true
			}
		}
	}
	return false
}

func hasDatasource(config datasourceConfig, uid, kind, url string, defaultSource bool) bool {
	for _, datasource := range config.Datasources {
		if datasource.UID == uid && datasource.Type == kind && datasource.URL == url && datasource.IsDefault == defaultSource {
			return true
		}
	}
	return false
}

func hasDashboardProvider(config dashboardProvisioning) bool {
	for _, provider := range config.Providers {
		if provider.Name == "Fuse" && provider.Type == "file" && provider.Options.Path == "/var/lib/grafana/dashboards" {
			return true
		}
	}
	return false
}

func validateDashboard(board dashboard) error {
	expected := map[string]string{
		"Loop throughput": "fuse_loop_operations_total", "Loop outcomes": "fuse_loop_operations_total", "Loop p95 latency": "fuse_loop_operation_duration_seconds_bucket", "Active loops": "fuse_loop_active",
		"Model retries and timeouts": "fuse_model_", "Tool and spawn failures": "fuse_tool_calls_total", "Projector and exporter failures": "fuse_observability_export_errors_total", "Observation drops": "fuse_observability_dropped_total", "Active log overrides": "fuse_observability_log_overrides", "Cardinality health": "fuse_metrics_label_",
	}
	seen := make(map[string]bool)
	for _, panel := range board.Panels {
		metric, required := expected[panel.Title]
		if !required {
			continue
		}
		seen[panel.Title] = true
		if len(panel.Targets) == 0 {
			return fmt.Errorf("dashboard panel %q has no PromQL targets", panel.Title)
		}
		matched := false
		for _, target := range panel.Targets {
			if !validPromQL(target.Expr) {
				return fmt.Errorf("dashboard panel %q has invalid PromQL structure", panel.Title)
			}
			if strings.Contains(target.Expr, metric) {
				matched = true
			}
		}
		if !matched {
			return fmt.Errorf("dashboard panel %q does not query %s", panel.Title, metric)
		}
	}
	for title := range expected {
		if !seen[title] {
			return fmt.Errorf("dashboard missing required panel %q", title)
		}
	}
	return nil
}

func validateAlerts(config alertConfig) error {
	expected := map[string]string{"FuseHighErrorRate": "fuse_loop_operations_total", "FuseHighLatency": "fuse_loop_operation_duration_seconds_bucket", "FuseOverflowAttributionIncomplete": "fuse_metrics_label_overflow_total", "FuseExporterErrors": "fuse_observability_export_errors_total", "FuseDroppedObservations": "fuse_observability_dropped_total"}
	seen := make(map[string]bool)
	for _, group := range config.Groups {
		for _, rule := range group.Rules {
			metric, required := expected[rule.Alert]
			if !required {
				continue
			}
			seen[rule.Alert] = true
			if !validPromQL(rule.Expr) || !strings.Contains(rule.Expr, metric) {
				return fmt.Errorf("alert %s must contain valid PromQL for %s", rule.Alert, metric)
			}
			if (rule.Alert == "FuseHighErrorRate" || rule.Alert == "FuseHighLatency") && !strings.Contains(rule.Expr, `tenant_id!="__overflow__"`) {
				return fmt.Errorf("alert %s must exclude __overflow__ tenant attribution", rule.Alert)
			}
		}
	}
	for alert := range expected {
		if !seen[alert] {
			return fmt.Errorf("alerts missing %s", alert)
		}
	}
	return nil
}

func validPromQL(expr string) bool {
	if strings.TrimSpace(expr) == "" {
		return false
	}
	pairs := map[rune]rune{')': '(', ']': '[', '}': '{'}
	stack := make([]rune, 0, 8)
	for _, r := range expr {
		switch r {
		case '(', '[', '{':
			stack = append(stack, r)
		case ')', ']', '}':
			if len(stack) == 0 || stack[len(stack)-1] != pairs[r] {
				return false
			}
			stack = stack[:len(stack)-1]
		}
	}
	return len(stack) == 0
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func readYAML(path string, out any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := yaml.Unmarshal(b, out); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	return nil
}

func readJSON(path string, out any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	return nil
}

func fail(err error) { fmt.Fprintln(os.Stderr, "observability artifact validation:", err); os.Exit(1) }
