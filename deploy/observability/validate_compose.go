// validateCompose is the compose dev stack's half of artifact validation
// (docket change 0076). It is deliberately a SECOND, narrower entry point
// rather than validate() with a different root: validate() demands the full
// four-sidecar reference shape (prometheus + grafana + otel-collector + tempo,
// two provisioned datasources, two dashboard JSONs), and the compose stack
// owns none of that — it owns exactly one file, the prometheus.yml it mounts
// over the inherited one. What it does share is the falsifiable core:
// registeredMetrics / requireRegisteredMetrics / readYAML.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// validateCompose checks the compose stack's own prometheus.yml: it scrapes
// fuse at wantTarget (a service on the compose network, not the host), it loads
// the alerts file that is still mounted from deploy/observability/, and every
// Fuse series it names is one the recorder actually registers.
func validateCompose(root, wantTarget string) error {
	if err := validateComposeIncludeShape(root); err != nil {
		return err
	}
	var prom prometheusConfig
	path := filepath.Join(root, "prometheus.yml")
	if err := readYAML(path, &prom); err != nil {
		return err
	}
	if !contains(prom.RuleFiles, "/etc/prometheus/alerts.yml") {
		return fmt.Errorf("compose prometheus does not load the mounted alert rules")
	}
	if !hasFuseScrapeTarget(prom, wantTarget) {
		return fmt.Errorf("compose prometheus missing Fuse /metrics scrape target %s", wantTarget)
	}
	// The compose prometheus.yml has no PromQL field of its own, so the check
	// is over its raw text: any fuse_* identifier appearing anywhere in it
	// (a relabel rule, a metric_relabel_configs regex, a recording target)
	// must be a registered series, or the file silently references nothing.
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return requireRegisteredMetrics("compose prometheus.yml", string(b), registeredMetrics())
}

// composeIncludeShape is the slice of deploy/compose/docker-compose.yml this
// guard reads: the include list and the top-level service names.
type composeIncludeShape struct {
	Include []struct {
		Path []string `yaml:"path"`
	} `yaml:"include"`
	Services map[string]struct{} `yaml:"services"`
}

// validateComposeIncludeShape is a Docker-free guard for a regression that
// only a newer Compose reports: v2.24+ (v2.38 on the CI runner) refuses a
// top-level `services.<name>:` that also arrives through `include:` with
// "conflicts with imported resource", while some other releases (v5.1 locally)
// accept it silently. So the check is structural: none of the observability
// stack's services may be declared at this file's top level, and the Prometheus
// override must be merged INTO the include as a later entry of its path list,
// mounting this directory's prometheus.yml (relative to the first file's
// directory, deploy/observability) over the inherited config.
func validateComposeIncludeShape(root string) error {
	var shape composeIncludeShape
	if err := readYAML(filepath.Join(root, "docker-compose.yml"), &shape); err != nil {
		return err
	}
	for _, service := range []string{"prometheus", "grafana", "otel-collector", "tempo"} {
		if _, ok := shape.Services[service]; ok {
			return fmt.Errorf("compose declares included service %q at top level; Compose v2.24+ rejects that (\"conflicts with imported resource\") — override it inside the include's path list instead", service)
		}
	}
	found := false
	for _, inc := range shape.Include {
		if len(inc.Path) < 2 {
			continue
		}
		if filepath.Base(inc.Path[0]) == "docker-compose.yml" && contains(inc.Path[1:], "./prometheus-override.yml") {
			found = true
		}
	}
	if !found {
		return fmt.Errorf("compose include must be a path list: the observability docker-compose.yml first, then ./prometheus-override.yml")
	}
	var override composeConfig
	if err := readYAML(filepath.Join(root, "prometheus-override.yml"), &override); err != nil {
		return err
	}
	for _, volume := range override.Services["prometheus"].Volumes {
		if strings.HasPrefix(volume, "../compose/prometheus.yml:") && strings.Contains(volume, ":/etc/prometheus/prometheus.yml") {
			return nil
		}
	}
	return fmt.Errorf("prometheus-override.yml must mount ../compose/prometheus.yml at /etc/prometheus/prometheus.yml (paths resolve against deploy/observability)")
}
