// Package charts_test is the chart gate: `helm lint` plus a `helm template`
// matrix over the values combinations the chart's guards and pod invariants are
// supposed to enforce.
//
// It is Go-driven so `go test ./...` runs it, and it SKIPS LOUDLY rather than
// silently passing when `helm` is absent — the same posture as
// deploy/observability's compose smoke test: a missing tool must never look
// like a tested one.
//
// Run: go test ./deploy/charts/ -count=1 -v
package charts_test

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	chartDir   = "./fuse"
	alertsPath = "../observability/alerts.yml"
	release    = "chart-test"
)

// minimal is the smallest values set that satisfies BOTH mandatory guards, so
// every other case can add exactly the one thing it is about.
var minimal = []string{
	"--set", "postgres.dsn=postgres://user:pw@db:5432/fuse",
	"--set", "auth.allowDevToken=true",
}

// requireHelm skips the whole test loudly when helm is not installed. It is a
// skip and not a failure because a contributor without helm should still be
// able to run `go test ./...`; it is LOUD because a green run that tested
// nothing is worse than a red one.
func requireHelm(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("helm")
	if err != nil {
		t.Skipf("SKIPPING CHART VALIDATION — `helm` is not on PATH (%v), so NOTHING in this file was verified. "+
			"The fuse chart at deploy/charts/fuse was NOT linted and NOT rendered: its guards, its pod "+
			"invariants, and the PrometheusRule/alerts.yml equality are all unchecked in this run. "+
			"Install Helm v3 (https://helm.sh/docs/intro/install/) and re-run `go test ./deploy/charts/`. "+
			"CI installs helm, so this must not be skipped there.", err)
	}
	return path
}

// helmTemplate renders the chart. It returns stdout and the error, so callers
// can assert on a refusal as easily as on a success.
func helmTemplate(t *testing.T, extra ...string) (string, error) {
	t.Helper()
	args := append([]string{"template", release, chartDir}, extra...)
	cmd := exec.Command("helm", args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// mustTemplate renders and fails the test if helm refused.
func mustTemplate(t *testing.T, extra ...string) string {
	t.Helper()
	out, err := helmTemplate(t, extra...)
	if err != nil {
		t.Fatalf("helm template %v failed, expected success:\n%s", extra, out)
	}
	return out
}

// mustRefuse renders and fails the test if helm SUCCEEDED, returning the
// combined output so the caller can assert on the message. A guard whose text
// is not asserted is a guard that can silently become the wrong guard.
func mustRefuse(t *testing.T, extra ...string) string {
	t.Helper()
	out, err := helmTemplate(t, extra...)
	if err == nil {
		t.Fatalf("helm template %v SUCCEEDED, expected a guard to refuse:\n%s", extra, out)
	}
	return out
}

func requireContains(t *testing.T, haystack, needle, what string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Fatalf("%s: output does not mention %q:\n%s", what, needle, haystack)
	}
}

// docs parses multi-document helm output into parsed YAML documents, skipping
// the empty ones helm emits for templates that render nothing.
//
// This uses a real YAML stream decoder rather than splitting the text on
// line-initial "---": a block scalar (the config Secret's stringData holds
// one via `|`) can itself contain a line starting with "---" — from an
// extraManifests value rendered through tpl, or a future comment banner — and
// a text split would cut a document in half there, silently dropping a pod
// spec from the set assertPodInvariants walks.
func docs(t *testing.T, rendered string) []map[string]any {
	t.Helper()
	var out []map[string]any
	dec := yaml.NewDecoder(strings.NewReader(rendered))
	for {
		var doc map[string]any
		err := dec.Decode(&doc)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("rendered document is not valid YAML: %v\n%s", err, rendered)
		}
		if len(doc) == 0 {
			continue
		}
		out = append(out, doc)
	}
	if len(out) == 0 {
		t.Fatalf("no documents rendered:\n%s", rendered)
	}
	return out
}

func mapAt(v any, keys ...string) map[string]any {
	cur, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	for _, k := range keys {
		next, ok := cur[k].(map[string]any)
		if !ok {
			return nil
		}
		cur = next
	}
	return cur
}

func sliceAt(v any, keys ...string) []any {
	if len(keys) == 0 {
		s, _ := v.([]any)
		return s
	}
	parent := mapAt(v, keys[:len(keys)-1]...)
	if parent == nil {
		return nil
	}
	s, _ := parent[keys[len(keys)-1]].([]any)
	return s
}

// podSpec is one rendered pod template, named well enough for a failure to say
// WHICH pod broke an invariant.
type podSpec struct {
	desc      string
	component string
	spec      map[string]any
	meta      map[string]any
}

// podSpecs walks every rendered document for a pod template, rather than
// grepping the rendered text. This is the point: a Pod-bearing kind added to
// the chart later is picked up here automatically and must satisfy the same
// invariants, where a string grep over the whole output would have passed as
// long as SOME pod had the string.
func podSpecs(t *testing.T, rendered string) []podSpec {
	t.Helper()
	var pods []podSpec
	for _, doc := range docs(t, rendered) {
		kind, _ := doc["kind"].(string)
		name, _ := mapAt(doc, "metadata")["name"].(string)

		var tmpl map[string]any
		switch kind {
		case "Deployment", "StatefulSet", "DaemonSet", "ReplicaSet", "Job":
			tmpl = mapAt(doc, "spec", "template")
		case "CronJob":
			tmpl = mapAt(doc, "spec", "jobTemplate", "spec", "template")
		case "Pod":
			tmpl = doc
		default:
			continue
		}
		if tmpl == nil {
			t.Fatalf("%s/%s: expected a pod template, found none", kind, name)
		}
		meta := mapAt(tmpl, "metadata")
		if meta == nil {
			meta = map[string]any{}
		}
		component, _ := mapAt(meta, "labels")["app.kubernetes.io/component"].(string)
		spec := mapAt(tmpl, "spec")
		if spec == nil {
			t.Fatalf("%s/%s: pod template has no spec", kind, name)
		}
		pods = append(pods, podSpec{
			desc:      fmt.Sprintf("%s/%s", kind, name),
			component: component,
			spec:      spec,
			meta:      meta,
		})
	}
	if len(pods) == 0 {
		t.Fatalf("no pod specs found in rendered output:\n%s", rendered)
	}
	return pods
}

// assertPodInvariants holds for EVERY pod the chart renders, server or dev
// Postgres: both probes, readOnlyRootFilesystem, the config checksum
// annotation that rolls the pod, and the writable /tmp that makes
// readOnlyRootFilesystem safe rather than merely quiet.
func assertPodInvariants(t *testing.T, pods []podSpec) {
	t.Helper()
	for _, p := range pods {
		annotations := mapAt(p.meta, "annotations")
		if sum, _ := annotations["checksum/config"].(string); sum == "" {
			t.Errorf("%s: missing the checksum/config pod annotation — a config change would not roll this pod", p.desc)
		}

		containers := sliceAt(p.spec, "containers")
		if len(containers) == 0 {
			t.Errorf("%s: no containers", p.desc)
			continue
		}

		volumeIsEmptyDir := map[string]bool{}
		for _, v := range sliceAt(p.spec, "volumes") {
			vol, _ := v.(map[string]any)
			name, _ := vol["name"].(string)
			_, isEmpty := vol["emptyDir"]
			volumeIsEmptyDir[name] = isEmpty
		}

		for i, c := range containers {
			ctr, ok := c.(map[string]any)
			if !ok {
				t.Errorf("%s: container %d is not a mapping", p.desc, i)
				continue
			}
			cname, _ := ctr["name"].(string)
			where := fmt.Sprintf("%s container %q", p.desc, cname)

			for _, probe := range []string{"livenessProbe", "readinessProbe"} {
				if mapAt(ctr, probe) == nil {
					t.Errorf("%s: missing %s", where, probe)
				}
			}

			sc := mapAt(ctr, "securityContext")
			if ro, _ := sc["readOnlyRootFilesystem"].(bool); !ro {
				t.Errorf("%s: readOnlyRootFilesystem is not true", where)
			}

			var tmpVolume string
			for _, m := range sliceAt(ctr, "volumeMounts") {
				mount, _ := m.(map[string]any)
				if path, _ := mount["mountPath"].(string); path == "/tmp" {
					tmpVolume, _ = mount["name"].(string)
				}
			}
			if tmpVolume == "" {
				t.Errorf("%s: no /tmp mount. readOnlyRootFilesystem with no writable /tmp silently degrades "+
					"the sandbox egress proxy to deny-all while the pod stays Ready", where)
				continue
			}
			if !volumeIsEmptyDir[tmpVolume] {
				t.Errorf("%s: /tmp is backed by volume %q, which is not an emptyDir", where, tmpVolume)
			}
		}
	}
}

func TestHelmLint(t *testing.T) {
	requireHelm(t)
	args := append([]string{"lint", chartDir}, minimal...)
	out, err := exec.Command("helm", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("helm lint failed: %v\n%s", err, out)
	}
	if strings.Contains(string(out), "[ERROR]") {
		t.Fatalf("helm lint reported errors:\n%s", out)
	}
}

func TestGuardRefusesWithNoAuth(t *testing.T) {
	requireHelm(t)
	out := mustRefuse(t, "--set", "postgres.dsn=postgres://user:pw@db:5432/fuse")
	requireContains(t, out, "no authentication configured", "auth guard")
}

func TestGuardRefusesWithNoDSN(t *testing.T) {
	requireHelm(t)
	out := mustRefuse(t, "--set", "auth.allowDevToken=true")
	requireContains(t, out, "no Postgres DSN configured", "dsn guard")
}

func TestDevPostgresSatisfiesDSNGuard(t *testing.T) {
	requireHelm(t)
	out := mustTemplate(t, "--set", "auth.allowDevToken=true", "--set", "postgres.dev.enabled=true")

	var sawStatefulSet bool
	for _, doc := range docs(t, out) {
		if kind, _ := doc["kind"].(string); kind == "StatefulSet" {
			sawStatefulSet = true
		}
	}
	if !sawStatefulSet {
		t.Fatalf("postgres.dev.enabled=true rendered no StatefulSet:\n%s", out)
	}

	pods := podSpecs(t, out)
	if len(pods) < 2 {
		t.Fatalf("expected the server pod AND the dev-Postgres pod, got %d", len(pods))
	}
	assertPodInvariants(t, pods)
}

func TestDockerSocketRequiresAcknowledgement(t *testing.T) {
	requireHelm(t)
	args := append(append([]string{}, minimal...), "--set", "sandbox.mode=docker-socket")
	out := mustRefuse(t, args...)
	requireContains(t, out, "acknowledgeHostRoot", "docker-socket guard")
}

func TestDockerSocketAcknowledgedRendersHostPath(t *testing.T) {
	requireHelm(t)
	args := append(append([]string{}, minimal...),
		"--set", "sandbox.mode=docker-socket",
		"--set", "sandbox.dockerSocket.acknowledgeHostRoot=true")
	out := mustTemplate(t, args...)

	var sawVolume, sawMount bool
	for _, p := range podSpecs(t, out) {
		for _, v := range sliceAt(p.spec, "volumes") {
			vol, _ := v.(map[string]any)
			if hp := mapAt(vol, "hostPath"); hp != nil {
				if path, _ := hp["path"].(string); path == "/var/run/docker.sock" {
					sawVolume = true
				}
			}
		}
		for _, c := range sliceAt(p.spec, "containers") {
			ctr, _ := c.(map[string]any)
			for _, m := range sliceAt(ctr, "volumeMounts") {
				mount, _ := m.(map[string]any)
				if path, _ := mount["mountPath"].(string); path == "/var/run/docker.sock" {
					sawMount = true
				}
			}
		}
	}
	if !sawVolume {
		t.Errorf("acknowledged docker-socket rendered no hostPath volume for /var/run/docker.sock:\n%s", out)
	}
	if !sawMount {
		t.Errorf("acknowledged docker-socket rendered no container mount at /var/run/docker.sock:\n%s", out)
	}
	assertPodInvariants(t, podSpecs(t, out))
}

// The kubernetes mode asserts the REFUSAL ONLY, and asserts it does not read as
// a version comparison: "0.0.0-dev is older than ”" would look like a
// misconfiguration an operator could fix by bumping a value, and they cannot —
// the handler is not in the image. The message must name change #75.
func TestKubernetesSandboxRefusesNaming75(t *testing.T) {
	requireHelm(t)
	args := append(append([]string{}, minimal...), "--set", "sandbox.mode=kubernetes")
	out := mustRefuse(t, args...)
	requireContains(t, out, "not yet available", "kubernetes refusal")
	requireContains(t, out, "#75", "kubernetes refusal")
}

func TestDefaultRenderPodInvariants(t *testing.T) {
	requireHelm(t)
	assertPodInvariants(t, podSpecs(t, mustTemplate(t, minimal...)))
}

// The server pod specifically: liveness on /healthz and readiness on /readyz,
// over HTTP. The dev-Postgres pod uses exec/pg_isready, so this assertion is
// scoped by the component label rather than applied to every pod.
func TestServerPodHTTPProbePaths(t *testing.T) {
	requireHelm(t)
	out := mustTemplate(t, append(append([]string{}, minimal...), "--set", "postgres.dev.enabled=true")...)

	var checked int
	for _, p := range podSpecs(t, out) {
		if p.component == "postgres-dev" {
			continue
		}
		for _, c := range sliceAt(p.spec, "containers") {
			ctr, _ := c.(map[string]any)
			for probe, want := range map[string]string{"livenessProbe": "/healthz", "readinessProbe": "/readyz"} {
				got, _ := mapAt(ctr, probe, "httpGet")["path"].(string)
				if got != want {
					t.Errorf("%s: %s httpGet path = %q, want %q", p.desc, probe, got, want)
				}
			}
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("no server containers found to check probe paths on")
	}
}

// terminationGracePeriodSeconds = drainTimeout seconds + 10, so the kubelet's
// SIGKILL always lands AFTER the drain budget rather than inside it. Asserted
// at the default and at a non-default value, because a hardcoded 30 would pass
// the default case.
func TestTerminationGracePeriodTracksDrainTimeout(t *testing.T) {
	requireHelm(t)
	for _, tc := range []struct {
		drain string
		want  int
	}{
		{"20s", 30},
		{"45s", 55},
	} {
		out := mustTemplate(t, append(append([]string{}, minimal...), "--set", "server.drainTimeout="+tc.drain)...)
		var found bool
		for _, p := range podSpecs(t, out) {
			if p.component == "postgres-dev" {
				continue
			}
			got, ok := p.spec["terminationGracePeriodSeconds"].(int)
			if !ok {
				t.Errorf("%s: terminationGracePeriodSeconds absent or not an int: %#v", p.desc, p.spec["terminationGracePeriodSeconds"])
				continue
			}
			if got != tc.want {
				t.Errorf("%s: drainTimeout %s ⇒ terminationGracePeriodSeconds %d, want %d", p.desc, tc.drain, got, tc.want)
			}
			found = true
		}
		if !found {
			t.Errorf("drainTimeout %s: no server pod found", tc.drain)
		}
	}
}

func TestServiceMonitorAndPrometheusRuleRender(t *testing.T) {
	requireHelm(t)
	out := mustTemplate(t, append(append([]string{}, minimal...),
		"--set", "metrics.serviceMonitor.enabled=true",
		"--set", "metrics.prometheusRule.enabled=true")...)

	kinds := map[string]bool{}
	for _, doc := range docs(t, out) {
		kind, _ := doc["kind"].(string)
		kinds[kind] = true
	}
	for _, want := range []string{"ServiceMonitor", "PrometheusRule"} {
		if !kinds[want] {
			t.Errorf("metrics CRDs enabled but no %s rendered:\n%s", want, out)
		}
	}
}

// The rendered PrometheusRule's groups must EQUAL the source of truth's. The
// comparison is structural — both sides are parsed and the `groups` trees
// compared — because the in-chart copy carries a provenance header and the
// rendered copy is re-indented under `spec:`, so a string compare would fail on
// formatting while letting a real rule change through in the other direction.
func TestPrometheusRuleGroupsEqualObservabilityAlerts(t *testing.T) {
	requireHelm(t)

	source := struct {
		Groups any `yaml:"groups"`
	}{}
	raw, err := os.ReadFile(filepath.Clean(alertsPath))
	if err != nil {
		t.Fatalf("reading %s: %v", alertsPath, err)
	}
	if err := yaml.Unmarshal(raw, &source); err != nil {
		t.Fatalf("parsing %s: %v", alertsPath, err)
	}
	if source.Groups == nil {
		t.Fatalf("%s parsed with no groups", alertsPath)
	}

	out := mustTemplate(t, append(append([]string{}, minimal...), "--set", "metrics.prometheusRule.enabled=true")...)

	var rendered any
	for _, doc := range docs(t, out) {
		if kind, _ := doc["kind"].(string); kind != "PrometheusRule" {
			continue
		}
		rendered = mapAt(doc, "spec")["groups"]
	}
	if rendered == nil {
		t.Fatalf("no PrometheusRule groups rendered:\n%s", out)
	}

	if !reflect.DeepEqual(rendered, source.Groups) {
		gotYAML, _ := yaml.Marshal(rendered)
		wantYAML, _ := yaml.Marshal(source.Groups)
		t.Fatalf("the chart's alerts have drifted from %s (the source of truth).\nRun `make charts-sync-alerts` to regenerate deploy/charts/fuse/alerts.yml.\n--- rendered ---\n%s\n--- %s ---\n%s",
			alertsPath, gotYAML, alertsPath, wantYAML)
	}
}

// labelsSubsetOf implements the real Kubernetes label-selector semantics: a
// matchLabels selector matches a pod when EVERY key/value in the selector is
// present and equal in the pod's labels. Subset, not equality — which is
// precisely the trap this test exists to catch. A pod that merely ADDS a
// component label still satisfies a selector that omits it.
func labelsSubsetOf(selector, labels map[string]any) bool {
	if len(selector) == 0 {
		return false
	}
	for k, want := range selector {
		got, ok := labels[k]
		if !ok || fmt.Sprint(got) != fmt.Sprint(want) {
			return false
		}
	}
	return true
}

// The server's Service, Deployment, PDB and NetworkPolicy selectors must NOT
// match the dev-Postgres pod.
//
// They used to. The server's selector was the bare name+instance pair and the
// Postgres pod carried those two labels plus a component label, so — label
// selectors matching on SUBSET — every server selector matched the database
// pod: Service/…-fuse load-balanced Connect traffic onto Postgres, the server's
// ReplicaSet counted it toward `replicas`, the deny-all-egress NetworkPolicy
// applied to the database, and the PDB budgeted the wrong pod set.
//
// postgres.dev.enabled=true is the path `make helm-smoke` and the chart tests
// take, so this must stay asserted.
func TestServerSelectorsDoNotMatchDevPostgresPod(t *testing.T) {
	requireHelm(t)
	out := mustTemplate(t,
		"--set", "auth.allowDevToken=true",
		"--set", "postgres.dev.enabled=true",
		"--set", "podDisruptionBudget.enabled=true",
		"--set", "podDisruptionBudget.minAvailable=1",
		"--set", "networkPolicy.enabled=true")

	// The dev-Postgres pod template's labels, from the StatefulSet.
	var pgLabels map[string]any
	var serverLabels map[string]any
	for _, doc := range docs(t, out) {
		kind, _ := doc["kind"].(string)
		switch kind {
		case "StatefulSet":
			pgLabels = mapAt(doc, "spec", "template", "metadata", "labels")
		case "Deployment":
			serverLabels = mapAt(doc, "spec", "template", "metadata", "labels")
		}
	}
	if pgLabels == nil {
		t.Fatalf("no dev-Postgres StatefulSet pod labels rendered:\n%s", out)
	}
	if serverLabels == nil {
		t.Fatalf("no server Deployment pod labels rendered:\n%s", out)
	}
	if c, _ := pgLabels["app.kubernetes.io/component"].(string); c != "postgres-dev" {
		t.Fatalf("StatefulSet pod labels are not the dev-Postgres pod's: %#v", pgLabels)
	}

	// Every server-owned selector, by the path it lives at.
	type sel struct {
		kind string
		path []string
	}
	selectors := []sel{
		{"Deployment", []string{"spec", "selector", "matchLabels"}},
		{"Service", []string{"spec", "selector"}},
		{"PodDisruptionBudget", []string{"spec", "selector", "matchLabels"}},
		{"NetworkPolicy", []string{"spec", "podSelector", "matchLabels"}},
	}
	pgName := ""
	for _, doc := range docs(t, out) {
		if kind, _ := doc["kind"].(string); kind == "StatefulSet" {
			pgName, _ = mapAt(doc, "metadata")["name"].(string)
		}
	}

	var checked int
	for _, doc := range docs(t, out) {
		kind, _ := doc["kind"].(string)
		name, _ := mapAt(doc, "metadata")["name"].(string)
		// postgres-dev owns its own Service, which SHOULD select its own pod.
		if strings.HasPrefix(name, pgName) {
			continue
		}
		for _, s := range selectors {
			if kind != s.kind {
				continue
			}
			ml := mapAt(doc, s.path...)
			if ml == nil {
				t.Fatalf("%s/%s: no selector at %v:\n%s", kind, name, s.path, out)
			}
			checked++
			if labelsSubsetOf(ml, pgLabels) {
				t.Errorf("%s/%s selector %#v MATCHES the dev-Postgres pod's labels %#v — "+
					"Kubernetes selectors match on subset, so this server object is pointed at the database pod. "+
					"Narrow the selector with app.kubernetes.io/component: server.", kind, name, ml, pgLabels)
			}
			if !labelsSubsetOf(ml, serverLabels) {
				t.Errorf("%s/%s selector %#v does NOT match the server pod's own labels %#v — the object selects nothing",
					kind, name, ml, serverLabels)
			}
		}
	}
	if checked != len(selectors) {
		t.Fatalf("checked %d server selectors, want %d — a template stopped rendering:\n%s", checked, len(selectors), out)
	}
}

// config.existingSecret and auth.tokens together must REFUSE, for the same
// reason secret-dsn.yaml refuses postgres.dsn + postgres.existingSecret: with
// both, config.existingSecret wins and secret-config.yaml renders nothing, so
// the tokens never reach the server. Before this guard existed the render
// SUCCEEDED and `real-token` appeared nowhere in the output — an operator's
// bearer tokens silently discarded while the pod authenticated on whatever the
// external Secret held.
func TestExistingConfigSecretWithAuthTokensRefuses(t *testing.T) {
	requireHelm(t)
	out := mustRefuse(t,
		"--set", "postgres.dsn=postgres://user:pw@db:5432/fuse",
		"--set", "config.existingSecret=my-external-config",
		"--set", "auth.tokens[0].token=real-token",
		"--set", "auth.tokens[0].tenant=_default",
	)
	requireContains(t, out, "config.existingSecret", "config/auth guard")
	requireContains(t, out, "auth.tokens", "config/auth guard")
}

// The counterpart: config.existingSecret with auth.allowDevToken=true is NOT
// guarded and must keep rendering. allowDevToken supplies no content of its own
// — it is the flag that OMITS loop_server.auth so the server synthesizes its
// dev token — so nothing is discarded, and if the external Secret carries no
// auth the server does exactly what the flag asked for.
func TestExistingConfigSecretWithAllowDevTokenRenders(t *testing.T) {
	requireHelm(t)
	mustTemplate(t,
		"--set", "postgres.dsn=postgres://user:pw@db:5432/fuse",
		"--set", "config.existingSecret=my-external-config",
		"--set", "auth.allowDevToken=true")
}

// chartAlertsPath is the in-chart COPY of the alert rules, generated by
// `make charts-sync-alerts` (scripts/charts-sync-alerts.sh).
//
//go:generate sh -c "cd ../.. && make charts-sync-alerts"
const chartAlertsPath = "./fuse/alerts.yml"

// alertGroups parses one alerts YAML file and returns its `groups` tree.
func alertGroups(t *testing.T, path string) any {
	t.Helper()
	doc := struct {
		Groups any `yaml:"groups"`
	}{}
	raw, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	if doc.Groups == nil {
		t.Fatalf("%s parsed with no groups", path)
	}
	return doc.Groups
}

// The in-chart copy of the alerts must equal the source of truth. This is the
// UNSKIPPABLE half of the drift gate: it parses both files directly, so it runs
// on a machine without helm, where TestPrometheusRuleGroupsEqualObservabilityAlerts
// (which proves the RENDERED output matches — a different assertion) is skipped.
func TestChartAlertsCopyEqualsObservabilityAlerts(t *testing.T) {
	want := alertGroups(t, alertsPath)
	got := alertGroups(t, chartAlertsPath)

	if !reflect.DeepEqual(got, want) {
		gotYAML, _ := yaml.Marshal(got)
		wantYAML, _ := yaml.Marshal(want)
		t.Fatalf("%s has drifted from %s (the source of truth).\nRun `make charts-sync-alerts` to regenerate it.\n--- %s ---\n%s\n--- %s ---\n%s",
			chartAlertsPath, alertsPath, chartAlertsPath, gotYAML, alertsPath, wantYAML)
	}
}
