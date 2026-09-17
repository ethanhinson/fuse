package kubernetes

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	neturl "net/url"
	"sort"
	"strings"
	"sync"

	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/client-go/tools/remotecommand"
	clientgoexec "k8s.io/client-go/util/exec"
	utilexec "k8s.io/utils/exec"

	"github.com/ethanhinson/fuse/internal/tools/sandbox"
)

// The in-sandbox egress datapath addresses (spec §5), fixed here and matching the
// container handler's own constants. They only have to agree with each other and
// with the sidecar's -listen, and both live in this repo.
const (
	// podEgressListen is where the forwarder sidecar listens, on the Pod's
	// loopback. A sidecar shares the Pod's network namespace, so 127.0.0.1 inside
	// the workload container IS the sidecar — which is the whole reason the
	// ADR-0051 "sidecar sharing the netns" escape hatch is the natural k8s shape.
	podEgressListen = "127.0.0.1:3128"
	// podEgressProxyURL is what the injected *_PROXY variables carry. `http://`
	// because the proxy speaks HTTP CONNECT, which is what curl/git/pip do with
	// an http-scheme proxy for BOTH http and https destinations.
	podEgressProxyURL = "http://" + podEgressListen
)

// execScript is the inner shell program, and its exact text is a security
// property.
//
// THE WORKING DIRECTORY AND THE COMMAND ARE POSITIONAL PARAMETERS, never
// interpolated. A workingDir spliced into this text — even one that
// resolveWorkspace already contained — could contain a quote, close the quoting,
// and append arbitrary shell. `$1` and `$2` are values the shell has already
// finished parsing and cannot re-parse.
//
// `cd --` ends option parsing, so a directory literally named "-rf" is a path and
// not a flag to cd.
//
// `&&` and NOT `;`: a failed cd must not fall through to running the command in
// whatever directory the shell happened to start in. That would run the caller's
// command somewhere they did not ask for, which is precisely the refusal
// containRemoteWorkingDir makes on the containment side — it would be perverse to
// undo it here. On this substrate the `&&` carries MORE weight than on the local
// one: the adapter's check is lexical, so this cd is the only step that resolves
// the path against the filesystem that will actually run the command.
//
// `exec` so the outer shell replaces itself: one fewer process between the
// caller's deadline and the command, and the command's own exit status is what
// the exec subresource reports.
const execScript = `cd -- "$1" && exec /bin/sh -c "$2"`

// Paths the workload image MUST provide. They are not negotiable and there is no
// fallback: a substrate that quietly tried another shell would be running
// commands under an environment it did not render.
const (
	execEnvPath   = "/usr/bin/env"
	execShellPath = "/bin/sh"
)

// execArgv renders the complete exec command for one Exec.
//
// # Why the environment is rendered HERE and not baked into the Pod
//
// The workload container's `env` is EMPTY by spec (§3). Every variable the
// command observes comes from this argv, built from the Runner's CURRENT
// allowlist on every single Exec. Three things follow, and all three are the
// point:
//
//   - #63's scrub invariant survives on a warm sandbox. `env -i` makes the
//     environment COMPLETE rather than additive, so there is no inheritance path
//     from the image, from the kubelet, or from a Service link.
//   - ResetEnv MEANS something. A Pod's container environment cannot be changed
//     after creation, so a baked variable would survive every reset for the Pod's
//     whole life — a rotated credential lingering in every later command. The
//     Pool's reset-on-checkout stores the fresh allowlist on the Runner and this
//     function renders it.
//   - A key REMOVED from the allowlist is simply not rendered, so a revoked
//     passthrough is a variable the command no longer sees at all rather than one
//     it sees an overwritten value for.
//
// The argv is deterministic (assignments sorted) because it is golden-tested, and
// it is golden-tested because every token in it is a decision.
func (s *Substrate) execArgv(env sandbox.Env, cmd, workingDir string) []string {
	argv := []string{execEnvPath, "-i"}
	argv = append(argv, s.renderExecEnv(env)...)
	argv = append(argv,
		execShellPath, "-c", execScript,
		// $0 for the inner shell. It is only ever a diagnostic name, but the
		// positional parameters below are shifted by it, so it must be present.
		"sh",
		workingDir, // $1
		cmd,        // $2
	)
	return argv
}

// renderExecEnv resolves the allowlist into sorted `K=V` assignments, applying
// the enforce-mode proxy ownership.
//
// Under EgressEnforce the eight proxy keys are STRIPPED from the operator's
// passthrough and RE-INJECTED pointing at the Pod's loopback forwarder, exactly
// as the container handler does. The two halves are one decision: without the
// strip, an env_passthrough naming HTTP_PROXY could redirect egress at a host of
// the operator's choosing, and one naming NO_PROXY could exempt destinations from
// it. A variable this package injects is a variable this package owns.
//
// The strip runs BEFORE the injection so no duplicate key is ever rendered.
// `env -i A=1 A=2` is last-wins, and making a trust boundary depend on an
// argument-order rule is not a boundary.
//
// Under EgressAllowAll the operator's own values pass through untouched: there is
// no proxy to point at, and injecting a loopback address nothing listens on would
// break every command that honours it.
func (s *Substrate) renderExecEnv(env sandbox.Env) []string {
	enforce := s.egress.Mode == sandbox.EgressEnforce

	out := make([]string, 0, len(env.Allow)+len(proxyEnvKeys))
	for k, v := range env.Allow {
		if enforce && isProxyEnvKey(k) {
			continue
		}
		out = append(out, k+"="+v)
	}
	if enforce {
		// Emitted under enforce whether or not an allowlist was declared. An
		// EMPTY allowlist is the deny-all state (ADR-0053's salvaged posture) and
		// is a legitimate resolved outcome: deny-all is a PROXY decision, and the
		// datapath has to be there for the proxy to make it, or "everything is
		// denied" and "the datapath is missing" would be indistinguishable to a
		// command and to whoever reads the logs.
		out = append(out, egressProxyEnv()...)
	}

	// Sorted, so argv is deterministic and golden-testable. The sandbox package's
	// renderEnv sorts for exactly this reason.
	sort.Strings(out)
	return out
}

// proxyEnvKeys are every variable that decides whether — and through what — a
// process in the Pod uses a proxy. It mirrors the container handler's list
// deliberately: the same eight keys are the trusted side's on both substrates, and
// a list that drifted between them would be a hole on whichever one was forgotten.
var proxyEnvKeys = [...]string{
	"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY",
	"http_proxy", "https_proxy", "all_proxy", "no_proxy",
}

func isProxyEnvKey(key string) bool {
	for _, k := range proxyEnvKeys {
		if key == k {
			return true
		}
	}
	return false
}

// egressProxyEnv is the injected proxy environment.
//
// BOTH cases are emitted because the ecosystem is split and the split is not
// principled: curl reads lowercase and deliberately ignores uppercase HTTP_PROXY
// (the CGI `Proxy:` header attack), while Go's net/http and most language
// runtimes read either. Emitting one case would leave the other free for an
// image-baked value to occupy.
//
// NO_PROXY is set EXPLICITLY EMPTY rather than omitted. Omitting it leaves
// whatever the image or a base layer baked in, and a single `NO_PROXY=*` is a
// complete bypass of everything above it.
func egressProxyEnv() []string {
	return []string{
		"HTTP_PROXY=" + podEgressProxyURL,
		"HTTPS_PROXY=" + podEgressProxyURL,
		"ALL_PROXY=" + podEgressProxyURL,
		"NO_PROXY=",
		"http_proxy=" + podEgressProxyURL,
		"https_proxy=" + podEgressProxyURL,
		"all_proxy=" + podEgressProxyURL,
		"no_proxy=",
	}
}

// Exec runs cmd in the Pod's workload container.
//
// workingDir arrives ALREADY CONTAINMENT-CHECKED by the adapter (through
// containRemoteWorkingDir against MountRoot) and is an in-Pod absolute path.
// Nothing here re-derives it, and nothing here consults anything the model
// supplied beyond cmd itself.
//
// That adapter check is LEXICAL, because fuse has no access to the Pod's
// filesystem. execScript's `cd -- "$1" && ...` is therefore the REAL resolver,
// and its `&&` is load-bearing for containment and not only for tidiness: a cd
// that fails must fail the command, never fall through to running it in whatever
// directory the shell started in.
//
// Error semantics match the container and host handlers exactly, because the bash
// tool cannot tell which substrate it is on: a command that RAN and exited
// non-zero is a normal Output with a NIL error, and a non-nil error means the
// SUBSTRATE could not start the command at all. ExitCode is never left 0 on a
// failure path, so a caller reading only ExitCode still fails closed.
func (p *sandboxPod) Exec(ctx context.Context, env sandbox.Env, cmd, workingDir string) (sandbox.Output, error) {
	s := p.s
	argv := s.execArgv(env, cmd, workingDir)

	// THE EXEC URL IS BUILT HERE rather than through the clientset's REST client,
	// for a reason worth stating: the generated fake clientset returns a nil REST
	// client, so routing through it would make the entire exec path — argv, target
	// container, stream wiring, exit classification — unreachable without a live
	// cluster. Building the URL directly keeps all of that testable AND keeps the
	// URL itself an assertable value, which is what TestExecTargetsTheWorkload
	// Container and TestExecArgvReachesTheWire read.
	//
	// The shape is the API server's documented exec subresource path, and the
	// query parameters are the ones PodExecOptions would have encoded.
	u := s.execURL(p.namespace, p.name, argv)

	exec, err := s.executorFor(u)
	if err != nil {
		return sandbox.Output{ExitCode: -1}, fmt.Errorf("kubernetes: exec into %s: %w", p.ID(), err)
	}

	// One buffer for both streams, guarded because the exec protocol writes them
	// from separate goroutines. Interleaved into Output.Combined as the bash tool
	// has always reported them, so a command whose diagnostic goes to stderr is
	// not silently dropped.
	var buf syncBuffer
	streamErr := exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &buf,
		Stderr: &buf,
	})

	out := sandbox.Output{Combined: buf.Bytes()}

	if streamErr == nil {
		return out, nil
	}

	// A command that RAN and exited non-zero comes back as a CodeExitError. That
	// is a RESULT, not a substrate failure: reporting it as one would make `grep`
	// finding nothing look like a broken sandbox.
	if code, ok := exitCodeOf(streamErr); ok {
		if diag := s.diagnoseMissingShell(code, buf.String()); diag != "" {
			out.ExitCode = -1
			return out, fmt.Errorf("kubernetes: exec into %s: %s", p.ID(), diag)
		}
		out.ExitCode = code
		return out, nil
	}

	// Anything else is the substrate failing to start the command. A missing
	// `env` or `/bin/sh` usually lands here, as a transport-level exec failure.
	out.ExitCode = -1
	if diag := s.diagnoseMissingShell(-1, streamErr.Error()); diag != "" {
		return out, fmt.Errorf("kubernetes: exec into %s: %s: %w", p.ID(), diag, streamErr)
	}
	return out, fmt.Errorf("kubernetes: exec into %s: %w", p.ID(), streamErr)
}

// diagnoseMissingShell recognises the ONE image-configuration failure an operator
// can actually fix, and names it.
//
// `env -i` and `/bin/sh` are REQUIREMENTS of the workload image (spec §3). There
// is deliberately no fallback: a substrate that quietly tried another shell would
// be running commands under an environment it did not render, which is the
// invariant `env -i` exists to hold. So the only useful thing to do is fail the
// first Exec loudly, NAMING THE IMAGE — because the fix is to change the image,
// and an operator who pinned a distroless one has no other way to learn that.
//
// Exit 126 ("found but not executable") and 127 ("not found") are included
// because a kubelet that resolves the binary and then fails to run it reports
// through the exit status rather than through a transport error. It returns ""
// for anything it does not recognise, so an ordinary command's exit 127 — a typo'd
// command name inside the shell — is NOT misreported as a broken image: that case
// has output and a shell diagnostic, which is what the text match keys on.
func (s *Substrate) diagnoseMissingShell(code int, text string) string {
	missing := ""
	switch {
	case strings.Contains(text, execEnvPath):
		missing = execEnvPath
	case strings.Contains(text, execShellPath):
		missing = execShellPath
	case code == 126 || code == 127:
		// The kubelet could not execute what fuse named, and the only two things
		// fuse names are these.
		missing = execEnvPath + " or " + execShellPath
	default:
		return ""
	}
	return fmt.Sprintf("the workload image %q must provide %s and %s; %s appears to be missing (there is no fallback shell: the complete environment is rendered as `env -i` argv, and another shell would run the command under an environment fuse did not render)",
		s.image, execEnvPath, execShellPath, missing)
}

// executorFor builds the exec transport, honouring the test seam.
func (s *Substrate) executorFor(url string) (remotecommand.Executor, error) {
	if s.newExecutor != nil {
		return s.newExecutor(url)
	}
	return s.newRemoteExecutor(url)
}

// newRemoteExecutor is the real transport: WEBSOCKET with an SPDY FALLBACK.
//
// WebSocket first because it is the protocol current API servers prefer and the
// only one that carries the v5 CLOSE signal correctly. SPDY second because
// clusters behind an older proxy still only speak it, and a substrate that worked
// on one cluster and not another for transport reasons would look like a fuse bug.
// NewFallbackExecutor's predicate decides per-connection which one is usable.
func (s *Substrate) newRemoteExecutor(url string) (remotecommand.Executor, error) {
	if s.rest == nil {
		// Only reachable for a substrate built over a bare clientset with no REST
		// config — the test constructor's shape. Failing closed here rather than
		// nil-dereferencing inside client-go.
		return nil, errors.New("kubernetes: no REST config, so no exec transport (tests must inject newExecutor)")
	}
	parsed, err := neturl.Parse(url)
	if err != nil {
		return nil, fmt.Errorf("kubernetes: exec url: %w", err)
	}
	ws, err := remotecommand.NewWebSocketExecutor(s.rest, "GET", url)
	if err != nil {
		return nil, fmt.Errorf("kubernetes: websocket exec transport: %w", err)
	}
	spdy, err := remotecommand.NewSPDYExecutor(s.rest, "POST", parsed)
	if err != nil {
		return nil, fmt.Errorf("kubernetes: spdy exec transport: %w", err)
	}
	return remotecommand.NewFallbackExecutor(ws, spdy, httpstream.IsUpgradeFailure)
}

// syncBuffer is a bytes.Buffer safe for the two writers the exec protocol uses.
//
// The mutex is not defensive: stdout and stderr are written from separate
// goroutines inside client-go, so an unguarded buffer here is a data race that
// -race reports and that in production corrupts output
// (learning: mutex-test-double-concurrent-provider, applied to real code).
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	// A copy: the caller keeps this in an Output that outlives the lock.
	return append([]byte(nil), b.buf.Bytes()...)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// execURL is the API server's exec-subresource URL for one command.
//
// Every query parameter here is a decision:
//
//   - container=workload, ALWAYS and BY NAME. Omitting it lets the API server pick
//     the Pod's FIRST container, which under enforce is whichever of
//     workload/egress is listed first — and running the caller's command in the
//     forwarder sidecar would hand it the sidecar's TLS client credential.
//   - stdin is NOT attached. A sandboxed command has no interactive input, and an
//     attached stdin nothing writes to leaves a command that reads it hanging
//     until the caller's deadline.
//   - stdout and stderr both, so Combined carries a diagnostic written to stderr.
//   - tty=false. A TTY merges stderr into stdout at the kubelet, which loses the
//     distinction upstream of the interleaving fuse does itself.
//
// Each argv element is one repeated `command=` parameter. url.Values escapes each
// of them, which is what makes a command containing spaces, newlines or quotes
// survive the wire unchanged — the counterpart of the positional-parameter
// argument in execScript.
func (s *Substrate) execURL(namespace, pod string, argv []string) string {
	q := neturl.Values{}
	q.Set("container", containerWorkload)
	q.Set("stdout", "true")
	q.Set("stderr", "true")
	q.Set("stdin", "false")
	q.Set("tty", "false")
	for _, a := range argv {
		q.Add("command", a)
	}

	base := strings.TrimSuffix(s.apiHost, "/")
	return fmt.Sprintf("%s/api/v1/namespaces/%s/pods/%s/exec?%s",
		base, neturl.PathEscape(namespace), neturl.PathEscape(pod), q.Encode())
}

// exitCodeOf reports the exit status of a command that RAN, distinguishing it from
// a transport failure in which nothing ran at all.
//
// # Why this is a function and not two lines of errors.As
//
// There are TWO unrelated types named CodeExitError in the Kubernetes module
// graph — k8s.io/client-go/util/exec and k8s.io/utils/exec — with identical
// shapes, identical names, and identical %T renderings ("exec.CodeExitError").
// client-go's remotecommand streams return the FIRST one; this package originally
// matched only the second, so errors.As silently returned false for every real
// exit code and every legitimately-failing command was reported as a broken
// substrate. Worse, Verify's leg 2 — whose failure to connect IS the pass
// condition — read as "could not be probed" and disqualified every cluster,
// including correctly-enforcing ones.
//
// Nothing about the types makes that visible at a call site, and a hand-rolled
// test double fabricating either one proves nothing about which the wire produces
// (both existing tables used the wrong one and were green). So the match lives
// HERE, once, checking both, with the client-go one first because it is what the
// transport actually returns — and TestExitCodeClassificationUsesTheTransportsOwn
// ErrorType drives both through it.
//
// The false return is load-bearing and must not be widened: a transport failure
// carries no evidence about the command, and reporting it as exit 0 would make a
// broken datapath look like a passing canary.
func exitCodeOf(err error) (int, bool) {
	if err == nil {
		return 0, false
	}
	// The one the wire returns.
	var cg clientgoexec.CodeExitError
	if errors.As(err, &cg) {
		return cg.Code, true
	}
	// Kept so a client-go switch to the shared package cannot silently reintroduce
	// the defect this function exists to close.
	var ut utilexec.CodeExitError
	if errors.As(err, &ut) {
		return ut.Code, true
	}
	return 0, false
}
