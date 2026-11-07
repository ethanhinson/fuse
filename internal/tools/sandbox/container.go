package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
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

// containerManagedLabel marks every container fuse starts, so an operator can list
// them (`docker ps --filter label=fuse.managed`) and tell them from their own.
const containerManagedLabel = "fuse.managed=true"

// containerKillTimeout bounds the `kill` sent after a timeout or cancel. A daemon
// that has stopped answering must not hold the tool call, or a signalled run's
// exit, for longer than this.
const containerKillTimeout = 5 * time.Second

// ErrNoContainerRuntime reports that none of the supported container CLIs is on
// PATH, so the container substrate cannot be constructed.
//
// This is returned rather than quietly degrading to the host handler. A silent
// fallback would mean that uninstalling docker turns containment off, which is
// exactly the fail-open direction ADR-0044 forbids. Selection (T5) decides what
// to do with this error; it is never this package's job to answer it by running
// the command somewhere less safe.
var ErrNoContainerRuntime = errors.New("no container runtime available")

// errPullFailed marks an Acquire that failed at the pre-pull. It is unexported
// and carries no caller-facing meaning: its only job is to let the pool tell a
// pull failure apart from every other cold-start failure, so ONE incident is
// reported under ONE reason (pull_failed, at the site that observed it) rather
// than being counted again as acquire_failed one frame up.
var errPullFailed = errors.New("sandbox: image pre-pull failed")

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

// withContainerEnvLookup overrides how the container CLI CLIENT's own
// passthrough variables (containerClientPassthrough) are resolved. It is the
// same seam Service.WithEnvLookup governs for the sandboxed command's
// environment, plumbed through so ONE lookup governs both halves rather than
// the client half silently re-reading the real process environment on every
// Exec.
func withContainerEnvLookup(fn func(string) (string, bool)) containerOption {
	return func(h *containerHandler) {
		if fn != nil {
			h.envLookup = fn
		}
	}
}

// withLimits sets the per-container cgroup caps this handler renders into argv
// (change 0077).
//
// SECURITY-CRITICAL: like withTrustedRoot, the caps come from the COMPOSITION
// ROOT — resolved once from trusted operator config at Service construction,
// before any model has run, with posture defaults already applied. They must
// never be derived from a tool argument, a wire field, working_dir, or model
// output: a cap a model could influence is a cap a model could raise, and a cap
// a model can raise is not a cap. Applied once, at construction; no method
// changes them afterwards.
func withLimits(l Limits) containerOption {
	return func(h *containerHandler) { h.limits = l }
}

// withEgress sets the resolved egress posture this handler renders into argv
// (change 0064).
//
// SECURITY-CRITICAL: like withLimits and withTrustedRoot, the posture comes from
// the COMPOSITION ROOT — resolved once from trusted operator config at Service
// construction, before any model has run — and it is applied LAST in the options
// chain (see NewService) so no caller-supplied containerOption can downgrade
// EgressEnforce back to EgressAllowAll. It must never be derived from a tool
// argument, a wire field, working_dir, or model output: a floor the model can
// select is not a floor. Applied once, at construction; no method changes it
// afterwards.
func withEgress(e Egress) containerOption {
	return func(h *containerHandler) { h.egress = e }
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

	// limits are the per-container cgroup caps (change 0077), resolved once at
	// construction from trusted operator config with posture defaults already
	// applied. Every field is optional: an unset field emits no flag. Never
	// model-derived; see withLimits.
	limits Limits

	// egress is the resolved egress posture (change 0064), settled once at
	// construction from trusted operator config. Its zero value is
	// EgressAllowAll, which renders no network flag at all — byte-for-byte the
	// pre-0064 argv. Never model-derived; see withEgress.
	egress Egress

	// pullOnce guards the single-flight pre-pull, and pullErr records its
	// outcome. A failed pull is retried on a later Acquire rather than cached as
	// a permanent failure, so pullOnce is reset on failure (see prePull).
	pullMu   sync.Mutex
	pulling  chan struct{} // non-nil while a pull is in flight; closed when it finishes
	pullDone bool          // true once a pull has SUCCEEDED; then never pulled again
	pullErr  error         // last pull's error, read under pullMu

	// envLookup resolves the CLI client's own passthrough variables
	// (containerClientPassthrough). Nil means the real process environment.
	// This governs the SAME seam as the sandboxed command's environment
	// (Service.lookup / WithEnvLookup) — see withContainerEnvLookup — so one
	// seam controls both halves of what "the environment is frozen" claims.
	envLookup func(string) (string, bool)

	// health is the substrate-health observer (change 0065, task 7). Installed
	// once at the composition root via Service.SetHealthHooks, before the
	// handler is used concurrently, and never written again — same discipline
	// as the gate's hooks. The zero value is inert.
	health HealthHooks

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
	// Applied only if no test injected its own execRunner (withExecRunner sets
	// h.run directly), so the client-env lookup wiring never overrides an
	// explicit test double.
	if h.run == nil {
		h.run = h.runClientCommand
	}

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

// setHealthHooks satisfies healthObserved. Written once at the composition root
// before concurrent use, never afterwards — the same discipline every other
// field on this handler follows.
func (h *containerHandler) setHealthHooks(hooks HealthHooks) { h.health = hooks }

// Acquire returns a Runner bound to p and to env.
//
// The environment is rendered here, once, from the allowlist the caller
// resolved. The Runner never consults the process environment afterwards, which
// is what makes it structurally impossible for an ambient variable to reach a
// container.
func (h *containerHandler) Acquire(ctx context.Context, p Principal, env Env) (Runner, error) {
	// Bounded, single-flight pre-pull (change 0077). With --pull=never on the run
	// argv, the image must be acquired here or `run` fails; doing it under the
	// pull's own timeout is what keeps a cold image from hanging an Exec on the
	// command's own deadline. A failed pull is an ACQUIRE failure — it is
	// reported so selection surfaces "pull_failed" — and is retried on a later
	// Acquire rather than cached as permanent.
	if err := h.prePull(ctx); err != nil {
		// The one place pull_failed can be observed honestly: the image
		// acquisition itself failed, so this substrate can start no container
		// for anyone until a later pull succeeds. The error is NOT carried into
		// the hook — only the closed reason is (see HealthInfo).
		//
		// A caller-deadline expiry is excluded: prePull returns ctx.Err() when
		// the CALLER ran out of time while the pull continues in the background,
		// and that is the caller's bound firing, not the substrate failing.
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			h.health.fire(HealthInfo{
				Principal: p,
				Handler:   HandlerContainer,
				Runtime:   h.runtime,
				Reason:    HealthPullFailed,
			})
		}
		return nil, fmt.Errorf("%s pull %s: %w: %w", h.runtime, h.image, errPullFailed, err)
	}

	return &containerRunner{
		handler:   h,
		principal: p,
		env:       renderEnv(env),
		root:      h.root,
	}, nil
}

// containerRunner is one principal's container execution context.
type containerRunner struct {
	handler *containerHandler
	// principal is fixed at Acquire and never reassigned: a Runner belongs to
	// exactly one principal for its whole life, which is what lets the warm
	// pool re-assert ownership on checkout (see acquiredFor).
	principal Principal

	// root is the HOST directory THIS Runner's containers bind-mount: the handler's
	// trusted root, fixed at Acquire. "" means mount nothing — the degraded-safe state — and is never a
	// substitute root; see workspace.
	root string

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
func (r *containerRunner) acquiredFor() Principal { return r.principal }

// mountRoot reports the HOST directory this Runner's containers bind-mount, so
// a pool can verify — against the Runner itself, not only against its own
// bookkeeping — that a warm checkout still mounts the tree it was certified
// with (change 0065; see pool.go's certifyEntry).
//
// It reads the immutable field set at Acquire and takes no lock, because there
// is nothing to lock: root is written once, before this Runner is visible to
// any other goroutine, and never reassigned. If that ever stops being true,
// this accessor is where the guard belongs.
func (r *containerRunner) mountRoot() string { return r.root }

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
// It is a one-line DELEGATION to resolveWorkspace (workspace.go), which carries
// the algorithm and the reasoning behind every line of it. The algorithm was
// promoted out of this method in change 0075 so that the remote substrate
// adapter resolves a model-supplied working_dir through the SAME implementation
// rather than a second copy of it — see resolveWorkspace's doc comment, which
// records why duplicating it is forbidden.
//
// The only thing this method adds is the container substrate's mount point:
// containerWorkspace, a constant, never anything derived from model output.
//
// A caller MUST still pass an already-canonicalised root. Both call paths do:
// h.root is canonicalised at construction, and the per-tenant root at Acquire,
// through that same one function (see resolveMountRoot).
func (h *containerHandler) workspace(root string, workingDir string) (mount string, workdir string, err error) {
	return resolveWorkspace(root, workingDir, containerWorkspace)
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
func (r *containerRunner) argv(name string, cmd string, workingDir string) ([]string, error) {
	// The Runner's own root, fixed at Acquire, is the ONLY root argv ever mounts.
	mount, workdir, err := r.handler.workspace(r.root, workingDir)
	if err != nil {
		return nil, err
	}

	env := r.currentEnv()

	args := make([]string, 0, 20+2*len(env))
	args = append(args,
		"run",
		// --rm: the container is torn down when the command exits, so no
		// state — including anything the command wrote outside the mount —
		// survives into the next principal's execution.
		"--rm",
		// -i: keeps stdin open. No -t: there is no terminal here, and
		// allocating one would corrupt the combined-output capture.
		"-i",
		// --name: the handle Exec kills by. Killing the `run` client on a timeout
		// does not stop the container; the daemon owns it and it keeps writing to
		// the mount, and without a name there is nothing to ask the daemon to stop.
		"--name", name,
		"--label", containerManagedLabel,
	)

	// Per-container cgroup caps (change 0077). Emitted ONLY for fields the
	// resolved config set; an unset field renders nothing — never a sentinel,
	// never `--memory 0` (which some runtimes read as "unlimited" and others
	// reject). Placement is deliberate: after `--rm -i` and BEFORE the egress
	// floor below (change 0064), which is where network posture lands.
	args = append(args, r.handler.limits.argv()...)

	// The egress FLOOR (change 0064). Under EgressEnforce the container gets
	// `--network none`: loopback only, no route off the box, so nothing the
	// command runs can reach the network. Under EgressAllowAll — the default, and the zero value
	// — nothing is emitted at all, so an operator who never configured egress
	// gets byte-for-byte the pre-0064 argv.
	//
	// Three things about this site a reader must not have to rediscover:
	//
	//   - It is decided by the MODE alone, never by the allowlist being
	//     non-empty. Enforcing with nothing declared is the deny-all state and
	//     still gets the floor; that is the fail-safe direction.
	//   - It is the PAIR form (`--network`, `none`), which docker, nerdctl and
	//     podman all accept — preserving the one-argv-builder-serves-all-three
	//     property.
	//   - It is emitted HERE, in the run builder, and nowhere else. The pre-pull
	//     (prePull/runPull) builds its own `pull <image>` argv straight to h.run
	//     and must keep doing so: a pull under --network none cannot reach a
	//     registry, and with --pull=never below, a floored pull would take the
	//     whole substrate down on every cold image.
	//
	if r.handler.egress.Mode == EgressEnforce {
		args = append(args, "--network", "none")

		// Under enforcement a proxy variable can only point somewhere the
		// container cannot reach, so an operator's env_passthrough naming one is
		// dropped rather than left to make clients fail confusingly.
		env = stripProxyEnv(env)
	}

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

	// The source is the trusted root and only ever the trusted root. mount is ""
	// only when no trusted root was resolved at all, in which case nothing is
	// mounted — never a substitute derived from the command's arguments.
	if mount != "" {
		args = append(args, "-v", mount+":"+containerWorkspace)
	}
	args = append(args, "-w", workdir)

	// --pull=never (change 0077): the image is acquired by the explicit,
	// separately-timed pre-pull (see prePull), so `run` must never trigger an
	// unbounded pull under the command's own deadline. Supported by all three
	// detected CLIs (docker, nerdctl, podman), preserving the
	// one-argv-builder-serves-all-three property.
	args = append(args, "--pull=never")

	args = append(args, r.handler.image)
	args = append(args, "/bin/sh", "-c", cmd)
	return args, nil
}

// argv renders the per-container cgroup caps as OCI run flags, in a fixed order
// (change 0077). Only set fields render; an unset field emits nothing.
//
// Three details a reader must not have to rediscover:
//
//   - --memory-swap is pinned EQUAL to --memory. Docker's default when --memory
//     is set and --memory-swap is not is TWICE the memory limit, so a lone
//     --memory 2g actually permits 4 GB of memory+swap. Pinning them equal is
//     what makes the number mean what it says.
//   - --ulimit fsize bounds a SINGLE FILE, not the mount. It is real protection
//     against `dd if=/dev/zero of=big`; it is not a disk quota (that is #0065).
//   - --cpus is rendered from the config's already-canonicalised decimal string,
//     never a float formatted by %v — argv is golden-tested, so rendering must
//     be deterministic and locale-independent.
func (l Limits) argv() []string {
	var a []string
	if l.MemoryBytes != nil {
		m := strconv.FormatInt(*l.MemoryBytes, 10)
		// Pinned equal — see the doc comment.
		a = append(a, "--memory", m, "--memory-swap", m)
	}
	if l.CPUs != nil {
		a = append(a, "--cpus", *l.CPUs)
	}
	if l.Pids != nil {
		a = append(a, "--pids-limit", strconv.FormatInt(*l.Pids, 10))
	}
	if l.NoFile != nil {
		n := strconv.FormatInt(*l.NoFile, 10)
		a = append(a, "--ulimit", "nofile="+n+":"+n)
	}
	if l.FsizeBytes != nil {
		a = append(a, "--ulimit", "fsize="+strconv.FormatInt(*l.FsizeBytes, 10))
	}
	return a
}

// prePull performs the bounded, single-flight image acquisition (change 0077).
//
// The pull runs under a context derived from context.Background() and the
// configured pull_timeout — deliberately NOT from the caller's context, so a
// short-timeout bash call cannot cancel a pull a later call would have benefited
// from. The caller waits on the shared in-flight pull only up to ITS OWN
// deadline: if the caller's ctx fires first it returns that error while the pull
// completes in the background and the next Acquire finds the image warm.
//
// Concurrent callers JOIN the in-flight pull rather than each starting one. A
// SUCCEEDED pull is remembered and never repeated; a FAILED pull is not cached —
// it is retried on a later Acquire, because a transient registry blip must not
// permanently break the substrate.
func (h *containerHandler) prePull(ctx context.Context) error {
	h.pullMu.Lock()
	if h.pullDone {
		h.pullMu.Unlock()
		return nil
	}
	wait := h.pulling
	if wait == nil {
		// We start the pull. A fresh channel marks it in-flight; a background
		// goroutine runs it under the pull_timeout, independent of any caller.
		wait = make(chan struct{})
		h.pulling = wait
		timeout := DefaultPullTimeout
		if h.limits.PullTimeout != nil {
			timeout = *h.limits.PullTimeout
		}
		go h.runPull(wait, timeout)
	}
	h.pullMu.Unlock()

	// Wait on the shared pull only up to the CALLER's own deadline.
	select {
	case <-wait:
		h.pullMu.Lock()
		err := h.pullErr
		h.pullMu.Unlock()
		return err
	case <-ctx.Done():
		// The caller ran out of time; the pull continues in the background.
		return ctx.Err()
	}
}

// runPull executes the actual pull under its own timeout and records the
// outcome, then closes done to release every joined caller. On failure it clears
// the in-flight marker so a later Acquire retries; on success it latches
// pullDone so the image is never pulled again.
func (h *containerHandler) runPull(done chan struct{}, timeout time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	_, _, err := h.run(ctx, h.runtime, "pull", h.image)

	h.pullMu.Lock()
	h.pullErr = err
	if err == nil {
		h.pullDone = true
	}
	h.pulling = nil // allow a retry on failure; harmless on success (pullDone gates)
	h.pullMu.Unlock()

	close(done)
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
	name, err := newContainerName()
	if err != nil {
		return Output{ExitCode: -1}, err
	}
	args, err := r.argv(name, cmd, workingDir)
	if err != nil {
		// A containment refusal, decided BEFORE the CLI is touched: no
		// container is created, no mount is established, nothing runs. It is
		// reported as a substrate failure (ExitCode -1, non-nil error) because
		// that is exactly what it is — the command could not be started — and
		// because an ExitCode-only caller must fail closed on it.
		return Output{ExitCode: -1}, err
	}

	combined, code, err := r.handler.run(ctx, r.handler.runtime, args...)

	// The context killed the client, not the container. This runs before Exec
	// returns, so bash's teardown drain (see CloseSandbox) keeps a signalled process
	// alive until it has happened.
	if ctx.Err() != nil {
		combined = append(combined, r.handler.killContainer(ctx, name)...)
	}

	out := Output{
		Combined: combined,
		ExitCode: code,
		// Checked on the caller's context: the CLI is killed when the deadline
		// fires, and the resulting status is indistinguishable from any other
		// signal death without this.
		TimedOut: errors.Is(ctx.Err(), context.DeadlineExceeded),
	}

	// Substrate-health classification (change 0065, task 7), decided BEFORE the
	// error branch below rewrites ExitCode to -1 — classifyExit needs the raw
	// status the runtime actually reported. It fires for a signal death or a
	// failure to start, and for NOTHING else: an ordinary non-zero command exit
	// is the command reporting a result, not the sandbox being unhealthy.
	if reason, unhealthy := classifyExit(code, err, out.TimedOut); unhealthy {
		r.handler.health.fire(HealthInfo{
			Principal: r.principal,
			Handler:   HandlerContainer,
			Runtime:   r.handler.runtime,
			Reason:    reason,
		})
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

// newContainerName returns a name no other run on the daemon is using. The daemon
// is shared across every fuse process and every checkout, so nothing derived from
// this process would do, and the failure mode of a collision is killing someone
// else's command.
func newContainerName() (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("container name: %w", err)
	}
	return "fuse-" + hex.EncodeToString(b), nil
}

// killContainer stops a container whose client was killed. It returns a note for
// the model when the container may still be running: the command is reported as
// timed out or cancelled, and the model must not take that to mean it has stopped
// writing to the repo.
//
// A container that has already gone is the expected race with --rm, not a failure.
//
// A client killed in the instant before the daemon registers the name can still
// lose this race the other way. That container is reported here as already gone.
func (h *containerHandler) killContainer(ctx context.Context, name string) []byte {
	// The caller's context is what just fired.
	killCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), containerKillTimeout)
	defer cancel()
	out, code, err := h.run(killCtx, h.runtime, "kill", name)
	if err == nil && code == 0 {
		return nil
	}
	lower := strings.ToLower(string(out))
	for _, gone := range [...]string{"no such", "not running", "no container"} {
		if err == nil && strings.Contains(lower, gone) {
			return nil
		}
	}
	reason := strings.TrimSpace(string(out))
	if err != nil {
		reason = err.Error()
	}
	return fmt.Appendf(nil, "\n[sandbox: could not confirm container %s stopped; it may still be running and writing to the workspace: %s]\n", name, reason)
}

// Release is a no-op: each Exec's container is `--rm`, so there is nothing held
// between commands.
func (r *containerRunner) Release(context.Context) error { return nil }

// runClientCommand is the default execRunner, bound to h so that the CLI
// client's own environment is resolved through the SAME lookup seam
// (h.envLookup) that governs the sandboxed command's environment, rather than
// re-reading the real process environment on every Exec regardless of what
// WithEnvLookup / withContainerEnvLookup declared.
func (h *containerHandler) runClientCommand(ctx context.Context, name string, args ...string) ([]byte, int, error) {
	return runCommand(ctx, name, h.envLookup, args...)
}

// runCommand is the default execRunner implementation: a real subprocess.
//
// It maps os/exec's error model onto the execRunner contract — an ExitError is
// a RESULT (the CLI ran and reported a status), anything else is a start
// failure — and it never returns a zero exit code alongside a failure.
func runCommand(ctx context.Context, name string, lookup func(string) (string, bool), args ...string) ([]byte, int, error) {
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
	//
	// lookup is the SAME seam (Service.lookup / WithEnvLookup) that governs the
	// sandboxed command's environment; nil means the real process environment.
	if lookup != nil {
		c.Env = renderEnv(ResolveEnv(containerClientPassthrough[:], lookup))
	} else {
		c.Env = renderEnv(ResolveEnvFromOS(containerClientPassthrough[:]))
	}

	// Bounded like the host path: a noisy container is just as able to exhaust this
	// process through the client's output.
	captured := newBoundedOutput()
	c.Stdout, c.Stderr = captured, captured
	// A killed client must not keep this call waiting on an inherited pipe.
	c.WaitDelay = pipeWaitDelay
	err := c.Run()
	combined := captured.Bytes()
	if err == nil || errors.Is(err, exec.ErrWaitDelay) {
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

// stripProxyEnv removes every proxy-controlling key from an already-rendered
// `K=V` environment, returning a fresh slice.
//
// This runs under EgressEnforce whether or not a datapath was wired, because the
// two reasons are independent: with a datapath, a passthrough value must not
// compete with the injected one; without a datapath, an inherited HTTP_PROXY
// naming a host-side proxy is a description of an escape route that no longer
// exists, and handing it to the command only produces confusing failures.
//
// It is a STRIP and not a re-ordering. `--env K=V1 --env K=V2` resolves last-wins
// on docker, but this builder serves nerdctl and podman from the same argv, and
// making a trust boundary depend on how three runtimes each resolve a duplicate
// key is not a boundary. After this, no duplicate exists for them to resolve.
func stripProxyEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		key, _, _ := strings.Cut(kv, "=")
		if isProxyEnvKey(key) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func isProxyEnvKey(key string) bool {
	for _, k := range proxyEnvKeys {
		if key == k {
			return true
		}
	}
	return false
}

// proxyEnvKeys are every variable that decides whether — and through what — a
// process in the container uses a proxy. All eight are trusted-side territory
// under enforcement: they are stripped from the resolved passthrough environment
// and re-injected from egressProxyEnv, so the operator's env_passthrough list can
// neither redirect egress at a host of its choosing (HTTP_PROXY) nor exempt
// destinations from it (NO_PROXY).
//
// Both cases are the same defect wearing different clothes, which is why the list
// is one list: a variable this package injects is a variable this package owns.
var proxyEnvKeys = [...]string{
	"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY",
	"http_proxy", "https_proxy", "all_proxy", "no_proxy",
}
