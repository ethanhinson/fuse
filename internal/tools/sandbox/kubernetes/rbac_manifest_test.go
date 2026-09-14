package kubernetes

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// THE MANIFEST/CODE CONTRACT.
//
// The shipped ClusterRoles in deploy/k8s/sandbox-rbac.yaml are the ONLY authority
// a real deployment runs under, and the kind lane drives an admin kubeconfig — so
// nothing else in this package can observe a verb the code needs and the manifest
// omits. That gap is silent by construction: heartbeatLoop drops its Patch error
// and adoptSecretBestEffort is best-effort, so a missing verb surfaces as a live
// sandbox being reaped or a private key leaking, never as a failing call.
//
// requiredGrants is therefore maintained by hand ALONGSIDE the substrate's API
// call sites: every entry below names the call that justifies it. Adding an API
// call to this package means adding its (resource, verb) here, and the test then
// fails until the manifest grants it. That is the direction that matters — it
// fails when a needed verb is ABSENT, not merely when the file changes.
//
// The reverse direction is deliberately NOT asserted as an error: a verb granted
// beyond this set is reported as an informational finding (below) rather than a
// failure, because an operator-motivated grant is not a defect and narrowing is a
// judgement call, not a mechanical one.
var requiredGrants = []struct {
	apiGroup string
	resource string
	verb     string
	why      string
}{
	// Namespaces — cluster-scoped, in fuse-sandbox-namespaces.
	{"", "namespaces", "get", "assertNamespace: Namespaces().Get"},
	{"", "namespaces", "create", "assertNamespace: Namespaces().Create"},
	{"", "namespaces", "list", "Reap: Namespaces().List over label fuse.dev/managed"},

	// Pods.
	{"", "pods", "get", "waitReady: Pods(ns).Get"},
	{"", "pods", "list", "Reap: Pods(ns).List"},
	{"", "pods", "create", "Provision: Pods(ns).Create"},
	{"", "pods", "delete", "Teardown / Reap / deletePodBestEffort: Pods(ns).Delete"},
	{"", "pods", "patch", "sandboxPod.Heartbeat: Pods(ns).Patch (MergePatchType on the idle annotation)"},

	// Exec is the bash tool's datapath: websocket executor issues GET, the SPDY
	// fallback issues POST.
	{"", "pods/exec", "create", "Exec: SPDY executor POSTs the exec subresource"},
	{"", "pods/exec", "get", "Exec: websocket executor GETs the exec subresource"},

	// Secrets — the per-Pod fuse-egress-tls-* objects.
	{"", "secrets", "get", "adoptSecretBestEffort: Secrets(ns).Get"},
	{"", "secrets", "create", "Provision: Secrets(ns).Create"},
	{"", "secrets", "delete", "provision rollback: Secrets(ns).Delete"},
	{"", "secrets", "update", "adoptSecretBestEffort: Secrets(ns).Update sets the ownerReference the GC relies on"},

	{"", "serviceaccounts", "create", "assertServiceAccount: ServiceAccounts(ns).Create"},

	// Services — Verify reads the API server's ClusterIP for the canary target.
	{"", "services", "get", "canaryTarget: Services(\"default\").Get"},

	{"", "resourcequotas", "get", "assertQuota: ResourceQuotas(ns).Get"},
	{"", "resourcequotas", "create", "assertQuota: ResourceQuotas(ns).Create"},
	{"", "resourcequotas", "update", "assertQuota: ResourceQuotas(ns).Update"},

	{"networking.k8s.io", "networkpolicies", "get", "applyPolicy: NetworkPolicies(ns).Get"},
	{"networking.k8s.io", "networkpolicies", "create", "applyPolicy: NetworkPolicies(ns).Create"},
	{"networking.k8s.io", "networkpolicies", "update", "applyPolicy: NetworkPolicies(ns).Update"},
	{"networking.k8s.io", "networkpolicies", "delete", "Teardown: NetworkPolicies(ns).Delete"},
}

// rbacManifestPath walks up from this package to the repository root.
func rbacManifestPath(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 8; i++ {
		candidate := filepath.Join(dir, "deploy", "k8s", "sandbox-rbac.yaml")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("could not locate deploy/k8s/sandbox-rbac.yaml above the package directory")
	return ""
}

type rbacRule struct {
	APIGroups []string `yaml:"apiGroups"`
	Resources []string `yaml:"resources"`
	Verbs     []string `yaml:"verbs"`
}

type rbacDoc struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Rules    []rbacRule            `yaml:"rules"`
	RoleRef  struct{ Name string } `yaml:"roleRef"`
	Subjects []struct {
		Kind      string `yaml:"kind"`
		Name      string `yaml:"name"`
		Namespace string `yaml:"namespace"`
	} `yaml:"subjects"`
}

// loadManifest parses every YAML document and returns the granted
// (apiGroup, resource, verb) set, restricted to ClusterRoles actually bound to
// the `fuse` ServiceAccount — an unbound rule grants nothing.
func loadManifest(t *testing.T) (granted map[string]bool, roleNames []string) {
	t.Helper()
	raw, err := os.ReadFile(rbacManifestPath(t))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}

	var docs []rbacDoc
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	for {
		var d rbacDoc
		err := dec.Decode(&d)
		if err != nil {
			break
		}
		docs = append(docs, d)
	}
	if len(docs) == 0 {
		t.Fatal("manifest parsed to zero documents")
	}

	bound := map[string]bool{}
	for _, d := range docs {
		if d.Kind != "ClusterRoleBinding" {
			continue
		}
		for _, s := range d.Subjects {
			if s.Kind == "ServiceAccount" && s.Name == "fuse" {
				bound[d.RoleRef.Name] = true
			}
		}
	}
	if len(bound) == 0 {
		t.Fatal("no ClusterRoleBinding binds the `fuse` ServiceAccount, so the substrate has no verbs at all")
	}

	granted = map[string]bool{}
	for _, d := range docs {
		if d.Kind != "ClusterRole" || !bound[d.Metadata.Name] {
			continue
		}
		roleNames = append(roleNames, d.Metadata.Name)
		for _, r := range d.Rules {
			for _, g := range r.APIGroups {
				for _, res := range r.Resources {
					for _, v := range r.Verbs {
						granted[g+"/"+res+":"+v] = true
					}
				}
			}
		}
	}
	sort.Strings(roleNames)
	return granted, roleNames
}

// TestRBACManifestGrantsEveryVerbTheSubstrateUses fails when the shipped role
// omits a verb one of the substrate's call sites needs.
func TestRBACManifestGrantsEveryVerbTheSubstrateUses(t *testing.T) {
	granted, roles := loadManifest(t)
	if len(roles) == 0 {
		t.Fatal("no bound ClusterRole found in the manifest")
	}

	var missing []string
	for _, g := range requiredGrants {
		key := g.apiGroup + "/" + g.resource + ":" + g.verb
		if !granted[key] {
			group := g.apiGroup
			if group == "" {
				group = "core"
			}
			missing = append(missing, group+" "+g.resource+" `"+g.verb+"` — needed by "+g.why)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("deploy/k8s/sandbox-rbac.yaml does not grant %d verb(s) the substrate calls; "+
			"every one of these returns Forbidden in a real deployment:\n  - %s",
			len(missing), strings.Join(missing, "\n  - "))
	}
}

// TestRBACManifestReportsVerbsNoCallSiteJustifies is informational: a grant with
// no call site behind it is a candidate for removal, but not a build failure.
func TestRBACManifestReportsVerbsNoCallSiteJustifies(t *testing.T) {
	granted, _ := loadManifest(t)
	required := map[string]bool{}
	for _, g := range requiredGrants {
		required[g.apiGroup+"/"+g.resource+":"+g.verb] = true
	}
	var extra []string
	for key := range granted {
		if !required[key] {
			extra = append(extra, key)
		}
	}
	sort.Strings(extra)
	if len(extra) > 0 {
		t.Logf("manifest grants %d verb(s) no call site in this package justifies (not a failure): %s",
			len(extra), strings.Join(extra, ", "))
	}
}

// TestSandboxServiceAccountIsBoundToNothing guards ADR-0058 rule 6.
func TestSandboxServiceAccountIsBoundToNothing(t *testing.T) {
	raw, err := os.ReadFile(rbacManifestPath(t))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	for {
		var d rbacDoc
		if err := dec.Decode(&d); err != nil {
			break
		}
		if d.Kind != "ClusterRoleBinding" && d.Kind != "RoleBinding" {
			continue
		}
		for _, s := range d.Subjects {
			if s.Name == "fuse-sandbox" {
				t.Fatalf("%s %q binds the fuse-sandbox ServiceAccount; a sandbox identity must hold NO verbs (ADR-0058 rule 6)", d.Kind, d.Metadata.Name)
			}
		}
	}
}
