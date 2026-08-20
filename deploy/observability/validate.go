// validate checks that the development observability stack remains wired to the
// Fuse metric and tracing surfaces. Run with: go run ./deploy/observability/validate.go
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	fusemetrics "github.com/ethanhinson/fuse/internal/observe/prometheus"
	"gopkg.in/yaml.v3"
)

// metricRef matches any Fuse metric identifier appearing in a PromQL
// expression, including label matchers such as {metric="fuse_sandbox_active"}.
var metricRef = regexp.MustCompile(`fuse_[a-z0-9_]+`)

// registeredMetrics is the set of series names the Prometheus recorder actually
// registers, taken from the recorder's own catalog rather than restated here so
// the two cannot drift. Histogram families additionally expose the derived
// _bucket/_sum/_count series.
func registeredMetrics() map[string]bool {
	set := make(map[string]bool)
	for _, d := range fusemetrics.Catalog() {
		set[d.Name] = true
		if d.Type == "histogram" {
			for _, suffix := range []string{"_bucket", "_sum", "_count"} {
				set[d.Name+suffix] = true
			}
		}
	}
	return set
}

// requireRegisteredMetrics is the falsifiable half of expression validation: a
// balanced-bracket check proves only that a query parses, not that it can ever
// return data. Every Fuse identifier a query names must be a series the
// recorder registers, so a dashboard or alert pointed at a nonexistent series
// fails this gate instead of silently rendering empty forever.
func requireRegisteredMetrics(owner, expr string, registered map[string]bool) error {
	for _, name := range metricRef.FindAllString(expr, -1) {
		if !registered[name] {
			return fmt.Errorf("%s queries %q, which the Prometheus recorder never registers", owner, name)
		}
	}
	return nil
}

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

type panelDatasource struct {
	Type string `json:"type"`
	UID  string `json:"uid"`
}

type dashboard struct {
	UID    string `json:"uid"`
	Panels []struct {
		Title      string           `json:"title"`
		Type       string           `json:"type"`
		Datasource *panelDatasource `json:"datasource"`
		Targets    []struct {
			Expr       string           `json:"expr"`
			Query      string           `json:"query"`
			Datasource *panelDatasource `json:"datasource"`
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

	registered := registeredMetrics()
	provisioned := make(map[string]string, len(datasources.Datasources))
	for _, ds := range datasources.Datasources {
		provisioned[ds.UID] = ds.Type
	}

	var board dashboard
	if err := readJSON(filepath.Join(root, "grafana/dashboards/fuse-loop.json"), &board); err != nil {
		return err
	}
	if board.UID != "fuse-loop" {
		return fmt.Errorf("dashboard UID must be fuse-loop")
	}
	if err := validateDashboard(board, loopDashboardPanels, registered, provisioned); err != nil {
		return err
	}

	var sandboxBoard dashboard
	if err := readJSON(filepath.Join(root, "grafana/dashboards/fuse-sandbox.json"), &sandboxBoard); err != nil {
		return err
	}
	if sandboxBoard.UID != "fuse-sandbox" {
		return fmt.Errorf("dashboard UID must be fuse-sandbox")
	}
	if err := validateDashboard(sandboxBoard, sandboxDashboardPanels, registered, provisioned); err != nil {
		return err
	}

	var alerts alertConfig
	if err := readYAML(filepath.Join(root, "alerts.yml"), &alerts); err != nil {
		return err
	}
	return validateAlerts(alerts, registered)
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

var loopDashboardPanels = map[string]string{
	"Loop throughput": "fuse_loop_operations_total", "Loop outcomes": "fuse_loop_operations_total", "Loop p95 latency": "fuse_loop_operation_duration_seconds_bucket", "Active loops": "fuse_loop_active",
	"Model retries and timeouts": "fuse_model_", "Tool and spawn failures": "fuse_tool_calls_total", "Projector and exporter failures": "fuse_observability_export_errors_total", "Observation drops": "fuse_observability_dropped_total", "Active log overrides": "fuse_observability_log_overrides", "Cardinality health": "fuse_metrics_label_",
}

// sandboxDashboardPanels covers every sandbox panel, and validateDashboard
// requires the coverage to be exhaustive in both directions.
//
// There is deliberately no loop->container panel. Mapping a running loop to its
// container needs container_id, which is a correlation token rather than a
// metric dimension and is excluded from the recorder catalog on purpose. The
// projected Record does carry it, but that projection reaches only the JSON log
// sink: this stack provisions Prometheus and Tempo and no logs datasource, and
// no fuse.sandbox.* span exists either — span names are minted solely from an
// observe.Descriptor in internal/observe/otel, which the sandbox package must
// not import. Restore the panel once one of those series genuinely exists, and
// teach this validator to check it then.
var sandboxDashboardPanels = map[string]string{
	"Active sandboxes by handler/runtime": "fuse_sandbox_active",
	"Cold-start latency heatmap":          "fuse_sandbox_cold_start_seconds_bucket",
	"Unhealthy by reason":                 "fuse_sandbox_unhealthy_total",
	"Reap rate by cause":                  "fuse_sandbox_reaped_total",
}

func validateDashboard(board dashboard, expected map[string]string, registered map[string]bool, provisioned map[string]string) error {
	seen := make(map[string]bool)
	for _, panel := range board.Panels {
		metric, required := expected[panel.Title]
		if !required {
			// Exhaustive in this direction too: an unrecognised panel is an
			// unvalidated panel, and an unvalidated panel is how a query
			// against a series nothing produces gets shipped.
			return fmt.Errorf("dashboard %s panel %q has no declared expectation; add it to the panel map", board.UID, panel.Title)
		}
		seen[panel.Title] = true
		if err := requirePrometheusDatasource(panel.Title, panel.Datasource, provisioned); err != nil {
			return err
		}
		if len(panel.Targets) == 0 {
			return fmt.Errorf("dashboard panel %q has no PromQL targets", panel.Title)
		}
		matched := false
		for _, target := range panel.Targets {
			if err := requirePrometheusDatasource(panel.Title, target.Datasource, provisioned); err != nil {
				return err
			}
			if strings.TrimSpace(target.Query) != "" {
				return fmt.Errorf("dashboard panel %q uses a non-PromQL query; only Prometheus-backed panels can be validated today", panel.Title)
			}
			if !validPromQL(target.Expr) {
				return fmt.Errorf("dashboard panel %q has invalid PromQL structure", panel.Title)
			}
			if err := requireRegisteredMetrics(fmt.Sprintf("dashboard panel %q", panel.Title), target.Expr, registered); err != nil {
				return err
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

// requirePrometheusDatasource rejects any datasource this validator cannot hold
// to a real series. An explicit UID must be one Grafana actually provisions and
// must match that datasource's type; beyond that, only Prometheus panels are
// permitted, because Prometheus is the sole signal whose series names can be
// checked against the emitting code (see registeredMetrics). A trace- or
// logs-backed panel is accepted only once this function learns to verify the
// spans or streams it names actually exist.
func requirePrometheusDatasource(title string, ds *panelDatasource, provisioned map[string]string) error {
	if ds == nil || (ds.UID == "" && ds.Type == "") {
		return nil // inherits the provisioned default, which is Prometheus.
	}
	if ds.UID != "" {
		kind, ok := provisioned[ds.UID]
		if !ok {
			return fmt.Errorf("dashboard panel %q references datasource %q, which Grafana never provisions", title, ds.UID)
		}
		if ds.Type != "" && ds.Type != kind {
			return fmt.Errorf("dashboard panel %q declares datasource %q as type %q, but it is provisioned as %q", title, ds.UID, ds.Type, kind)
		}
	}
	if ds.Type != "" && ds.Type != "prometheus" {
		return fmt.Errorf("dashboard panel %q uses the %q datasource; only Prometheus panels can be validated against an emitted series today", title, ds.Type)
	}
	return nil
}

func validateAlerts(config alertConfig, registered map[string]bool) error {
	expected := map[string]string{
		"FuseHighErrorRate": "fuse_loop_operations_total", "FuseHighLatency": "fuse_loop_operation_duration_seconds_bucket", "FuseOverflowAttributionIncomplete": "fuse_metrics_label_overflow_total", "FuseExporterErrors": "fuse_observability_export_errors_total", "FuseDroppedObservations": "fuse_observability_dropped_total",
		"FuseSandboxUnhealthy": "fuse_sandbox_unhealthy_total", "FuseSandboxIdleTTLLeak": "fuse_sandbox_reaped_total", "FuseSandboxPoolNotDraining": "fuse_sandbox_active", "FuseSandboxStaleCheckout": "fuse_sandbox_reaped_total",
	}
	seen := make(map[string]bool)
	for _, group := range config.Groups {
		for _, rule := range group.Rules {
			// Every rule is held to the registered-series check, declared or
			// not: an undeclared alert firing on a metric nothing emits is the
			// same silent failure as an undeclared panel.
			if err := requireRegisteredMetrics("alert "+rule.Alert, rule.Expr, registered); err != nil {
				return err
			}
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
