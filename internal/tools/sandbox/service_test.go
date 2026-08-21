package sandbox

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ethanhinson/fuse/internal/loopauth"
)

// recordingHandler is a Handler double that records every Acquire, and whose
// Runners record every Exec.
//
// It exists for exactly one assertion: on the fail-closed path, NOTHING runs.
// "Acquire returned an error" is not enough — a fallback that quietly acquired
// a host Runner first and then errored, or one that ran a probe command, would
// pass a naive error assertion while still having executed model-authored shell
// on the host. Counting is what makes "nothing happened" observable.
//
// It is mutex-guarded because a Handler may legitimately be shared across
// goroutines (learning: mutex-test-double-concurrent-provider).
type recordingHandler struct {
	name string

	mu       sync.Mutex
	acquires int
	execs    []string
}

func (h *recordingHandler) Name() string { return h.name }

func (h *recordingHandler) Acquire(context.Context, loopauth.Principal, Env) (Runner, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.acquires++
	return &recordingRunner{handler: h}, nil
}

func (h *recordingHandler) counts() (acquires int, execs []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.acquires, append([]string(nil), h.execs...)
}

type recordingRunner struct{ handler *recordingHandler }

func (r *recordingRunner) Exec(_ context.Context, cmd string, _ string) (Output, error) {
	r.handler.mu.Lock()
	defer r.handler.mu.Unlock()
	r.handler.execs = append(r.handler.execs, cmd)
	return Output{}, nil
}

func (r *recordingRunner) Release(context.Context) error { return nil }

// recordingExec is an execRunner double for the container path: it records
// every CLI invocation instead of shelling out.
type recordingExec struct {
	mu    sync.Mutex
	calls [][]string
}

func (e *recordingExec) run(_ context.Context, name string, args ...string) ([]byte, int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, append([]string{name}, args...))
	return nil, 0, nil
}

func (e *recordingExec) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.calls)
}

// runCalls returns only the container-RUN invocations, filtering out the
// pre-pull `pull <image>` calls (change 0077). A test asserting "the run mounted
// the trusted root" cares about runs, not the separate, expected pre-pull.
func (e *recordingExec) runCalls() [][]string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var runs [][]string
	for _, c := range e.calls {
		// c is [name, arg0, ...]; a pull is `<cli> pull <image>`.
		if len(c) >= 2 && c[1] == "pull" {
			continue
		}
		runs = append(runs, c)
	}
	return runs
}

// assertHandlerRoutes checks the label AND the concrete Runner the Service
// hands out. Asserting only HandlerName() would pass for a Service that
// reported "container" while acquiring from the host handler.
func assertHandlerRoutes(t *testing.T, svc *Service, want string) {
	t.Helper()

	if got := svc.HandlerName(); got != want {
		t.Errorf("HandlerName() = %q, want %q", got, want)
	}
	if got, wantContained := svc.Contained(), want != HandlerHost; got != wantContained {
		t.Errorf("Contained() = %v, want %v for handler %q", got, wantContained, want)
	}
	if !svc.Available() {
		t.Errorf("Available() = false, want true when handler %q was selected", want)
	}

	runner, err := svc.Acquire(context.Background(), testPrincipal())
	if err != nil {
		t.Fatalf("Acquire: unexpected error: %v", err)
	}
	if runner == nil {
		t.Fatalf("Acquire returned a nil Runner with a nil error")
	}
	switch runner.(type) {
	case *containerRunner:
		if want != HandlerContainer {
			t.Errorf("Acquire returned a container Runner, want a %q Runner", want)
		}
	case *hostRunner:
		if want != HandlerHost {
			t.Errorf("Acquire returned a HOST Runner, want a %q Runner", want)
		}
	default:
		t.Errorf("Acquire returned unexpected Runner type %T", runner)
	}
}

// TestServiceSelectionMatrix is the full contained/hosted decision table.
//
// The hosted rows are the ones that matter most: under the hosted (loop-server)
// posture the file-only off-switch is structurally INERT, so an operator file
// — or an agent that managed to author one — cannot turn containment off for
// someone else's workload.
func TestServiceSelectionMatrix(t *testing.T) {
	tests := []struct {
		name   string
		file   string // "" means no config file at all
		hosted bool
		want   string
	}{
		{name: "absent config is contained", want: HandlerContainer},
		{name: "absent config is contained when hosted", hosted: true, want: HandlerContainer},
		{name: "contained true is contained", file: "contained: true\n", want: HandlerContainer},
		{name: "handler container is contained", file: "handler: container\n", want: HandlerContainer},

		// The off-switch, honoured: local operator, local machine.
		{name: "contained false selects host", file: "contained: false\n", want: HandlerHost},
		{name: "handler host selects host", file: "handler: host\n", want: HandlerHost},

		// The off-switch, inert: hosted posture ignores it ENTIRELY.
		{name: "hosted ignores contained false", file: "contained: false\n", hosted: true, want: HandlerContainer},
		{name: "hosted ignores handler host", file: "handler: host\n", hosted: true, want: HandlerContainer},

		// A file we could not understand must not become an off-switch.
		{name: "malformed config is contained", file: "contained: [not, a, bool]\n", want: HandlerContainer},
		{name: "unknown handler is contained", file: "handler: vm\n", want: HandlerContainer},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if tc.file != "" {
				writeConfigFile(t, root, tc.file)
			}

			svc, _, err := NewServiceFromRoot(root,
				WithHostedPosture(tc.hosted),
				withContainerLookPath(fakeLookPath("docker")),
			)
			if err != nil {
				t.Fatalf("NewServiceFromRoot: %v", err)
			}

			assertHandlerRoutes(t, svc, tc.want)
		})
	}
}

// TestServiceFailsClosedWithoutContainerRuntime is the fail-closed rule: when
// the container substrate could not be constructed and no local operator
// authorized the host, Acquire REFUSES. It never degrades to the host, because
// "docker isn't installed" must not be a way to turn containment off.
func TestServiceFailsClosedWithoutContainerRuntime(t *testing.T) {
	tests := []struct {
		name   string
		file   string
		hosted bool
	}{
		{name: "no cli and no authorization"},
		{name: "no cli and contained true", file: "contained: true\n"},
		// Hosted makes the off-switch inert, so these are unauthorized too —
		// and a refusal, never a host fallback.
		{name: "hosted with contained false", file: "contained: false\n", hosted: true},
		{name: "hosted with handler host", file: "handler: host\n", hosted: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if tc.file != "" {
				writeConfigFile(t, root, tc.file)
			}

			host := &recordingHandler{name: HandlerHost}
			cli := &recordingExec{}

			svc, _, err := NewServiceFromRoot(root,
				WithHostedPosture(tc.hosted),
				withContainerLookPath(fakeLookPath()), // nothing on PATH
				withContainerExec(cli.run),
				withHostHandler(host),
			)
			if err != nil {
				// Construction must NOT fail: an error return here invites the
				// caller to write a fallback. The refusal belongs at Acquire.
				t.Fatalf("NewServiceFromRoot: unexpected construction error: %v", err)
			}

			if svc.Available() {
				t.Errorf("Available() = true, want false with no container runtime and no host authorization")
			}
			if got := svc.HandlerName(); got != "" {
				t.Errorf("HandlerName() = %q, want %q when nothing was selected", got, "")
			}

			// Deliberately Errorf, not Fatalf: a fallback that acquires a host
			// Runner must still be caught by the did-anything-run assertions
			// below, and a Fatalf here would skip exactly the check that sees a
			// silent host acquisition.
			runner, err := svc.Acquire(context.Background(), testPrincipal())
			switch {
			case err == nil:
				t.Errorf("Acquire succeeded (%T), want a refusal", runner)
			default:
				if runner != nil {
					t.Errorf("Acquire returned a non-nil Runner (%T) alongside the refusal", runner)
				}
				if !errors.Is(err, ErrRefusedUncontained) {
					t.Errorf("Acquire error = %v, want it to wrap ErrRefusedUncontained", err)
				}
				if want := "refusing to run bash uncontained"; !strings.Contains(err.Error(), want) {
					t.Errorf("Acquire error = %q, want it to contain %q", err.Error(), want)
				}
			}
			if _, isHost := runner.(*hostRunner); isHost {
				// Note we do NOT Exec it: the point is that this Runner should
				// not exist, and running a command through it to prove that
				// would be the very thing under test.
				t.Errorf("Acquire handed out a real host Runner on the refusal path")
			}

			// The load-bearing assertion: nothing ran anywhere.
			if acquires, execs := host.counts(); acquires != 0 || len(execs) != 0 {
				t.Errorf("host handler saw %d Acquire and %d Exec calls, want 0 and 0 — the refusal path must never touch the host",
					acquires, len(execs))
			}
			if n := cli.count(); n != 0 {
				t.Errorf("container CLI was invoked %d times on the refusal path, want 0", n)
			}
		})
	}
}

// A hand-built zero-value Service (constructible as &Service{} since the type
// is exported with unexported fields) has both handler and refusal nil.
// Acquire must still refuse rather than returning (nil, nil) — a nil Runner
// with a nil error, which a caller like Pool.acquireFresh would store and
// later nil-deref on Exec.
func TestServiceZeroValueAcquireRefusesRatherThanNilNil(t *testing.T) {
	svc := &Service{}

	runner, err := svc.Acquire(context.Background(), testPrincipal())
	if err == nil {
		t.Fatalf("Acquire on a zero-value Service returned a nil error (runner=%v), want a refusal", runner)
	}
	if runner != nil {
		t.Errorf("Acquire on a zero-value Service returned a non-nil Runner (%T) alongside an error", runner)
	}
	if !errors.Is(err, ErrRefusedUncontained) {
		t.Errorf("Acquire error = %v, want it to wrap ErrRefusedUncontained", err)
	}
}

// A missing container CLI is only a refusal when the host was NOT authorized.
// The host substrate needs no CLI, so a local operator who explicitly asked for
// it still gets it. This is the companion that keeps the refusal specific.
func TestServiceNoCLIStillHonoursLocalHostAuthorization(t *testing.T) {
	root := t.TempDir()
	writeConfigFile(t, root, "handler: host\n")

	svc, _, err := NewServiceFromRoot(root, withContainerLookPath(fakeLookPath()))
	if err != nil {
		t.Fatalf("NewServiceFromRoot: %v", err)
	}

	assertHandlerRoutes(t, svc, HandlerHost)
}

// TestServiceReadsConfigOnceAtConstruction pins the attack this Service exists
// to make unwritable: an agent that can write files authors its own
// .fuse/sandbox.local.yml mid-loop and turns containment off for its next bash
// call. Selection must be frozen at construction against the trusted root.
func TestServiceReadsConfigOnceAtConstruction(t *testing.T) {
	t.Run("a config written after construction cannot turn containment off", func(t *testing.T) {
		root := t.TempDir()

		svc, _, err := NewServiceFromRoot(root, withContainerLookPath(fakeLookPath("docker")))
		if err != nil {
			t.Fatalf("NewServiceFromRoot: %v", err)
		}
		assertHandlerRoutes(t, svc, HandlerContainer)

		// The agent writes the off-switch, mid-loop.
		writeConfigFile(t, root, "contained: false\n")

		assertHandlerRoutes(t, svc, HandlerContainer)
	})

	t.Run("removing the config after construction does not change selection", func(t *testing.T) {
		root := t.TempDir()
		path := writeConfigFile(t, root, "handler: host\n")

		svc, _, err := NewServiceFromRoot(root, withContainerLookPath(fakeLookPath("docker")))
		if err != nil {
			t.Fatalf("NewServiceFromRoot: %v", err)
		}
		assertHandlerRoutes(t, svc, HandlerHost)

		if err := os.Remove(path); err != nil {
			t.Fatalf("remove config: %v", err)
		}

		// The decision was made once, from the trusted root, at construction.
		// It does not drift with the filesystem underneath a running loop.
		assertHandlerRoutes(t, svc, HandlerHost)
	})
}

// Structural companion to the behavioral test above. A per-call re-read is the
// vulnerability; this fails on the shape, not just on one observed behaviour,
// so an edit that reintroduces a re-read in a method no behavioral test happens
// to exercise still turns red.
func TestServiceMethodsNeverReloadConfig(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "service.go", nil, 0)
	if err != nil {
		t.Fatalf("parse service.go: %v", err)
	}

	// Matched on the selector/ident name alone so routing the read through an
	// alias, or through syscall, does not evade the guard.
	forbidden := map[string]bool{
		"LoadConfig": true, "ReadFile": true, "ReadDir": true,
		"Open": true, "OpenFile": true, "Stat": true, "WalkDir": true,
	}

	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || fn.Body == nil {
			// Only METHODS are constrained: the constructor is exactly where a
			// single load is allowed to happen.
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			var name string
			switch e := n.(type) {
			case *ast.SelectorExpr:
				name = e.Sel.Name
			case *ast.Ident:
				name = e.Name
			default:
				return true
			}
			if forbidden[name] {
				t.Errorf("method %s references %s at %s: the sandbox config is loaded ONCE at construction; "+
					"re-reading it per call lets an agent author its own off-switch mid-loop",
					fn.Name.Name, name, fset.Position(n.Pos()))
			}
			return true
		})
	}
}

// LoadConfig's warnings are the only signal an operator gets that their file
// did not do what they thought. A Service that swallowed them would leave
// someone believing containment is off when it is not.
func TestServiceSurfacesConfigWarnings(t *testing.T) {
	root := t.TempDir()
	path := writeConfigFile(t, root, "handler: nonsense\n")

	svc, warns, err := NewServiceFromRoot(root, withContainerLookPath(fakeLookPath("docker")))
	if err != nil {
		t.Fatalf("NewServiceFromRoot: %v", err)
	}

	if !hasWarning(warns, WarnUnknownHandler) {
		t.Errorf("returned warnings = %v, want a %q warning", warns, WarnUnknownHandler)
	}
	if !hasWarning(svc.Warnings(), WarnUnknownHandler) {
		t.Errorf("svc.Warnings() = %v, want a %q warning", svc.Warnings(), WarnUnknownHandler)
	}
	if len(warns) > 0 && !strings.Contains(warns[0].Error(), filepath.Base(path)) {
		t.Errorf("warning %q does not name the config file", warns[0].Error())
	}

	// And the degraded load is still contained.
	assertHandlerRoutes(t, svc, HandlerContainer)
}

// The hosted posture is a property of how this process was deployed. It is
// supplied by the composition root and must never be derivable from anything an
// agent (or a config file) can influence.
func TestServiceHostedPostureIsNotConfigurable(t *testing.T) {
	root := t.TempDir()
	// Every spelling someone might reach for to smuggle the posture in through
	// the file. All are unknown keys, so the loader rejects the file wholesale.
	writeConfigFile(t, root, "hosted: true\ncontained: false\n")

	svc, warns, err := NewServiceFromRoot(root, withContainerLookPath(fakeLookPath("docker")))
	if err != nil {
		t.Fatalf("NewServiceFromRoot: %v", err)
	}
	if len(warns) == 0 {
		t.Errorf("want a warning for the unrecognised hosted: key")
	}
	assertHandlerRoutes(t, svc, HandlerContainer)

	// Structural: no on-disk config field feeds the posture.
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "service.go", nil, 0)
	if err != nil {
		t.Fatalf("parse service.go: %v", err)
	}
	forbidden := map[string]bool{"Getenv": true, "LookupEnv": true, "Environ": true, "ExpandEnv": true}
	ast.Inspect(file, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok || !forbidden[sel.Sel.Name] {
			return true
		}
		if sel.Sel.Name == "LookupEnv" {
			// os.LookupEnv is the default env-SCRUB lookup (what a command may
			// observe). That is not the off-switch; allow it and keep the guard
			// on the rest.
			return true
		}
		t.Errorf("service.go references %s at %s: the hosted posture and containment come from the composition root and the file, never the environment",
			sel.Sel.Name, fset.Position(sel.Pos()))
		return true
	})
}

// The Env a Service hands to its Handler is the scrubbed allowlist, resolved
// from the operator's declared passthrough — not the process environment.
func TestServiceResolvesScrubbedEnvForTheHandler(t *testing.T) {
	root := t.TempDir()
	writeConfigFile(t, root, "handler: host\nenv_passthrough: [ALLOWED]\n")

	svc, _, err := NewServiceFromRoot(root,
		withContainerLookPath(fakeLookPath("docker")),
		WithEnvLookup(func(key string) (string, bool) {
			switch key {
			case "PATH":
				return "/usr/bin", true
			case "ALLOWED":
				return "yes", true
			case "FUSE_TEST_SECRET":
				return "leaked", true
			}
			return "", false
		}),
	)
	if err != nil {
		t.Fatalf("NewServiceFromRoot: %v", err)
	}

	runner, err := svc.Acquire(context.Background(), testPrincipal())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	hr, ok := runner.(*hostRunner)
	if !ok {
		t.Fatalf("Acquire returned %T, want *hostRunner", runner)
	}
	if got, want := hr.env, []string{"ALLOWED=yes", "PATH=/usr/bin"}; strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("runner env = %v, want %v", got, want)
	}
	if hr.principal != testPrincipal() {
		t.Errorf("runner principal = %+v, want %+v", hr.principal, testPrincipal())
	}
}

// NewService is the composition-root form: the Config is already loaded, and
// the posture is passed explicitly. A zero Config must be contained — the
// zero value's Contained field is false, and nothing may read that as "the
// operator authorized the host".
func TestNewServiceZeroConfigIsContained(t *testing.T) {
	svc, err := NewService(Config{}, withContainerLookPath(fakeLookPath("docker")))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	assertHandlerRoutes(t, svc, HandlerContainer)
}

// A hand-built Config that bypassed the loader's consistency guarantee — the
// invariant Contained == (Handler != HandlerHost) broken in either direction —
// must never be read as host authorization. Only a Config where BOTH fields say
// host reaches the host.
func TestNewServiceInconsistentConfigIsContained(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
	}{
		{name: "contained false with a container handler", cfg: Config{Contained: false, Handler: HandlerContainer}},
		{name: "contained true with a host handler", cfg: Config{Contained: true, Handler: HandlerHost}},
		{name: "empty handler with contained false", cfg: Config{Contained: false, Handler: ""}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc, err := NewService(tc.cfg, withContainerLookPath(fakeLookPath("docker")))
			if err != nil {
				t.Fatalf("NewService: %v", err)
			}
			assertHandlerRoutes(t, svc, HandlerContainer)
		})
	}
}

// The trusted root the composition root supplied must actually reach the
// substrate, or the whole containment argument is theory: NewServiceFromRoot
// resolves a root, and the container handler is what mounts it. Nothing between
// them may drop it, and no caller-supplied option may substitute another.
func TestServiceThreadsTrustedRootToTheContainerMount(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	cli := &recordingExec{}

	svc, _, err := NewServiceFromRoot(root,
		withContainerLookPath(fakeLookPath("docker")),
		withContainerExec(cli.run),
		// Supplied BEFORE the root NewServiceFromRoot was trusted with, and it
		// must lose: an option cannot redirect the mount.
		WithTrustedRoot("/"),
	)
	if err != nil {
		t.Fatalf("NewServiceFromRoot: %v", err)
	}
	if got := svc.TrustedRoot(); got != root {
		t.Fatalf("TrustedRoot() = %q, want %q", got, root)
	}

	runner, err := svc.Acquire(context.Background(), testPrincipal())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	// working_dir unset: the default the bash tool sends when a model leaves
	// the optional argument alone.
	if _, err := runner.Exec(context.Background(), "true", ""); err != nil {
		t.Fatalf("Exec: %v", err)
	}

	runs := cli.runCalls()
	if len(runs) != 1 {
		t.Fatalf("container RUN invocations = %d, want 1 (pulls excluded)", len(runs))
	}
	argv := runs[0]

	want := root + ":" + containerWorkspace
	found := false
	for i, a := range argv {
		if a == "-v" && i+1 < len(argv) {
			if argv[i+1] != want {
				t.Fatalf("-v = %q, want %q (argv: %#v)", argv[i+1], want, argv)
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("argv mounts nothing; the trusted root never reached the substrate: %#v", argv)
	}
}
