package sandbox

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeConfigFile materializes .fuse/sandbox.local.yml under root.
func writeConfigFile(t *testing.T, root, body string) string {
	t.Helper()
	dir := filepath.Join(root, configDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	path := filepath.Join(dir, configFileName)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func hasWarning(warns []Warning, reason WarnReason) bool {
	for _, w := range warns {
		if w.Reason == reason {
			return true
		}
	}
	return false
}

// assertContained is the fail-safe assertion every degraded-input case shares:
// the returned Config must be the contained default, and the two containment
// fields must agree with each other.
func assertContained(t *testing.T, cfg Config) {
	t.Helper()
	if !cfg.Contained {
		t.Errorf("Contained = false, want true (fail-safe violated)")
	}
	if cfg.Handler != HandlerContainer {
		t.Errorf("Handler = %q, want %q (fail-safe violated)", cfg.Handler, HandlerContainer)
	}
	assertConsistent(t, cfg)
}

// assertConsistent pins the invariant that Handler is authoritative and
// Contained is exactly its boolean shadow. A caller that reads only one of the
// two must never reach a different conclusion than one that reads the other.
func assertConsistent(t *testing.T, cfg Config) {
	t.Helper()
	if want := cfg.Handler != HandlerHost; cfg.Contained != want {
		t.Errorf("inconsistent config: Handler=%q but Contained=%v", cfg.Handler, cfg.Contained)
	}
}

func TestLoadConfigAbsentFileIsContained(t *testing.T) {
	cfg, warns := LoadConfig(t.TempDir())

	assertContained(t, cfg)
	if len(warns) != 0 {
		t.Errorf("warnings = %v, want none for an absent file", warns)
	}
	if cfg.IdleTTL != DefaultIdleTTL {
		t.Errorf("IdleTTL = %v, want %v", cfg.IdleTTL, DefaultIdleTTL)
	}
}

func TestLoadConfigEmptyFileIsContained(t *testing.T) {
	root := t.TempDir()
	writeConfigFile(t, root, "")

	cfg, warns := LoadConfig(root)

	assertContained(t, cfg)
	// An empty file is indistinguishable in intent from an absent one; it is
	// not a malformed file and must not be reported as one.
	if len(warns) != 0 {
		t.Errorf("warnings = %v, want none for an empty file", warns)
	}
}

func TestLoadConfigUnreadableFileIsContainedAndWarns(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: mode 000 is not enforced, so the case cannot be constructed")
	}
	root := t.TempDir()
	path := writeConfigFile(t, root, "contained: false\n")
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })

	cfg, warns := LoadConfig(root)

	// The file says host. We could not read it, so we must not believe it.
	assertContained(t, cfg)
	if !hasWarning(warns, WarnUnreadable) {
		t.Errorf("warnings = %v, want a %q warning", warns, WarnUnreadable)
	}
}

func TestLoadConfigMalformedYAMLIsContainedAndWarns(t *testing.T) {
	cases := map[string]string{
		"unparsable":       "contained: [oh: no\n",
		"wrong scalar":     "contained: \"not a bool\"\n",
		"not a mapping":    "- contained\n- false\n",
		"unknown key":      "containd: false\n",
		"unknown pool key": "pool:\n  idle_tt1: 5m\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeConfigFile(t, root, body)

			cfg, warns := LoadConfig(root)

			assertContained(t, cfg)
			if !hasWarning(warns, WarnMalformed) {
				t.Errorf("warnings = %v, want a %q warning", warns, WarnMalformed)
			}
		})
	}
}

func TestLoadConfigContainedFalseSelectsHost(t *testing.T) {
	root := t.TempDir()
	writeConfigFile(t, root, "contained: false\n")

	cfg, warns := LoadConfig(root)

	if cfg.Contained {
		t.Errorf("Contained = true, want false")
	}
	if cfg.Handler != HandlerHost {
		t.Errorf("Handler = %q, want %q", cfg.Handler, HandlerHost)
	}
	assertConsistent(t, cfg)
	if len(warns) != 0 {
		t.Errorf("warnings = %v, want none for a well-formed off-switch", warns)
	}
}

func TestLoadConfigHandlerHostSelectsHost(t *testing.T) {
	root := t.TempDir()
	writeConfigFile(t, root, "handler: host\n")

	cfg, warns := LoadConfig(root)

	if cfg.Handler != HandlerHost {
		t.Errorf("Handler = %q, want %q", cfg.Handler, HandlerHost)
	}
	assertConsistent(t, cfg)
	if len(warns) != 0 {
		t.Errorf("warnings = %v, want none", warns)
	}
}

func TestLoadConfigExplicitContainerStaysContained(t *testing.T) {
	root := t.TempDir()
	writeConfigFile(t, root, "contained: true\nhandler: container\n")

	cfg, warns := LoadConfig(root)

	assertContained(t, cfg)
	if len(warns) != 0 {
		t.Errorf("warnings = %v, want none", warns)
	}
}

// A file that both asks for containment and names the host handler is
// contradictory. The explicit handler wins (an operator who typed "host" meant
// it, and `contained: true` is the line every example file already carries),
// but the contradiction is reported loudly so the operator can resolve it.
func TestLoadConfigContradictoryContainedAndHostWarnsAndHonorsHandler(t *testing.T) {
	root := t.TempDir()
	writeConfigFile(t, root, "contained: true\nhandler: host\n")

	cfg, warns := LoadConfig(root)

	if cfg.Handler != HandlerHost {
		t.Errorf("Handler = %q, want %q", cfg.Handler, HandlerHost)
	}
	assertConsistent(t, cfg)
	if !hasWarning(warns, WarnContradictory) {
		t.Errorf("warnings = %v, want a %q warning", warns, WarnContradictory)
	}
}

// contained: false with handler: container is the mirror contradiction. It
// resolves toward containment, which is both the safe direction and the
// explicit handler.
func TestLoadConfigContainedFalseWithContainerHandlerStaysContained(t *testing.T) {
	root := t.TempDir()
	writeConfigFile(t, root, "contained: false\nhandler: container\n")

	cfg, warns := LoadConfig(root)

	assertContained(t, cfg)
	if !hasWarning(warns, WarnContradictory) {
		t.Errorf("warnings = %v, want a %q warning", warns, WarnContradictory)
	}
}

func TestLoadConfigUnknownHandlerIsContainedAndWarns(t *testing.T) {
	for _, body := range []string{
		"handler: banana\n",
		"contained: false\nhandler: banana\n",
		"handler: \"\"\n",
		"handler: HOST~\n",
	} {
		t.Run(body, func(t *testing.T) {
			root := t.TempDir()
			writeConfigFile(t, root, body)

			cfg, warns := LoadConfig(root)

			assertContained(t, cfg)
			if !hasWarning(warns, WarnUnknownHandler) {
				t.Errorf("warnings = %v, want a %q warning", warns, WarnUnknownHandler)
			}
		})
	}
}

// An unusable handler value invalidates the whole file: nothing from a config
// we cannot fully understand is partially honoured.
func TestLoadConfigUnknownHandlerDiscardsTheRestOfTheFile(t *testing.T) {
	root := t.TempDir()
	writeConfigFile(t, root, "handler: banana\nimage: evil:latest\nenv_passthrough: [AWS_SECRET_ACCESS_KEY]\n")

	cfg, _ := LoadConfig(root)

	assertContained(t, cfg)
	if cfg.Image != "" {
		t.Errorf("Image = %q, want empty", cfg.Image)
	}
	if len(cfg.EnvPassthrough) != 0 {
		t.Errorf("EnvPassthrough = %v, want empty", cfg.EnvPassthrough)
	}
}

// The off-switch is file-only. No environment variable, in any spelling, may
// weaken containment.
func TestLoadConfigIgnoresEnvironmentEntirely(t *testing.T) {
	names := []string{
		"FUSE_SANDBOX_CONTAINED",
		"FUSE_SANDBOX",
		"FUSE_SANDBOX_HANDLER",
		"FUSE_SANDBOX_OFF",
		"FUSE_CONTAINED",
		"SANDBOX",
		"SANDBOX_HANDLER",
		"FUSE_SANDBOX_DISABLE",
		"FUSE_DISABLE_SANDBOX",
		"FUSE_SANDBOX_LOCAL",
	}

	// One sub-run per "off" spelling, with EVERY candidate name set to that
	// spelling. Setting each name once per spelling inside a single run would
	// leave only the last spelling in the environment, so a loader checking for
	// any of the others would slip through.
	for _, value := range []string{"false", "off", "0", "no", "none", "host", "disabled", "true", "1"} {
		t.Run(value, func(t *testing.T) {
			for _, name := range names {
				t.Setenv(name, value)
			}

			t.Run("absent file", func(t *testing.T) {
				cfg, warns := LoadConfig(t.TempDir())
				assertContained(t, cfg)
				if len(warns) != 0 {
					t.Errorf("warnings = %v, want none", warns)
				}
			})

			t.Run("file says contained", func(t *testing.T) {
				root := t.TempDir()
				writeConfigFile(t, root, "contained: true\n")
				cfg, _ := LoadConfig(root)
				assertContained(t, cfg)
			})

			t.Run("malformed file", func(t *testing.T) {
				root := t.TempDir()
				writeConfigFile(t, root, "contained: [oh: no\n")
				cfg, _ := LoadConfig(root)
				assertContained(t, cfg)
			})
		})
	}
}

// Structural companion to the behavioral test above: the loader must not so
// much as reference the process environment. A future edit that reintroduces an
// env opt-out fails here even if it is spelled in a way no behavioral test
// enumerated.
func TestConfigLoaderNeverReadsTheProcessEnvironment(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "config.go", nil, 0)
	if err != nil {
		t.Fatalf("parse config.go: %v", err)
	}

	// Matched on the selector name alone, independent of the package
	// qualifier, so routing the read through syscall, or through a local alias
	// for os, does not evade the guard.
	forbidden := map[string]bool{
		"Getenv": true, "LookupEnv": true, "Environ": true, "ExpandEnv": true,
	}
	ast.Inspect(file, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok || !forbidden[sel.Sel.Name] {
			return true
		}
		t.Errorf("config.go references %s at %s: the off-switch is file-only and must never consult the environment",
			sel.Sel.Name, fset.Position(sel.Pos()))
		return true
	})
}

func TestLoadConfigEmptyRootIsContainedAndWarns(t *testing.T) {
	// An empty root would otherwise resolve .fuse/ against the process working
	// directory — a tree the agent itself may be able to write to. A caller
	// that forgot to resolve the repo root gets contained defaults, loudly.
	cfg, warns := LoadConfig("")

	assertContained(t, cfg)
	if !hasWarning(warns, WarnNoRoot) {
		t.Errorf("warnings = %v, want a %q warning", warns, WarnNoRoot)
	}
}

func TestLoadConfigParsesEnvPassthroughAndImage(t *testing.T) {
	root := t.TempDir()
	writeConfigFile(t, root, "env_passthrough: [FOO, BAR, \"  \", \"\"]\nimage: example.invalid/img@sha256:abc\n")

	cfg, warns := LoadConfig(root)

	assertContained(t, cfg)
	if len(warns) != 0 {
		t.Errorf("warnings = %v, want none", warns)
	}
	if got, want := strings.Join(cfg.EnvPassthrough, ","), "FOO,BAR"; got != want {
		t.Errorf("EnvPassthrough = %v, want [FOO BAR] (blank entries dropped, order kept)", cfg.EnvPassthrough)
	}
	if cfg.Image != "example.invalid/img@sha256:abc" {
		t.Errorf("Image = %q", cfg.Image)
	}
}

func TestLoadConfigParsesIdleTTL(t *testing.T) {
	root := t.TempDir()
	writeConfigFile(t, root, "pool:\n  idle_ttl: 90s\n")

	cfg, warns := LoadConfig(root)

	if cfg.IdleTTL != 90*time.Second {
		t.Errorf("IdleTTL = %v, want 90s", cfg.IdleTTL)
	}
	if len(warns) != 0 {
		t.Errorf("warnings = %v, want none", warns)
	}
}

// A bad TTL must fall back to the default, never to zero: a zero TTL would make
// the reaper tear every warm Runner down the instant it is released.
func TestLoadConfigBadIdleTTLFallsBackToDefaultNotZero(t *testing.T) {
	for _, body := range []string{
		"pool:\n  idle_ttl: banana\n",
		"pool:\n  idle_ttl: 300\n", // unitless: not a Go duration
		"pool:\n  idle_ttl: 0\n",
		"pool:\n  idle_ttl: 0s\n",
		"pool:\n  idle_ttl: -5m\n",
		"pool:\n  idle_ttl: \"\"\n",
	} {
		t.Run(body, func(t *testing.T) {
			root := t.TempDir()
			writeConfigFile(t, root, body)

			cfg, warns := LoadConfig(root)

			if cfg.IdleTTL != DefaultIdleTTL {
				t.Errorf("IdleTTL = %v, want the default %v", cfg.IdleTTL, DefaultIdleTTL)
			}
			if cfg.IdleTTL <= 0 {
				t.Fatalf("IdleTTL = %v: a non-positive TTL reaps instantly", cfg.IdleTTL)
			}
			if !hasWarning(warns, WarnBadIdleTTL) {
				t.Errorf("warnings = %v, want a %q warning", warns, WarnBadIdleTTL)
			}
		})
	}
}

// An omitted pool block is not an error and must still yield the default TTL.
func TestLoadConfigOmittedPoolUsesDefaultTTL(t *testing.T) {
	root := t.TempDir()
	writeConfigFile(t, root, "handler: container\n")

	cfg, warns := LoadConfig(root)

	if cfg.IdleTTL != DefaultIdleTTL {
		t.Errorf("IdleTTL = %v, want %v", cfg.IdleTTL, DefaultIdleTTL)
	}
	if len(warns) != 0 {
		t.Errorf("warnings = %v, want none", warns)
	}
}

// Every warning must render a message an operator can act on; a diagnostic the
// caller logs as "{}" is not loud.
func TestWarningMessageIsLoud(t *testing.T) {
	root := t.TempDir()
	writeConfigFile(t, root, "contained: [oh: no\n")

	_, warns := LoadConfig(root)

	if len(warns) == 0 {
		t.Fatal("no warnings")
	}
	for _, w := range warns {
		msg := w.Error()
		if !strings.Contains(msg, string(w.Reason)) {
			t.Errorf("message %q does not name its reason %q", msg, w.Reason)
		}
		if !strings.Contains(msg, configFileName) {
			t.Errorf("message %q does not name the config file", msg)
		}
		if !strings.Contains(strings.ToLower(msg), "contained") {
			t.Errorf("message %q does not state the fail-safe outcome", msg)
		}
	}
}

func TestDefaultConfigIsContained(t *testing.T) {
	cfg := DefaultConfig()

	assertContained(t, cfg)
	if cfg.IdleTTL != DefaultIdleTTL {
		t.Errorf("IdleTTL = %v, want %v", cfg.IdleTTL, DefaultIdleTTL)
	}
	if cfg.Image != "" || len(cfg.EnvPassthrough) != 0 {
		t.Errorf("default config is not empty: %+v", cfg)
	}
}

// --- limits & concurrency (change 0077) --------------------------------------

// derefI64 and derefDur unwrap the presence-carrying pointers for assertions.
func derefI64(t *testing.T, p *int64, field string) int64 {
	t.Helper()
	if p == nil {
		t.Fatalf("%s: unset, want a value", field)
	}
	return *p
}

func TestLoadConfigLimitsAndConcurrencyParse(t *testing.T) {
	root := t.TempDir()
	writeConfigFile(t, root, `
contained: true
limits:
  memory: 2g
  cpus: "2.0"
  pids: 512
  nofile: 4096
  fsize: 1g
  pull_timeout: 2m
concurrency:
  max_inflight: 64
  max_inflight_per_tenant: 16
  max_queued: 256
  note_threshold: 2s
`)

	cfg, warns := LoadConfig(root)

	if len(warns) != 0 {
		t.Fatalf("warnings = %v, want none for a well-formed file", warns)
	}
	if got := derefI64(t, cfg.Limits.MemoryBytes, "memory"); got != 2<<30 {
		t.Errorf("memory = %d, want %d", got, 2<<30)
	}
	if cfg.Limits.CPUs == nil || *cfg.Limits.CPUs != "2.0" {
		t.Errorf("cpus = %v, want \"2.0\"", cfg.Limits.CPUs)
	}
	if got := derefI64(t, cfg.Limits.Pids, "pids"); got != 512 {
		t.Errorf("pids = %d, want 512", got)
	}
	if got := derefI64(t, cfg.Limits.NoFile, "nofile"); got != 4096 {
		t.Errorf("nofile = %d, want 4096", got)
	}
	if got := derefI64(t, cfg.Limits.FsizeBytes, "fsize"); got != 1<<30 {
		t.Errorf("fsize = %d, want %d", got, 1<<30)
	}
	if cfg.Limits.PullTimeout == nil || *cfg.Limits.PullTimeout != 2*time.Minute {
		t.Errorf("pull_timeout = %v, want 2m", cfg.Limits.PullTimeout)
	}
	if got := derefI64(t, cfg.Concurrency.MaxInflight, "max_inflight"); got != 64 {
		t.Errorf("max_inflight = %d, want 64", got)
	}
	if got := derefI64(t, cfg.Concurrency.MaxInflightPerTenant, "max_inflight_per_tenant"); got != 16 {
		t.Errorf("max_inflight_per_tenant = %d, want 16", got)
	}
	if got := derefI64(t, cfg.Concurrency.MaxQueued, "max_queued"); got != 256 {
		t.Errorf("max_queued = %d, want 256", got)
	}
	if cfg.Concurrency.NoteThreshold == nil || *cfg.Concurrency.NoteThreshold != 2*time.Second {
		t.Errorf("note_threshold = %v, want 2s", cfg.Concurrency.NoteThreshold)
	}
}

// An absent block leaves every field unset, so NewService's posture defaults
// (not a zero cap) decide the outcome.
func TestLoadConfigNoLimitsLeavesEverythingUnset(t *testing.T) {
	root := t.TempDir()
	writeConfigFile(t, root, "contained: true\n")

	cfg, warns := LoadConfig(root)

	if len(warns) != 0 {
		t.Fatalf("warnings = %v, want none", warns)
	}
	if cfg.Limits != (Limits{}) {
		t.Errorf("Limits = %+v, want zero (all unset)", cfg.Limits)
	}
	if cfg.Concurrency != (Concurrency{}) {
		t.Errorf("Concurrency = %+v, want zero (all unset)", cfg.Concurrency)
	}
}

// A malformed or non-positive value degrades to unset with the new WarnReason,
// and the Config stays directly usable — never a zero cap.
func TestLoadConfigBadLimitDegradesToUnset(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"unparsable memory", "limits:\n  memory: two-gigs\n"},
		{"non-positive pids", "limits:\n  pids: 0\n"},
		{"negative nofile", "limits:\n  nofile: -1\n"},
		{"unparsable cpus", "limits:\n  cpus: \"lots\"\n"},
		{"bad pull_timeout", "limits:\n  pull_timeout: soon\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeConfigFile(t, root, "contained: true\n"+tc.body)

			cfg, warns := LoadConfig(root)

			if !hasWarning(warns, WarnBadLimit) {
				t.Errorf("warnings = %v, want a %q", warns, WarnBadLimit)
			}
			assertContained(t, cfg) // still usable, still safe
			if cfg.Limits != (Limits{}) {
				t.Errorf("Limits = %+v, want zero after a bad value", cfg.Limits)
			}
		})
	}
}

func TestLoadConfigBadConcurrencyDegradesToUnset(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"zero backstop", "concurrency:\n  max_inflight: 0\n"},
		{"negative per-tenant", "concurrency:\n  max_inflight_per_tenant: -3\n"},
		{"bad note_threshold", "concurrency:\n  note_threshold: whenever\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeConfigFile(t, root, "contained: true\n"+tc.body)

			cfg, warns := LoadConfig(root)

			if !hasWarning(warns, WarnBadConcurrency) {
				t.Errorf("warnings = %v, want a %q", warns, WarnBadConcurrency)
			}
			if cfg.Concurrency != (Concurrency{}) {
				t.Errorf("Concurrency = %+v, want zero after a bad value", cfg.Concurrency)
			}
		})
	}
}

// An explicit zero is distinguishable from absent: it parses as a rejected value
// (a cap of zero is not a cap) and degrades to unset — but the point is that the
// loader SEES it and warns, rather than silently reading absent.
func TestLoadConfigExplicitZeroIsSeen(t *testing.T) {
	root := t.TempDir()
	writeConfigFile(t, root, "concurrency:\n  max_queued: 0\n")

	_, warns := LoadConfig(root)

	if !hasWarning(warns, WarnBadConcurrency) {
		t.Errorf("an explicit zero was not seen: warns = %v", warns)
	}
}

// An unrecognised key inside the new blocks still trips KnownFields(true) and
// discards the whole file toward the safe default.
func TestLoadConfigUnknownKeyInLimitsIsMalformed(t *testing.T) {
	root := t.TempDir()
	writeConfigFile(t, root, "limits:\n  memroy: 2g\n")

	cfg, warns := LoadConfig(root)

	if !hasWarning(warns, WarnMalformed) {
		t.Errorf("warnings = %v, want %q for an unknown key", warns, WarnMalformed)
	}
	assertContained(t, cfg)
}

func TestParseBytes(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		ok   bool
	}{
		{"2g", 2 << 30, true},
		{"512m", 512 << 20, true},
		{"1k", 1 << 10, true},
		{"1048576", 1048576, true},
		{"2G", 2 << 30, true},
		{"  4g  ", 4 << 30, true},
		{"0", 0, false},
		{"-1", 0, false},
		{"nonsense", 0, false},
		{"", 0, false},
	}
	for _, tc := range cases {
		got, ok := parseBytes(tc.in)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("parseBytes(%q) = (%d, %v), want (%d, %v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestParseCPUsCanonicalises(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"2", "2.0", true},
		{"2.0", "2.0", true},
		{"0.5", "0.5", true},
		{"1.500", "1.5", true},
		{"0", "", false},
		{"-1", "", false},
		{"lots", "", false},
	}
	for _, tc := range cases {
		got, ok := parseCPUs(tc.in)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("parseCPUs(%q) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}
