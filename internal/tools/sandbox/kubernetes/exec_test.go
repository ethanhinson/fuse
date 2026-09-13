package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/remotecommand"
	utilexec "k8s.io/utils/exec"

	"github.com/ethanhinson/fuse/internal/tools/sandbox"
)

// TestExecArgvGolden is the argv golden, and the reason it is a golden rather
// than a shape check is the container_test.go precedent: this command is the
// entire boundary between "the environment fuse resolved" and "the environment
// the command observes", so every token is a decision.
//
// The shape (spec §3):
//
//	/usr/bin/env -i K1=V1 … /bin/sh -c 'cd -- "$1" && exec /bin/sh -c "$2"' sh <workingDir> <cmd>
//
// Three properties matter and each is deliberate:
//
//   - `env -i` makes the environment COMPLETE, not additive. The container spec
//     carries none, so this argv is the whole environment — which is what keeps
//     #63's scrub invariant (empty + explicit allowlist) on a Pod whose container
//     env cannot be changed after creation, and what makes ResetEnv mean
//     something.
//   - The workingDir and the command are POSITIONAL PARAMETERS ($1, $2), never
//     interpolated into the script text. A workingDir containing a quote or a
//     space would otherwise close the script's quoting and append arbitrary
//     shell — the classic injection. Positional parameters cannot be re-parsed.
//   - `cd --` ends option parsing, so a workingDir beginning with `-` is a path
//     and not a flag to cd.
func TestExecArgvGolden(t *testing.T) {
	tests := []struct {
		name       string
		env        sandbox.Env
		cmd        string
		workingDir string
		want       []string
	}{
		{
			name:       "the ordinary case",
			env:        sandbox.Env{Allow: map[string]string{"PATH": "/usr/bin:/bin", "HOME": "/workspace", "LANG": "C"}},
			cmd:        "ls -la",
			workingDir: "/workspace",
			want: []string{
				"/usr/bin/env", "-i",
				// SORTED. argv is golden-tested, so the order has to be
				// deterministic; renderEnv on the sandbox side sorts for exactly
				// this reason.
				"HOME=/workspace", "LANG=C", "PATH=/usr/bin:/bin",
				"/bin/sh", "-c", execScript, "sh",
				"/workspace", "ls -la",
			},
		},
		{
			name:       "an EMPTY allowlist renders no assignments and still scrubs",
			env:        sandbox.Env{Allow: map[string]string{}},
			cmd:        "env",
			workingDir: "/workspace",
			// `env -i` with nothing after it is the DENY-ALL environment and is a
			// legitimate resolved state (an operator with no passthrough, or a
			// posture the loader salvaged). The -i must still be there: without it
			// the command would inherit the image's own environment.
			want: []string{
				"/usr/bin/env", "-i",
				"/bin/sh", "-c", execScript, "sh",
				"/workspace", "env",
			},
		},
		{
			name:       "a workingDir containing a SPACE",
			env:        sandbox.Env{Allow: map[string]string{"PATH": "/bin"}},
			cmd:        "pwd",
			workingDir: "/workspace/my project",
			// One argv element, unquoted and unescaped. It is a positional
			// parameter, so the shell never re-parses it and there is nothing to
			// escape. Any quoting added here would land in the path itself.
			want: []string{
				"/usr/bin/env", "-i", "PATH=/bin",
				"/bin/sh", "-c", execScript, "sh",
				"/workspace/my project", "pwd",
			},
		},
		{
			name:       "a workingDir containing a QUOTE",
			env:        sandbox.Env{Allow: map[string]string{"PATH": "/bin"}},
			cmd:        "pwd",
			workingDir: `/workspace/it's "here"`,
			want: []string{
				"/usr/bin/env", "-i", "PATH=/bin",
				"/bin/sh", "-c", execScript, "sh",
				`/workspace/it's "here"`, "pwd",
			},
		},
		{
			name: "a workingDir that would be a shell INJECTION if interpolated",
			env:  sandbox.Env{Allow: map[string]string{}},
			cmd:  "pwd",
			// If the working directory were spliced into the script text, this
			// closes the quoting and runs `id`. As a positional parameter it is
			// just a (nonexistent) path and `cd` fails.
			workingDir: `/workspace/x' && id && echo '`,
			want: []string{
				"/usr/bin/env", "-i",
				"/bin/sh", "-c", execScript, "sh",
				`/workspace/x' && id && echo '`, "pwd",
			},
		},
		{
			name:       "a workingDir beginning with a hyphen",
			env:        sandbox.Env{Allow: map[string]string{}},
			cmd:        "pwd",
			workingDir: "-rf",
			// `cd --` in the script is what makes this a path rather than a flag.
			want: []string{
				"/usr/bin/env", "-i",
				"/bin/sh", "-c", execScript, "sh",
				"-rf", "pwd",
			},
		},
		{
			name:       "a command containing quotes, newlines and a subshell",
			env:        sandbox.Env{Allow: map[string]string{}},
			cmd:        "echo \"a b\"\nfor i in 1 2; do echo $(date); done",
			workingDir: "/workspace",
			// The command reaches `/bin/sh -c "$2"` as ONE argument, untouched —
			// the same contract the container handler's `/bin/sh -c cmd` keeps, so
			// a command that works on one substrate works on the other.
			want: []string{
				"/usr/bin/env", "-i",
				"/bin/sh", "-c", execScript, "sh",
				"/workspace", "echo \"a b\"\nfor i in 1 2; do echo $(date); done",
			},
		},
		{
			name: "an env VALUE containing an equals sign and spaces",
			env:  sandbox.Env{Allow: map[string]string{"GIT_CONFIG": "a=b c=d"}},
			cmd:  "true",
			// `env -i` takes each assignment as one argv element, so a value with
			// '=' or whitespace needs no escaping and must not get any.
			workingDir: "/workspace",
			want: []string{
				"/usr/bin/env", "-i", "GIT_CONFIG=a=b c=d",
				"/bin/sh", "-c", execScript, "sh",
				"/workspace", "true",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestSubstrate(t, fake.NewClientset())
			got := s.execArgv(tc.env, tc.cmd, tc.workingDir)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("execArgv mismatch\n got: %#v\nwant: %#v", got, tc.want)
			}
		})
	}
}

// TestExecScriptShape pins the script itself, because the golden above treats it
// as one opaque token and the properties live INSIDE it.
func TestExecScriptShape(t *testing.T) {
	// Positional parameters, never interpolation. This is the whole
	// injection-safety argument.
	if !strings.Contains(execScript, `"$1"`) {
		t.Error(`the script must cd to "$1" — a positional parameter, so a workingDir with a quote cannot close the script's quoting and append shell`)
	}
	if !strings.Contains(execScript, `"$2"`) {
		t.Error(`the script must run "$2" — the command as a positional parameter`)
	}
	// `cd --` ends option parsing, so a directory named "-rf" is a path.
	if !strings.Contains(execScript, "cd -- ") {
		t.Error("the script must use `cd --` so a workingDir beginning with a hyphen is a path and not a flag")
	}
	// `&&`, not `;`: a failed cd must NOT fall through to running the command in
	// whatever directory the shell happened to start in. That would run the
	// caller's command somewhere they did not ask for — the same refusal
	// resolveWorkspace makes on the host side.
	if !strings.Contains(execScript, "&&") {
		t.Error("the script must chain with && so a failed cd does NOT fall through to running the command in the wrong directory")
	}
	if strings.Contains(execScript, ";") {
		t.Errorf("the script %q chains with ';', which runs the command even when cd failed", execScript)
	}
	// exec, so the shell replaces itself: one less process between the caller's
	// deadline and the command, and the command's exit status is the shell's.
	if !strings.Contains(execScript, "exec ") {
		t.Error("the script must `exec` the inner shell so the command's exit status is what the exec subresource reports")
	}
}

// TestExecEnvProxyKeysUnderEnforce is the strip-and-reinject table.
//
// Under enforcement these eight variables are the TRUSTED SIDE's: an operator's
// env_passthrough naming HTTP_PROXY could redirect egress at a host of their
// choosing, and NO_PROXY could exempt destinations from it. Both are the same
// defect, which is why it is one list — a variable this package injects is a
// variable this package owns.
func TestExecEnvProxyKeysUnderEnforce(t *testing.T) {
	// Every one of the eight, with a hostile value, plus an ordinary variable
	// that must survive untouched.
	hostile := map[string]string{
		"PATH":        "/usr/bin:/bin",
		"HTTP_PROXY":  "http://attacker.example:8080",
		"HTTPS_PROXY": "http://attacker.example:8080",
		"ALL_PROXY":   "socks5://attacker.example:1080",
		"NO_PROXY":    "*",
		"http_proxy":  "http://attacker.example:8080",
		"https_proxy": "http://attacker.example:8080",
		"all_proxy":   "socks5://attacker.example:1080",
		"no_proxy":    "*",
	}

	t.Run("enforce strips and re-injects", func(t *testing.T) {
		// enforcing() supplies the advertise address and credential source the
		// enforcing posture now requires at construction (task 8); the assertion
		// below is unchanged and is about the exec ARGV, not the datapath.
		s := newTestSubstrate(t, fake.NewClientset(), enforcing("10.1.2.3"))
		got := assignments(t, s.execArgv(sandbox.Env{Allow: hostile}, "true", "/workspace"))

		if got["PATH"] != "/usr/bin:/bin" {
			t.Errorf("PATH = %q; a non-proxy variable must pass through untouched", got["PATH"])
		}
		wantProxy := "http://127.0.0.1:3128"
		for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
			if got[k] != wantProxy {
				t.Errorf("%s = %q, want %q — the operator's value must be REPLACED, never merely joined", k, got[k], wantProxy)
			}
		}
		// Explicitly EMPTY, not omitted. Omitting it leaves whatever the image or
		// a base layer baked in, and a single NO_PROXY=* is a complete bypass of
		// everything above.
		for _, k := range []string{"NO_PROXY", "no_proxy"} {
			v, present := got[k]
			if !present {
				t.Errorf("%s is absent; it must be set EXPLICITLY EMPTY so an image-baked value cannot bypass the proxy", k)
			}
			if v != "" {
				t.Errorf("%s = %q, want empty", k, v)
			}
		}
		// BOTH cases, because the ecosystem is split: curl reads lowercase and
		// deliberately ignores uppercase HTTP_PROXY; Go and most runtimes read
		// either. Emitting one case leaves the other free for an inherited value.
		for _, k := range []string{"HTTP_PROXY", "http_proxy"} {
			if _, ok := got[k]; !ok {
				t.Errorf("%s missing: both cases must be emitted, since curl reads lowercase and most runtimes read either", k)
			}
		}
		// And no duplicate assignment for any key: `env -i A=1 A=2` is
		// last-wins, and making a trust boundary depend on that is not a boundary.
		assertNoDuplicateKeys(t, s.execArgv(sandbox.Env{Allow: hostile}, "true", "/workspace"))
	})

	t.Run("allow-all passes the operator's values through", func(t *testing.T) {
		// Under allow-all there is no datapath and no proxy to point at, so
		// fuse has no business rewriting these: the operator's HTTP_PROXY is
		// their own legitimate configuration, and injecting a loopback address
		// nothing listens on would break every command that honours it.
		s := newTestSubstrate(t, fake.NewClientset(), func(o *Options) {
			o.Config.Egress = sandbox.Egress{Mode: sandbox.EgressAllowAll}
		})
		got := assignments(t, s.execArgv(sandbox.Env{Allow: hostile}, "true", "/workspace"))

		for k, want := range hostile {
			if got[k] != want {
				t.Errorf("%s = %q, want the operator's own %q under allow-all", k, got[k], want)
			}
		}
	})

	t.Run("enforce with an EMPTY allowlist still injects the proxy", func(t *testing.T) {
		// ADR-0053's salvaged posture. Deny-all is a PROXY decision, and the
		// datapath must still be there for the proxy to make it — otherwise
		// "everything is denied" and "the proxy is missing" would be
		// indistinguishable to a command, and to whoever is reading the logs.
		s := newTestSubstrate(t, fake.NewClientset(), func(o *Options) {
			enforcing("10.1.2.3")(o)
			o.Config.Egress.Allow = nil
		})
		got := assignments(t, s.execArgv(sandbox.Env{Allow: map[string]string{"PATH": "/bin"}}, "true", "/workspace"))
		if got["HTTP_PROXY"] != "http://127.0.0.1:3128" {
			t.Errorf("HTTP_PROXY = %q under enforce with an empty allowlist, want the loopback proxy: deny-all is a proxy decision, observably distinct from a missing datapath", got["HTTP_PROXY"])
		}
	})
}

// TestExecRendersTheCurrentAllowlist is what makes ResetEnv mean anything on a
// warm Pod.
//
// The container's env is EMPTY by spec, so the environment is whatever this argv
// carries. Two Execs with two different Envs must render two different
// environments — if the substrate cached the first one, a rotated credential
// would linger in every later command for the Pod's whole life.
func TestExecRendersTheCurrentAllowlist(t *testing.T) {
	s := newTestSubstrate(t, fake.NewClientset())

	first := assignments(t, s.execArgv(sandbox.Env{Allow: map[string]string{"TOKEN": "old", "PATH": "/bin"}}, "true", "/workspace"))
	if first["TOKEN"] != "old" {
		t.Fatalf("first render TOKEN = %q, want old", first["TOKEN"])
	}

	second := assignments(t, s.execArgv(sandbox.Env{Allow: map[string]string{"TOKEN": "new", "PATH": "/bin"}}, "true", "/workspace"))
	if second["TOKEN"] != "new" {
		t.Errorf("second render TOKEN = %q, want new — the environment must be rendered from the CURRENT allowlist on every Exec, or ResetEnv is inert", second["TOKEN"])
	}

	// And a key that is gone from the new allowlist must be gone from the
	// environment, not merely overwritten: a revoked passthrough is a variable
	// the command must no longer see at all.
	third := assignments(t, s.execArgv(sandbox.Env{Allow: map[string]string{"PATH": "/bin"}}, "true", "/workspace"))
	if _, present := third["TOKEN"]; present {
		t.Error("TOKEN survived its removal from the allowlist; `env -i` renders the COMPLETE environment, so a removed key must simply not be rendered")
	}
}

// TestExecMissingShellDiagnosis — `env -i` and `/bin/sh` are REQUIREMENTS of the
// image, and their absence must fail the first Exec loudly, naming the image, so
// an operator who pinned a distroless image learns why rather than seeing an
// opaque exec failure. There is deliberately NO fallback: a substrate that
// quietly tried another shell would be running commands under an environment it
// did not render.
func TestExecMissingShellDiagnosis(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "env is not in the image", err: errors.New(`OCI runtime exec failed: exec: "/usr/bin/env": stat /usr/bin/env: no such file or directory`)},
		{name: "sh is not in the image", err: errors.New(`exec: "/bin/sh": stat /bin/sh: no such file or directory: unknown`)},
		{name: "exit 126, the shell could not execute", err: utilexec.CodeExitError{Err: errors.New("command terminated with exit code 126"), Code: 126}},
		{name: "exit 127, the command was not found", err: utilexec.CodeExitError{Err: errors.New("command terminated with exit code 127"), Code: 127}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cs := fake.NewClientset()
			s := newTestSubstrate(t, cs)
			admitWith(t, cs, s, nil)
			s.newExecutor = func(string) (remotecommand.Executor, error) {
				return &stubExecutor{err: tc.err}, nil
			}

			sb, err := s.Provision(context.Background(), loopPrincipal("acme"), sandbox.RemoteSpec{})
			if err != nil {
				t.Fatalf("Provision: %v", err)
			}

			out, err := sb.Exec(context.Background(), sandbox.Env{Allow: map[string]string{}}, "true", "/workspace")
			if err == nil {
				t.Fatalf("Exec must fail loudly when the image lacks env/sh, got Output %+v", out)
			}
			if out.ExitCode == 0 {
				t.Errorf("ExitCode = 0 on a failed Exec; a caller that reads only ExitCode must fail closed")
			}
			// The DIAGNOSTIC is the point: the operator must learn which image is
			// at fault, since the fix is to change the image.
			if !strings.Contains(err.Error(), "alpine:3.20") {
				t.Errorf("the error must name the image so an operator knows what to change; got: %v", err)
			}
			if !strings.Contains(err.Error(), "/bin/sh") && !strings.Contains(err.Error(), "/usr/bin/env") {
				t.Errorf("the error must name the missing requirement; got: %v", err)
			}
		})
	}
}

// TestExecOrdinaryNonZeroExitIsAResult — a command that RAN and exited non-zero
// is a normal Output with a NIL error, exactly as on the container and host
// handlers. Reporting it as a substrate failure would make `grep` finding nothing
// look like a broken sandbox.
func TestExecOrdinaryNonZeroExitIsAResult(t *testing.T) {
	cs := fake.NewClientset()
	s := newTestSubstrate(t, cs)
	admitWith(t, cs, s, nil)
	s.newExecutor = func(string) (remotecommand.Executor, error) {
		return &stubExecutor{
			stdout: "no match\n",
			err:    utilexec.CodeExitError{Err: errors.New("command terminated with exit code 1"), Code: 1},
		}, nil
	}

	sb, err := s.Provision(context.Background(), loopPrincipal("acme"), sandbox.RemoteSpec{})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}

	out, err := sb.Exec(context.Background(), sandbox.Env{Allow: map[string]string{}}, "grep x y", "/workspace")
	if err != nil {
		t.Fatalf("a command that ran and exited 1 is a RESULT, not a substrate failure: %v", err)
	}
	if out.ExitCode != 1 {
		t.Errorf("ExitCode = %d, want 1 (from the exec status)", out.ExitCode)
	}
	if string(out.Combined) != "no match\n" {
		t.Errorf("Combined = %q, want the command's output", out.Combined)
	}
}

// TestExecInterleavesStdoutAndStderr — Combined is both streams, as the bash tool
// has always reported them, so a command whose diagnostic goes to stderr is not
// silently dropped.
func TestExecInterleavesStdoutAndStderr(t *testing.T) {
	cs := fake.NewClientset()
	s := newTestSubstrate(t, cs)
	admitWith(t, cs, s, nil)
	s.newExecutor = func(string) (remotecommand.Executor, error) {
		return &stubExecutor{stdout: "out\n", stderr: "err\n"}, nil
	}

	sb, err := s.Provision(context.Background(), loopPrincipal("acme"), sandbox.RemoteSpec{})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	out, err := sb.Exec(context.Background(), sandbox.Env{Allow: map[string]string{}}, "true", "/workspace")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	combined := string(out.Combined)
	if !strings.Contains(combined, "out") || !strings.Contains(combined, "err") {
		t.Errorf("Combined = %q, want BOTH streams; a diagnostic on stderr must not be dropped", combined)
	}
	if out.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", out.ExitCode)
	}
}

// TestExecTargetsTheWorkloadContainer — the exec URL must name the workload
// container explicitly. Omitting it lets the API server pick the Pod's first
// container, which under enforce is whichever of workload/egress happens to be
// listed first — and running the caller's command in the forwarder sidecar would
// give it the sidecar's TLS client credential.
func TestExecTargetsTheWorkloadContainer(t *testing.T) {
	cs := fake.NewClientset()
	s := newTestSubstrate(t, cs)
	admitWith(t, cs, s, nil)

	var gotURL string
	s.newExecutor = func(u string) (remotecommand.Executor, error) {
		gotURL = u
		return &stubExecutor{}, nil
	}

	sb, err := s.Provision(context.Background(), loopPrincipal("acme"), sandbox.RemoteSpec{})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if _, err := sb.Exec(context.Background(), sandbox.Env{Allow: map[string]string{}}, "true", "/workspace"); err != nil {
		t.Fatalf("Exec: %v", err)
	}

	if !strings.Contains(gotURL, "container="+containerWorkload) {
		t.Errorf("exec URL %q does not name container=%s; the API server would then pick the Pod's FIRST container, which may be the egress sidecar holding the proxy client credential", gotURL, containerWorkload)
	}
	if !strings.Contains(gotURL, "/exec") {
		t.Errorf("exec URL %q does not address the exec subresource", gotURL)
	}
	// stdin is never attached: a sandboxed command has no interactive input, and
	// an attached stdin that nothing writes to leaves commands that read it
	// hanging until the caller's deadline.
	if strings.Contains(gotURL, "stdin=true") {
		t.Errorf("exec URL %q attaches stdin; a sandboxed command has no interactive input and a command that reads it would hang to the deadline", gotURL)
	}
	for _, want := range []string{"stdout=true", "stderr=true"} {
		if !strings.Contains(gotURL, want) {
			t.Errorf("exec URL %q is missing %s", gotURL, want)
		}
	}
	if strings.Contains(gotURL, "tty=true") {
		t.Errorf("exec URL %q requests a TTY, which merges stderr into stdout at the kubelet and loses the distinction", gotURL)
	}
}

// TestExecArgvReachesTheWire — the golden above tests the renderer; this tests
// that the rendered argv is what is actually SENT. A renderer whose output is
// never used is the security-knob-inert failure in miniature.
func TestExecArgvReachesTheWire(t *testing.T) {
	cs := fake.NewClientset()
	s := newTestSubstrate(t, cs)
	admitWith(t, cs, s, nil)

	var gotURL string
	s.newExecutor = func(u string) (remotecommand.Executor, error) {
		gotURL = u
		return &stubExecutor{}, nil
	}

	sb, err := s.Provision(context.Background(), loopPrincipal("acme"), sandbox.RemoteSpec{})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	env := sandbox.Env{Allow: map[string]string{"PATH": "/bin", "TOKEN": "secret-value"}}
	if _, err := sb.Exec(context.Background(), env, "echo hi", "/workspace/sub"); err != nil {
		t.Fatalf("Exec: %v", err)
	}

	// The URL carries each argv element as a repeated `command=` query parameter,
	// each percent-escaped — which is what makes a command with spaces, newlines
	// or quotes survive the wire unchanged.
	for _, want := range []string{
		"command=-i",
		"command=PATH%3D%2Fbin",
		"command=TOKEN%3Dsecret-value",
		"command=echo+hi",
		"command=%2Fworkspace%2Fsub",
	} {
		if !strings.Contains(gotURL, want) {
			t.Errorf("exec URL does not carry %q; the rendered argv is not what reaches the wire.\nURL: %s", want, gotURL)
		}
	}
}

// assignments pulls the K=V pairs out of a rendered argv, so a test can assert
// about the environment without restating the whole golden.
func assignments(t *testing.T, argv []string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for i, tok := range argv {
		if i < 2 {
			continue // /usr/bin/env -i
		}
		if tok == "/bin/sh" {
			break
		}
		k, v, ok := strings.Cut(tok, "=")
		if !ok {
			t.Fatalf("argv element %q before /bin/sh is not an assignment", tok)
		}
		out[k] = v
	}
	return out
}

func assertNoDuplicateKeys(t *testing.T, argv []string) {
	t.Helper()
	seen := map[string]bool{}
	for i, tok := range argv {
		if i < 2 {
			continue
		}
		if tok == "/bin/sh" {
			break
		}
		k, _, _ := strings.Cut(tok, "=")
		if seen[k] {
			t.Errorf("argv assigns %q twice; `env -i A=1 A=2` is last-wins, and a trust boundary must not depend on that", k)
		}
		seen[k] = true
	}
}

// stubExecutor is a remotecommand.Executor double. It writes fixed output and
// reports a fixed error, so the whole Exec path — argv, URL, stream wiring, exit
// classification — is testable with no cluster and no exec protocol.
type stubExecutor struct {
	stdout string
	stderr string
	err    error
}

func (e *stubExecutor) Stream(opts remotecommand.StreamOptions) error {
	return e.StreamWithContext(context.Background(), opts)
}

func (e *stubExecutor) StreamWithContext(_ context.Context, opts remotecommand.StreamOptions) error {
	if opts.Stdout != nil && e.stdout != "" {
		if _, err := fmt.Fprint(opts.Stdout, e.stdout); err != nil {
			return err
		}
	}
	if opts.Stderr != nil && e.stderr != "" {
		if _, err := fmt.Fprint(opts.Stderr, e.stderr); err != nil {
			return err
		}
	}
	return e.err
}
