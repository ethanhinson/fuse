package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/ethanhinson/fuse/internal/loopauth"
)

// DefaultContainerImage is the image used when the operator configured none.
//
// It is deliberately minimal (a shell, coreutils, and nothing else) and it is
// PINNED to a tag rather than floating on :latest, so that what a sandboxed
// command can reach does not change underneath an operator between two runs.
const DefaultContainerImage = "alpine:3.20"

// containerWorkspace is the fixed in-container mount point and working
// directory. It is a constant rather than a mirror of the host path so that
// nothing about the host's directory layout is disclosed to a command, and so
// argv is identical regardless of where the repo happens to live.
const containerWorkspace = "/workspace"

// containerCLIs are the supported OCI CLIs, in PREFERENCE ORDER. The order is
// load-bearing and deliberate: docker is the overwhelmingly common case, and
// nerdctl and podman are both argv-compatible with the subset used here
// (`run --rm -i --env -v -w`), which is why one argv builder serves all three.
var containerCLIs = [...]string{"docker", "nerdctl", "podman"}

// containerClientPassthrough are the variables the CLI BINARY needs to find and
// address a daemon. They are added on top of the base allowlist for the client
// process only, and never reach the container: the sandboxed command's
// environment is exactly what the caller resolved, passed as --env K=V.
var containerClientPassthrough = [...]string{
	"DOCKER_HOST",        // docker daemon socket / remote endpoint
	"DOCKER_CONFIG",      // docker client config dir (registry auth)
	"DOCKER_CONTEXT",     // selected docker context
	"CONTAINER_HOST",     // podman remote endpoint
	"CONTAINERD_ADDRESS", // nerdctl containerd socket
	"XDG_RUNTIME_DIR",    // rootless socket discovery for podman/nerdctl
}

// ErrNoContainerRuntime reports that none of the supported container CLIs is on
// PATH, so the container substrate cannot be constructed.
//
// This is returned rather than quietly degrading to the host handler. A silent
// fallback would mean that uninstalling docker turns containment off, which is
// exactly the fail-open direction ADR-0044 forbids. Selection (T5) decides what
// to do with this error; it is never this package's job to answer it by running
// the command somewhere less safe.
var ErrNoContainerRuntime = errors.New("no container runtime available")

// ErrWorkingDirRefused reports that a model-supplied working_dir did not
// resolve to a directory INSIDE the trusted mount.
//
// ADR-0044 requires that "the model-supplied `working_dir` resolves *within*
// the mount and cannot escape it", and that the root of trust "comes from the
// authenticated loop-start context, never from model output (not the `command`,
// not `working_dir`)". Letting working_dir choose the bind-mount SOURCE would
// invert that: a model naming "/" or an operator's home directory would mount
// that subtree into a container it controls, recovering by filesystem exactly
// the credential access the env-scrub closed. So working_dir is a SUBPATH
// request against a mount it cannot influence, and a request that does not
// resolve inside the mount is refused before anything is executed.
var ErrWorkingDirRefused = errors.New("working_dir must resolve inside the sandbox workspace")

// ErrNoTrustedRoot reports that no trusted mount root was supplied, so there is
// no workspace for a working_dir to be relative TO.
//
// This is the degraded state a composition root reaches only when it could not
// resolve a repo root at all (see cmd/fuse's os.Getwd fallback, which also
// makes LoadConfig warn). The safe answer is to mount nothing and refuse the
// working_dir — never to promote the model's path to a mount source because no
// trusted one was available.
var ErrNoTrustedRoot = errors.New("no trusted workspace root is mounted")

// execRunner runs one command to completion and reports its combined output.
//
// It is injected so that argv construction — the part of this handler that
// carries the security-relevant decisions — is unit-testable with no container
// daemon anywhere in sight.
//
// Its contract mirrors the substrate contract one level down: the returned
// error is non-nil ONLY when the command could not be started at all. A command
// that started and exited non-zero returns its code with a nil error, and the
// implementation must never report a zero code alongside a non-nil error.
type execRunner func(ctx context.Context, name string, args ...string) ([]byte, int, error)

// containerOption configures a containerHandler at construction.
type containerOption func(*containerHandler)

// withLookPath overrides PATH probing (tests, and any caller that must resolve
// against something other than the ambient PATH).
func withLookPath(fn func(string) (string, error)) containerOption {
	return func(h *containerHandler) {
		if fn != nil {
			h.lookPath = fn
		}
	}
}

// withExecRunner overrides how the CLI is invoked.
func withExecRunner(fn execRunner) containerOption {
	return func(h *containerHandler) {
		if fn != nil {
			h.run = fn
		}
	}
}

// withTrustedRoot sets the host directory this handler bind-mounts.
//
// SECURITY-CRITICAL: the value comes from the COMPOSITION ROOT — the repo root
// resolved at startup, before any model has run (see Service.root and
// NewServiceFromRoot). It must never be derived from a tool argument, a wire
// field, or model output. It is applied once, at construction; there is no
// method that can change it afterwards.
func withTrustedRoot(root string) containerOption {
	return func(h *containerHandler) { h.root = root }
}

// containerHandler runs commands inside a throwaway OCI container.
//
// It is the DEFAULT substrate. Detection happens once, at construction, so that
// the answer to "is containment available" is settled before any command is
// eligible to run — never mid-Exec, where the only remaining options would be
// to fail late or to fall back.
type containerHandler struct {
	// runtime is the detected CLI name ("docker"|"nerdctl"|"podman"). It is a
	// bounded value drawn from containerCLIs, safe as an event/metric label.
	runtime string
	// image is the resolved image reference (config, or the pinned default).
	image string

	// root is the TRUSTED bind-mount source: the one host directory any
	// command run through this handler can see, mounted at containerWorkspace.
	// It is resolved once at construction from the composition root's repo root
	// and is never model-derived. "" means no trusted root was supplied, which
	// is the degraded-but-safe state: nothing is mounted and any model-supplied
	// working_dir is refused rather than becoming a mount source itself.
	root string

	lookPath func(string) (string, error)
	run      execRunner
}

// newContainerHandler detects a container CLI and returns the handler bound to
// it. It returns a nil handler and an ErrNoContainerRuntime-wrapping error when
// none of the supported CLIs is available: a nil handler on the error path is
// what makes "use it anyway" unwritable at the call site.
func newContainerHandler(cfg Config, opts ...containerOption) (*containerHandler, error) {
	h := &containerHandler{
		image:    strings.TrimSpace(cfg.Image),
		lookPath: exec.LookPath,
		run:      runCommand,
	}
	if h.image == "" {
		h.image = DefaultContainerImage
	}
	for _, opt := range opts {
		opt(h)
	}
	// Resolved ONCE, here, at construction — before any model has run — so that
	// the mount source is a settled fact for the life of the handler rather
	// than something re-derived per Exec from whatever the filesystem looks
	// like by then.
	h.root = resolveMountRoot(h.root)

	for _, cli := range containerCLIs {
		if _, err := h.lookPath(cli); err == nil {
			h.runtime = cli
			return h, nil
		}
	}
	return nil, fmt.Errorf("%w: looked for %s on PATH", ErrNoContainerRuntime, strings.Join(containerCLIs[:], ", "))
}

// resolveMountRoot canonicalises the trusted mount source.
//
// It returns "" — meaning "mount nothing" — for anything it cannot prove is an
// existing directory, because a bind-mount source the daemon has to invent is
// not a working tree; docker would silently create it (as root), and the model
// would get an empty workspace that LOOKS mounted. Symlinks are resolved here
// so that the later working_dir containment check compares two canonical paths:
// a root reached through a symlink would make an in-tree path look like an
// escape, and — worse — a root and a candidate canonicalised differently could
// make an escape look in-tree.
func resolveMountRoot(root string) string {
	root = strings.TrimSpace(root)
	if root == "" {
		return ""
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return ""
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return ""
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return ""
	}
	return resolved
}

// Name reports the bounded handler identifier.
func (*containerHandler) Name() string { return HandlerContainer }

// Runtime reports which container CLI was detected at construction.
func (h *containerHandler) Runtime() string { return h.runtime }

// Acquire returns a Runner bound to p and to env.
//
// The environment is rendered here, once, from the allowlist the caller
// resolved. The Runner never consults the process environment afterwards, which
// is what makes it structurally impossible for an ambient variable to reach a
// container.
func (h *containerHandler) Acquire(_ context.Context, p loopauth.Principal, env Env) (Runner, error) {
	return &containerRunner{
		handler:   h,
		principal: p,
		env:       renderEnv(env),
	}, nil
}

// containerRunner is one principal's container execution context.
type containerRunner struct {
	handler *containerHandler
	// principal is fixed at Acquire and never reassigned: a Runner belongs to
	// exactly one principal for its whole life, which is what lets the warm
	// pool re-assert ownership on checkout (see acquiredFor).
	principal loopauth.Principal

	// mu guards env. A pooled Runner is re-environed on checkout (ResetEnv)
	// while a previous Exec may still be unwinding, so the environment is not
	// write-once even though it is set-once per checkout.
	mu sync.Mutex
	// env is the COMPLETE environment, pre-rendered as sorted K=V strings by
	// renderEnv. Sorting is what makes argv deterministic and golden-testable.
	env []string
}

// acquiredFor reports the principal this Runner was acquired for, so a pool can
// verify — against the Runner itself, not only against its own bookkeeping —
// that it is about to hand the right context to the right principal.
func (r *containerRunner) acquiredFor() loopauth.Principal { return r.principal }

// ResetEnv re-applies a freshly resolved environment to a warm Runner.
//
// It exists for the pool's reset-on-checkout: a Runner that outlives one
// checkout must observe the CURRENT allowlist on the next, not a snapshot taken
// when it was first acquired, or a rotated credential would linger in argv.
func (r *containerRunner) ResetEnv(env Env) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.env = renderEnv(env)
	return nil
}

// currentEnv snapshots the rendered environment. It never returns nil.
func (r *containerRunner) currentEnv() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.env == nil {
		return []string{}
	}
	return append([]string(nil), r.env...)
}

// workspace resolves the bind-mount SOURCE and the in-container working
// directory for one Exec.
//
// This is where ADR-0044's containment constraint is enforced, and the split it
// makes is the whole point:
//
//   - The mount source is ALWAYS the handler's trusted root. It is not a
//     function of workingDir, and there is no branch on which model output can
//     reach it. This is what makes "mount my home directory" unwritable.
//   - The model-supplied workingDir is a SUBPATH REQUEST resolved against that
//     root. It moves -w and nothing else, so the worst a hostile value can do
//     is name a directory the container was already going to be able to see.
//   - Anything that does not resolve inside the root is REFUSED, not clamped.
//     Silently rewriting an escape to the root would run a command somewhere
//     the caller did not ask for, which is how "cd /etc && rm -rf ." becomes a
//     surprise in the repo.
//
// Note that the returned workdir is a CONTAINER path. The runtime resolves it
// inside the container's own namespace, so a symlink the model plants in the
// tree after this check can only redirect -w to another path inside the mount —
// there is no TOCTOU window between here and the mount that reaches the host.
// The host-side canonicalisation below is what closes the window that DOES
// exist: a symlink already in the tree pointing out of it.
//
// Per-tenant subdivision of the root is #0065's job; this function only settles
// which root is mounted and that workingDir cannot escape it.
func (h *containerHandler) workspace(workingDir string) (mount string, workdir string, err error) {
	workingDir = strings.TrimSpace(workingDir)

	if h.root == "" {
		if workingDir != "" {
			// The one thing we must not do here is fall back to mounting the
			// model's path because we have no trusted one.
			return "", "", fmt.Errorf("%w: cannot place working_dir %q", ErrNoTrustedRoot, workingDir)
		}
		return "", containerWorkspace, nil
	}

	// THE DEFAULT, and the common case: no working_dir at all still mounts the
	// working tree. An unmounted container is an empty box the agent cannot
	// work in (ADR-0044: "The working tree must be mounted in for the model to
	// see the repo it edits").
	if workingDir == "" {
		return h.root, containerWorkspace, nil
	}

	// A relative working_dir is relative to the workspace — the only root the
	// model has any business naming a path against.
	candidate := workingDir
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(h.root, candidate)
	}

	// Canonicalise before comparing. A prefix test against an uncanonicalised
	// path is defeated by "..", by a doubled separator, and by a symlink; both
	// sides here are fully resolved, so the comparison is between real paths.
	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		// Includes "does not exist", which is a refusal rather than a silent
		// fallback to the root: the caller asked to run somewhere specific.
		// The host path is deliberately NOT echoed back — the container never
		// discloses this host's directory layout (see containerWorkspace).
		return "", "", fmt.Errorf("%w: %q could not be resolved", ErrWorkingDirRefused, workingDir)
	}

	rel, err := filepath.Rel(h.root, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", "", fmt.Errorf("%w: %q escapes it", ErrWorkingDirRefused, workingDir)
	}
	if info, err := os.Stat(resolved); err != nil || !info.IsDir() {
		// In-tree but not a directory. Refused here so the caller gets the
		// reason, rather than at the daemon, where it surfaces as an opaque
		// runtime failure of a container that was already created.
		return "", "", fmt.Errorf("%w: %q is not a directory", ErrWorkingDirRefused, workingDir)
	}
	if rel == "." {
		return h.root, containerWorkspace, nil
	}
	return h.root, containerWorkspace + "/" + filepath.ToSlash(rel), nil
}

// argv builds the exact command line for one Exec.
//
// It is separated from Exec because argv IS the security boundary of this
// handler: every containment property (the scrubbed environment, the mount, the
// workdir) is expressed here and nowhere else, so it must be assertable without
// running anything.
//
// It returns an error rather than a best-effort command line: a working_dir
// that cannot be contained is a REFUSAL, and a refusal must be unable to
// produce argv at all, so there is nothing for Exec to accidentally run.
func (r *containerRunner) argv(cmd string, workingDir string) ([]string, error) {
	mount, workdir, err := r.handler.workspace(workingDir)
	if err != nil {
		return nil, err
	}

	env := r.currentEnv()

	args := make([]string, 0, 12+2*len(env))
	args = append(args,
		"run",
		// --rm: the container is torn down when the command exits, so no
		// state — including anything the command wrote outside the mount —
		// survives into the next principal's execution.
		"--rm",
		// -i: stdin stays attached, which the bash tool relies on for
		// commands that read input. No -t: there is no terminal here, and
		// allocating one would corrupt the combined-output capture.
		"-i",
	)

	// TODO(#0064): egress control owns this flag (--network none floor + allowlist)

	for _, kv := range env {
		// ALWAYS the `--env K=V` pair form, NEVER a bare `--env K`.
		//
		// A bare `--env K` tells docker/nerdctl/podman to copy the HOST's
		// current value of K into the container. That would re-open the exact
		// ambient-inheritance hole this package exists to close, and it would
		// do so invisibly: argv would still look like it named only allowed
		// keys. renderEnv already produced K=V strings; pass them whole.
		args = append(args, "--env", kv)
	}

	// TODO(#0065): per-TENANT subdivision of this mount (Principal.Tenant).
	//
	// What is NOT deferred, and must stay here: the source is the trusted root
	// and only ever the trusted root. mount is "" only when no trusted root was
	// resolved at all, in which case nothing is mounted — never a substitute
	// derived from the command's arguments.
	if mount != "" {
		args = append(args, "-v", mount+":"+containerWorkspace)
	}
	args = append(args, "-w", workdir)

	args = append(args, r.handler.image, "/bin/sh", "-c", cmd)
	return args, nil
}

// Exec runs cmd inside a fresh container.
//
// Error semantics are identical to the host handler's, by contract: a command
// that RAN and exited non-zero, or that was killed by the context deadline, is
// a normal result reported through Output (ExitCode / TimedOut) with a nil
// error. A non-nil error means the SUBSTRATE could not start the command at
// all, and ExitCode is -1 on that path so a caller that reads only ExitCode
// still fails closed. ExitCode is never left 0 on any failure path.
func (r *containerRunner) Exec(ctx context.Context, cmd string, workingDir string) (Output, error) {
	args, err := r.argv(cmd, workingDir)
	if err != nil {
		// A containment refusal, decided BEFORE the CLI is touched: no
		// container is created, no mount is established, nothing runs. It is
		// reported as a substrate failure (ExitCode -1, non-nil error) because
		// that is exactly what it is — the command could not be started — and
		// because an ExitCode-only caller must fail closed on it.
		return Output{ExitCode: -1}, err
	}

	combined, code, err := r.handler.run(ctx, r.handler.runtime, args...)

	out := Output{
		Combined: combined,
		ExitCode: code,
		// Checked on the caller's context: the CLI is killed when the deadline
		// fires, and the resulting status is indistinguishable from any other
		// signal death without this.
		TimedOut: errors.Is(ctx.Err(), context.DeadlineExceeded),
	}

	if err != nil {
		// Could not start: no daemon, an unpullable image, a rejected mount.
		out.ExitCode = -1
		return out, fmt.Errorf("%s: %w", r.handler.runtime, err)
	}
	if out.TimedOut && out.ExitCode == 0 {
		// A killed command reporting success is not a result we will pass on.
		out.ExitCode = -1
	}
	return out, nil
}

// Release is a no-op. `run --rm` already tears the container down when the
// command exits, so there is no live resource to hand back; warm reuse is the
// pool's concern (T6), and it is the pool — not the Runner — that will own any
// teardown that outlives a single Exec. Release stays idempotent and
// non-blocking so callers may release unconditionally on every early-return
// path without branching on which substrate they hold.
func (*containerRunner) Release(context.Context) error { return nil }

// runCommand is the default execRunner: a real subprocess.
//
// It maps os/exec's error model onto the execRunner contract — an ExitError is
// a RESULT (the CLI ran and reported a status), anything else is a start
// failure — and it never returns a zero exit code alongside a failure.
func runCommand(ctx context.Context, name string, args ...string) ([]byte, int, error) {
	c := exec.CommandContext(ctx, name, args...)

	// SECURITY-CRITICAL: a nil exec.Cmd.Env means "inherit the parent process
	// environment", so this is set explicitly and is never left nil.
	//
	// Note what this env is and is not. It is the CLIENT process's environment —
	// what the docker/nerdctl/podman binary itself needs to locate a daemon and
	// read its own config. It has NO bearing on what the sandboxed command
	// observes: the container's environment is carried entirely in argv as
	// --env K=V. Inheriting here would still hand this process's API keys to
	// the CLI binary, so it is scrubbed to the same discipline, with the small
	// set of daemon-addressing variables the CLIs genuinely require added.
	c.Env = renderEnv(ResolveEnvFromOS(containerClientPassthrough[:]))

	combined, err := c.CombinedOutput()
	if err == nil {
		return combined, 0, nil
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		code := exitErr.ExitCode()
		if code == 0 {
			// Signalled-but-zero cannot normally happen; refuse to report it as
			// success regardless.
			code = -1
		}
		return combined, code, nil
	}

	return combined, -1, err
}
