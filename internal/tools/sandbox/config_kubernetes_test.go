package sandbox

import (
	"strings"
	"testing"
	"time"
)

// fullKubernetesBlock exercises every key in the schema at once, so the
// per-field assertions below are made against a file an operator could
// plausibly write rather than against one field in isolation.
const fullKubernetesBlock = `
handler: kubernetes
kubernetes:
  kubeconfig: /home/op/.kube/config
  context: kind-fuse
  namespace_prefix: fuse-sb
  image: alpine:3.20
  sidecar_image: ghcr.io/example/fuse:v1
  runtime_class: gvisor
  service_account: fuse-sandbox
  startup_timeout: 90s
  pod_max_lifetime: 2h
  proxy:
    listen: 0.0.0.0:3129
    advertise_address: 10.1.2.3
  tenant_quota:
    max_pods: 12
  workspace:
    size_limit: 4g
`

// handler: kubernetes is the third closed enum value, and it is CONTAINED — the
// invariant Contained == (Handler != HandlerHost) holds for it exactly as it
// does for the container handler.
func TestLoadConfigHandlerKubernetesIsContained(t *testing.T) {
	// Case- and space-tolerance, matching the other two values. Matching is
	// tolerant but otherwise exact: there is no prefix matching and no aliasing.
	for _, spelling := range []string{"kubernetes", "KUBERNETES", "  Kubernetes  ", "\"kubernetes\""} {
		t.Run(spelling, func(t *testing.T) {
			root := t.TempDir()
			writeConfigFile(t, root, "handler: "+spelling+"\n")

			cfg, warns := LoadConfig(root)

			if len(warns) != 0 {
				t.Fatalf("warnings = %v, want none for a recognised handler", warns)
			}
			if cfg.Handler != HandlerKubernetes {
				t.Fatalf("Handler = %q, want %q", cfg.Handler, HandlerKubernetes)
			}
			if !cfg.Contained {
				t.Error("Contained = false for the kubernetes handler, want true")
			}
			assertConsistent(t, cfg)
		})
	}
}

// Every neighbouring spelling must still be unrecognised. `kubernetes` becoming
// a value must not open prefix or fuzzy matching, because that is how a typo
// becomes a substrate nobody chose.
func TestLoadConfigHandlerNearMissesStillUnknown(t *testing.T) {
	for _, spelling := range []string{"k8s", "kube", "kubernetes-sandbox", "kuberentes", "kubernete"} {
		t.Run(spelling, func(t *testing.T) {
			root := t.TempDir()
			writeConfigFile(t, root, "handler: "+spelling+"\n")

			cfg, warns := LoadConfig(root)

			assertContained(t, cfg)
			if !hasWarning(warns, WarnUnknownHandler) {
				t.Fatalf("warnings = %v, want a %q warning", warns, WarnUnknownHandler)
			}
		})
	}
}

// The unknown-handler diagnostic must NAME the kubernetes value now that it
// exists: an operator who typed "k8s" and is told the valid values are
// "container" and "host" would conclude the substrate is not available at all.
func TestUnknownHandlerDetailNamesEveryValidHandler(t *testing.T) {
	root := t.TempDir()
	writeConfigFile(t, root, "handler: k8s\n")

	_, warns := LoadConfig(root)

	var detail string
	for _, w := range warns {
		if w.Reason == WarnUnknownHandler {
			detail = w.Detail
		}
	}
	if detail == "" {
		t.Fatalf("warnings = %v, want a %q warning", warns, WarnUnknownHandler)
	}
	for _, want := range []string{HandlerContainer, HandlerHost, HandlerKubernetes} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail = %q, want it to name %q", detail, want)
		}
	}
}

// contained: false disagreeing with handler: kubernetes is the same
// contradiction handler: container produces — the explicit handler wins and the
// contradiction is REPORTED, never silently resolved toward the uncontained
// side.
func TestLoadConfigKubernetesWithContainedFalseIsContainedAndWarns(t *testing.T) {
	root := t.TempDir()
	writeConfigFile(t, root, "contained: false\nhandler: kubernetes\n")

	cfg, warns := LoadConfig(root)

	if cfg.Handler != HandlerKubernetes {
		t.Fatalf("Handler = %q, want %q — the explicit handler wins", cfg.Handler, HandlerKubernetes)
	}
	if !cfg.Contained {
		t.Error("Contained = false, want true: contained: false must not defeat a contained handler")
	}
	if !hasWarning(warns, WarnContradictory) {
		t.Errorf("warnings = %v, want a %q warning", warns, WarnContradictory)
	}
	assertConsistent(t, cfg)
}

// --- the block's fields ------------------------------------------------------

// Every key in spec §6 parses, and each resolved field carries its own presence
// so absent is distinguishable from an explicit zero.
func TestLoadConfigKubernetesBlockParsesEveryKey(t *testing.T) {
	root := t.TempDir()
	writeConfigFile(t, root, fullKubernetesBlock)

	cfg, warns := LoadConfig(root)

	if len(warns) != 0 {
		t.Fatalf("warnings = %v, want none for a well-formed block", warns)
	}
	k := cfg.Kubernetes
	if k.Refused {
		t.Fatal("Refused = true for a well-formed block")
	}

	str := func(name string, got *string, want string) {
		t.Helper()
		if got == nil {
			t.Errorf("%s = nil, want %q", name, want)
			return
		}
		if *got != want {
			t.Errorf("%s = %q, want %q", name, *got, want)
		}
	}
	str("kubeconfig", k.Kubeconfig, "/home/op/.kube/config")
	str("context", k.Context, "kind-fuse")
	str("namespace_prefix", k.NamespacePrefix, "fuse-sb")
	str("image", k.Image, "alpine:3.20")
	str("sidecar_image", k.SidecarImage, "ghcr.io/example/fuse:v1")
	str("runtime_class", k.RuntimeClass, "gvisor")
	str("service_account", k.ServiceAccount, "fuse-sandbox")
	str("proxy.listen", k.ProxyListen, "0.0.0.0:3129")
	str("proxy.advertise_address", k.ProxyAdvertiseAddress, "10.1.2.3")

	if k.StartupTimeout == nil || *k.StartupTimeout != 90*time.Second {
		t.Errorf("startup_timeout = %v, want 90s", k.StartupTimeout)
	}
	if k.PodMaxLifetime == nil || *k.PodMaxLifetime != 2*time.Hour {
		t.Errorf("pod_max_lifetime = %v, want 2h", k.PodMaxLifetime)
	}
	if k.TenantQuotaMaxPods == nil || *k.TenantQuotaMaxPods != 12 {
		t.Errorf("tenant_quota.max_pods = %v, want 12", k.TenantQuotaMaxPods)
	}
	if k.WorkspaceSizeLimitBytes == nil || *k.WorkspaceSizeLimitBytes != 4<<30 {
		t.Errorf("workspace.size_limit = %v, want %d", k.WorkspaceSizeLimitBytes, 4<<30)
	}
}

// An absent block leaves every field unset, so the substrate's own defaults (not
// a zero value) decide the outcome — the same discipline limits: follows.
func TestLoadConfigNoKubernetesBlockLeavesEverythingUnset(t *testing.T) {
	root := t.TempDir()
	writeConfigFile(t, root, "handler: kubernetes\n")

	cfg, warns := LoadConfig(root)

	if len(warns) != 0 {
		t.Fatalf("warnings = %v, want none", warns)
	}
	if cfg.Kubernetes != (Kubernetes{}) {
		t.Fatalf("Kubernetes = %+v, want the zero value (all unset)", cfg.Kubernetes)
	}
	if cfg.Kubernetes.Refused {
		t.Error("Refused = true for an ABSENT block; absence is not a refusal")
	}
}

// The block is honoured for its own sake, not only under handler: kubernetes. An
// operator who configures the block and then flips the handler back must not
// have their configuration silently dropped — being ignored is what sends people
// looking for an env-var opt-out that must never exist.
func TestLoadConfigKubernetesBlockParsesUnderAnyHandler(t *testing.T) {
	root := t.TempDir()
	writeConfigFile(t, root, "handler: container\nkubernetes:\n  namespace_prefix: fuse-sb\n")

	cfg, warns := LoadConfig(root)

	if len(warns) != 0 {
		t.Fatalf("warnings = %v, want none", warns)
	}
	if cfg.Handler != HandlerContainer {
		t.Fatalf("Handler = %q, want %q", cfg.Handler, HandlerContainer)
	}
	if cfg.Kubernetes.NamespacePrefix == nil || *cfg.Kubernetes.NamespacePrefix != "fuse-sb" {
		t.Errorf("namespace_prefix = %v, want it parsed even under another handler", cfg.Kubernetes.NamespacePrefix)
	}
}

// --- the fail-safe path ------------------------------------------------------

// A MALFORMED value inside the block warns WarnBadKubernetes, DISCARDS the whole
// block, and marks it Refused.
//
// Whole-block discard rather than per-field degradation is the point: unlike
// limits:, where a bad cap degrades to a posture default that is at least as
// tight, a half-honoured kubernetes: block is a sandbox posture nobody wrote —
// the wrong namespace prefix, the wrong service account, a workspace with no
// size bound. The safe answer is to build nothing.
func TestLoadConfigMalformedKubernetesDiscardsBlockAndRefuses(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		detail string
	}{
		{
			name:   "namespace_prefix is not a DNS-1123 label",
			body:   "kubernetes:\n  namespace_prefix: Fuse_SB\n",
			detail: "namespace_prefix",
		},
		{
			name:   "namespace_prefix starts with a hyphen",
			body:   "kubernetes:\n  namespace_prefix: -fuse\n",
			detail: "namespace_prefix",
		},
		{
			name:   "namespace_prefix ends with a hyphen",
			body:   "kubernetes:\n  namespace_prefix: fuse-\n",
			detail: "namespace_prefix",
		},
		{
			name:   "namespace_prefix is too long for a label",
			body:   "kubernetes:\n  namespace_prefix: " + strings.Repeat("a", 64) + "\n",
			detail: "namespace_prefix",
		},
		{
			name:   "namespace_prefix is empty",
			body:   "kubernetes:\n  namespace_prefix: \"\"\n",
			detail: "namespace_prefix",
		},
		{
			name:   "startup_timeout is not a duration",
			body:   "kubernetes:\n  startup_timeout: soon\n",
			detail: "startup_timeout",
		},
		{
			name:   "startup_timeout is not positive",
			body:   "kubernetes:\n  startup_timeout: 0s\n",
			detail: "startup_timeout",
		},
		{
			name:   "pod_max_lifetime is not a duration",
			body:   "kubernetes:\n  pod_max_lifetime: forever\n",
			detail: "pod_max_lifetime",
		},
		{
			name:   "pod_max_lifetime is negative",
			body:   "kubernetes:\n  pod_max_lifetime: -4h\n",
			detail: "pod_max_lifetime",
		},
		{
			name:   "workspace.size_limit is not a byte size",
			body:   "kubernetes:\n  workspace:\n    size_limit: lots\n",
			detail: "workspace.size_limit",
		},
		{
			name:   "workspace.size_limit is zero",
			body:   "kubernetes:\n  workspace:\n    size_limit: 0\n",
			detail: "workspace.size_limit",
		},
		{
			name:   "tenant_quota.max_pods is negative",
			body:   "kubernetes:\n  tenant_quota:\n    max_pods: -1\n",
			detail: "tenant_quota.max_pods",
		},
		{
			name:   "proxy.listen is not host:port",
			body:   "kubernetes:\n  proxy:\n    listen: not-a-listener\n",
			detail: "proxy.listen",
		},
		{
			name:   "proxy.listen has no port",
			body:   "kubernetes:\n  proxy:\n    listen: \"0.0.0.0:\"\n",
			detail: "proxy.listen",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			// Every case carries a GOOD sibling field, so the assertion below —
			// that the block was discarded WHOLE — is meaningful rather than
			// vacuous.
			writeConfigFile(t, root, "handler: kubernetes\nimage: alpine:3.20\n"+tc.body+"  service_account: fuse-sandbox\n")

			cfg, warns := LoadConfig(root)

			if !hasWarning(warns, WarnBadKubernetes) {
				t.Fatalf("warnings = %v, want a %q warning", warns, WarnBadKubernetes)
			}
			var detail string
			for _, w := range warns {
				if w.Reason == WarnBadKubernetes {
					detail = w.Detail
				}
			}
			if !strings.Contains(detail, tc.detail) {
				t.Errorf("detail = %q, want it to name %q so the operator can find the line", detail, tc.detail)
			}

			// The whole block is gone — including the GOOD sibling. A partly
			// honoured posture is the shape this reason exists to prevent.
			if sa := cfg.Kubernetes.ServiceAccount; sa != nil {
				t.Errorf("service_account = %q survived a discarded block", *sa)
			}
			if cfg.Kubernetes != (Kubernetes{Refused: true}) {
				t.Errorf("Kubernetes = %+v, want only Refused: true", cfg.Kubernetes)
			}

			// And because the handler was NAMED, construction must refuse. This
			// is the load-bearing half: the handler stays kubernetes (so nothing
			// silently substitutes another substrate) and the Refused flag is
			// the load-time fact the factory refuses on.
			if cfg.Handler != HandlerKubernetes {
				t.Errorf("Handler = %q, want %q — a bad block must not silently reselect a substrate", cfg.Handler, HandlerKubernetes)
			}
			if !cfg.Kubernetes.Refused {
				t.Error("Refused = false after a discarded block; the named handler must be unbuildable")
			}
			// The rest of the file is untouched: this is a BLOCK-level discard,
			// not a whole-file one. The handler was understood, so there is no
			// reason to distrust the fields around it.
			if cfg.Image != "alpine:3.20" {
				t.Errorf("Image = %q, want it preserved: a bad kubernetes block is not a whole-file discard", cfg.Image)
			}
		})
	}
}

// An unknown key INSIDE the block is a file-level malformed (KnownFields), which
// discards the whole file — the pre-existing behaviour, and still the contained
// answer. Pinned so the block's addition is not mistaken for relaxing it.
func TestLoadConfigUnknownKeyInKubernetesIsWholeFileMalformed(t *testing.T) {
	root := t.TempDir()
	writeConfigFile(t, root, "handler: kubernetes\nkubernetes:\n  namespcae_prefix: fuse-sb\n")

	cfg, warns := LoadConfig(root)

	if !hasWarning(warns, WarnMalformed) {
		t.Fatalf("warnings = %v, want a %q warning", warns, WarnMalformed)
	}
	assertContained(t, cfg)
}

// THE SALVAGE HAZARD, checked directly (ADR-0053 / spec §6).
//
// salvageEgressPosture runs a SECOND, permissive decode over the bytes of a
// file that failed the strict one. Its shape (rawPosture) carries the egress
// mode and nothing else, and it must stay that way: a kubernetes: block
// recovered from a file fuse could not parse would be a sandbox posture derived
// from bytes fuse does not trust. A discarded file must therefore resolve to NO
// kubernetes configuration at all — and, because the handler is discarded with
// it, to the container default, which is buildable and contained.
func TestLoadConfigWholeFileDiscardNeverSalvagesTheKubernetesBlock(t *testing.T) {
	bodies := map[string]string{
		"unknown key elsewhere":                    "containd: false\nkubernetes:\n  namespace_prefix: fuse-sb\n  service_account: privileged\n",
		"broken yaml":                              "contained: [oh: no\nkubernetes:\n  namespace_prefix: fuse-sb\n",
		"unknown handler":                          "handler: banana\nkubernetes:\n  namespace_prefix: fuse-sb\n  service_account: privileged\n",
		"handler kubernetes with a typo elsewhere": "handler: kubernetes\ncontaind: false\nkubernetes:\n  service_account: privileged\n",
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeConfigFile(t, root, body)

			cfg, warns := LoadConfig(root)

			if len(warns) == 0 {
				t.Fatal("warnings = none, want a discard diagnostic")
			}
			// The contained default, on the CONTAINER substrate: a discarded file
			// resolves to a substrate that can actually be built, which is why a
			// whole-file discard is not the same thing as the block-level
			// Refused path above.
			assertContained(t, cfg)
			if cfg.Kubernetes != (Kubernetes{}) {
				t.Errorf("Kubernetes = %+v, want the zero value: no field of a discarded file is salvaged", cfg.Kubernetes)
			}
			if cfg.Kubernetes.Refused {
				t.Error("Refused = true on a whole-file discard; the handler is the container default, which IS buildable")
			}
		})
	}
}

// --- WarnLimitNotEnforceable -------------------------------------------------

// pids and nofile have no per-Pod expression in core Kubernetes (spec §4), so an
// operator who configured them under this handler is told ONCE that their cap is
// not enforced. The cap is not discarded — it stays in the resolved Config for
// any other substrate, and for the operator's own record — because silently
// dropping a value the operator wrote is how a limit becomes a fiction.
func TestLoadConfigKubernetesWarnsOnceForUnenforceableLimits(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		wantWarn  bool
		wantNames []string
	}{
		{
			name:      "pids alone",
			body:      "handler: kubernetes\nlimits:\n  pids: 512\n",
			wantWarn:  true,
			wantNames: []string{"pids"},
		},
		{
			name:      "nofile alone",
			body:      "handler: kubernetes\nlimits:\n  nofile: 4096\n",
			wantWarn:  true,
			wantNames: []string{"nofile"},
		},
		{
			// ONE warning naming BOTH, not two warnings. "Emitted once" is the
			// requirement: an operator configuring a whole limits block must not
			// get a warning per field.
			name:      "both together warn once",
			body:      "handler: kubernetes\nlimits:\n  pids: 512\n  nofile: 4096\n",
			wantWarn:  true,
			wantNames: []string{"pids", "nofile"},
		},
		{
			// An enforceable cap under kubernetes says nothing.
			name:     "only enforceable limits",
			body:     "handler: kubernetes\nlimits:\n  memory: 2g\n  cpus: \"2.0\"\n  fsize: 1g\n",
			wantWarn: false,
		},
		{
			// The same caps under the CONTAINER handler are fully enforceable, so
			// warning there would be noise on the common path.
			name:     "pids under the container handler",
			body:     "handler: container\nlimits:\n  pids: 512\n  nofile: 4096\n",
			wantWarn: false,
		},
		{
			// A cap that did not survive parsing is already unset, so there is
			// nothing unenforceable to report — and reporting it would name a
			// value the operator's config does not carry.
			name:     "a bad pids value warns bad_limit only",
			body:     "handler: kubernetes\nlimits:\n  pids: 0\n",
			wantWarn: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeConfigFile(t, root, tc.body)

			_, warns := LoadConfig(root)

			var found []Warning
			for _, w := range warns {
				if w.Reason == WarnLimitNotEnforceable {
					found = append(found, w)
				}
			}
			if !tc.wantWarn {
				if len(found) != 0 {
					t.Fatalf("warnings = %v, want no %q warning", warns, WarnLimitNotEnforceable)
				}
				return
			}
			if len(found) != 1 {
				t.Fatalf("got %d %q warnings, want exactly 1: %v", len(found), WarnLimitNotEnforceable, warns)
			}
			for _, name := range tc.wantNames {
				if !strings.Contains(found[0].Detail, name) {
					t.Errorf("detail = %q, want it to name %q", found[0].Detail, name)
				}
			}
			// The cap itself is NOT discarded: the operator's value stands in the
			// resolved Config, and the warning is what tells them the substrate
			// cannot honour it.
			cfg, _ := LoadConfig(root)
			if strings.Contains(tc.body, "pids: 512") && cfg.Limits.Pids == nil {
				t.Error("pids was discarded; an unenforceable cap is WARNED, not dropped")
			}
			if strings.Contains(tc.body, "nofile: 4096") && cfg.Limits.NoFile == nil {
				t.Error("nofile was discarded; an unenforceable cap is WARNED, not dropped")
			}
		})
	}
}

// --- the closed enums --------------------------------------------------------

// WarnReason is a closed enum used as an event and metric label, so its value
// set is pinned here: a future addition has to come through this test, which is
// what makes it a deliberate act rather than a silently widened label domain.
func TestWarnReasonValueSetIsPinned(t *testing.T) {
	want := []WarnReason{
		WarnNoRoot,
		WarnUnreadable,
		WarnMalformed,
		WarnUnknownHandler,
		WarnContradictory,
		WarnBadIdleTTL,
		WarnBadLimit,
		WarnBadConcurrency,
		WarnUnknownEgressMode,
		WarnBadEgress,
		WarnCredentialPlaintextOnly,
		WarnBadKubernetes,
		WarnLimitNotEnforceable,
	}
	if len(want) != 13 {
		t.Fatalf("this test pins %d reasons; the plan says 13", len(want))
	}

	// Every value distinct: two reasons sharing a string would collapse two
	// causes into one label an operator cannot tell apart.
	seen := make(map[WarnReason]bool, len(want))
	for _, r := range want {
		if r == "" {
			t.Error("a WarnReason is the empty string, which is an unlabelled series")
		}
		if seen[r] {
			t.Errorf("WarnReason %q is declared twice", r)
		}
		seen[r] = true
	}

	// And the two new ones carry their documented spellings, since the strings
	// themselves are the label values.
	if WarnBadKubernetes != "bad_kubernetes" {
		t.Errorf("WarnBadKubernetes = %q, want %q", WarnBadKubernetes, "bad_kubernetes")
	}
	if WarnLimitNotEnforceable != "limit_not_enforceable" {
		t.Errorf("WarnLimitNotEnforceable = %q, want %q", WarnLimitNotEnforceable, "limit_not_enforceable")
	}
}

// The handler enum is likewise closed, and parseHandler is its only door.
func TestParseHandlerAcceptsExactlyThreeValues(t *testing.T) {
	accepted := map[string]string{
		"container":     HandlerContainer,
		"host":          HandlerHost,
		"kubernetes":    HandlerKubernetes,
		"  CONTAINER ":  HandlerContainer,
		"Host":          HandlerHost,
		" Kubernetes\t": HandlerKubernetes,
	}
	for in, want := range accepted {
		if got, ok := parseHandler(in); !ok || got != want {
			t.Errorf("parseHandler(%q) = (%q, %v), want (%q, true)", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "  ", "k8s", "kube", "containers", "hosts", "kubernetes!", "docker", "podman"} {
		if got, ok := parseHandler(in); ok {
			t.Errorf("parseHandler(%q) = (%q, true), want it rejected", in, got)
		}
	}
}
