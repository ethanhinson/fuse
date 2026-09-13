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
)

// validateCompose checks the compose stack's own prometheus.yml: it scrapes
// fuse at wantTarget (a service on the compose network, not the host), it loads
// the alerts file that is still mounted from deploy/observability/, and every
// Fuse series it names is one the recorder actually registers.
func validateCompose(root, wantTarget string) error {
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
